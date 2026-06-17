// clip — a tiny self-hosted shared clipboard for your homelab.
// Open the page on any device, drop text/images/files, and they show up
// instantly on every other device to copy or download.
//
// Pure standard library, no external dependencies, single static binary.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static
var staticFS embed.FS

// ---------- configuration ----------

type config struct {
	port       string
	dataDir    string
	pin        string
	maxUpload  int64 // bytes
	maxText    int64 // bytes
	retention  time.Duration
	maxItems   int
	trustProxy bool
	allowNoPin bool
}

const placeholderPIN = "troque-este-pin"

func envStr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func loadConfig() config {
	c := config{
		port:       envStr("PORT", "8080"),
		dataDir:    envStr("CLIP_DATA", "./data"),
		pin:        envStr("CLIP_PIN", ""),
		maxUpload:  int64(envInt("CLIP_MAX_UPLOAD_MB", 64)) << 20,
		maxText:    int64(envInt("CLIP_MAX_TEXT_KB", 1024)) << 10,
		maxItems:   envInt("CLIP_MAX_ITEMS", 300),
		trustProxy: envBool("CLIP_TRUST_PROXY"),
		allowNoPin: envBool("CLIP_ALLOW_NO_PIN"),
	}
	if c.maxUpload < 1<<20 { // at least 1 MiB
		c.maxUpload = 1 << 20
	}
	if c.maxText < 1<<10 { // at least 1 KiB
		c.maxText = 1 << 10
	}
	days := envInt("CLIP_RETENTION_DAYS", 14)
	if days > 0 {
		c.retention = time.Duration(days) * 24 * time.Hour
	}
	return c
}

// ---------- data model ----------

type Item struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // "text" | "file"
	Text    string `json:"text,omitempty"`
	Name    string `json:"name,omitempty"`
	Mime    string `json:"mime,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Created int64  `json:"created"` // unix milliseconds
}

type store struct {
	mu      sync.Mutex
	cfg     config
	items   []*Item // newest first
	hub     *hub
}

func nowMS() int64 { return time.Now().UnixMilli() }

var idRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should never fail; derive a valid 32-hex id as a last resort
		// so the result still matches idRe and routes correctly.
		t := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(t>>(uint(i%8)*8)) ^ byte(i*31)
		}
	}
	return hex.EncodeToString(b)
}

func (s *store) blobsDir() string { return filepath.Join(s.cfg.dataDir, "blobs") }
func (s *store) blobPath(id string) string { return filepath.Join(s.blobsDir(), id) }
func (s *store) indexPath() string { return filepath.Join(s.cfg.dataDir, "index.json") }

func (s *store) load() error {
	if err := os.MkdirAll(s.blobsDir(), 0o750); err != nil {
		return err
	}
	// Clean up temp files left behind by interrupted uploads.
	if entries, err := os.ReadDir(s.blobsDir()); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				_ = os.Remove(filepath.Join(s.blobsDir(), e.Name()))
			}
		}
	}
	data, err := os.ReadFile(s.indexPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var items []*Item
	if err := json.Unmarshal(data, &items); err != nil {
		log.Printf("warning: could not parse index, starting empty: %v", err)
		return nil
	}
	// Drop entries whose backing blob has gone missing.
	kept := items[:0]
	for _, it := range items {
		if it.Kind == "file" {
			if _, err := os.Stat(s.blobPath(it.ID)); err != nil {
				continue
			}
		}
		kept = append(kept, it)
	}
	s.mu.Lock()
	s.items = kept
	s.sortAndPruneLocked()
	s.mu.Unlock()
	return nil
}

// saveLocked persists the index atomically. Caller must hold s.mu.
func (s *store) saveLocked() {
	data, err := json.Marshal(s.items)
	if err != nil {
		log.Printf("error marshaling index: %v", err)
		return
	}
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		log.Printf("error writing index: %v", err)
		return
	}
	if err := os.Rename(tmp, s.indexPath()); err != nil {
		log.Printf("error renaming index: %v", err)
	}
}

// sortAndPruneLocked sorts newest-first and removes items past retention or
// over the count cap, deleting their blobs. Caller must hold s.mu.
func (s *store) sortAndPruneLocked() {
	sort.SliceStable(s.items, func(i, j int) bool {
		return s.items[i].Created > s.items[j].Created
	})
	var removed []*Item
	if s.cfg.retention > 0 {
		cutoff := nowMS() - s.cfg.retention.Milliseconds()
		kept := s.items[:0]
		for _, it := range s.items {
			if it.Created < cutoff {
				removed = append(removed, it)
				continue
			}
			kept = append(kept, it)
		}
		s.items = kept
	}
	if s.cfg.maxItems > 0 && len(s.items) > s.cfg.maxItems {
		removed = append(removed, s.items[s.cfg.maxItems:]...)
		s.items = s.items[:s.cfg.maxItems]
	}
	for _, it := range removed {
		if it.Kind == "file" {
			_ = os.Remove(s.blobPath(it.ID))
		}
	}
}

func (s *store) add(it *Item) {
	s.mu.Lock()
	s.items = append(s.items, it)
	s.sortAndPruneLocked()
	s.saveLocked()
	s.mu.Unlock()
	s.hub.broadcast()
}

func (s *store) list() []*Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Item, len(s.items))
	copy(out, s.items)
	return out
}

func (s *store) get(id string) (*Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		if it.ID == id {
			return it, true
		}
	}
	return nil, false
}

func (s *store) delete(id string) bool {
	s.mu.Lock()
	idx := -1
	for i, it := range s.items {
		if it.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return false
	}
	it := s.items[idx]
	s.items = append(s.items[:idx], s.items[idx+1:]...)
	if it.Kind == "file" {
		_ = os.Remove(s.blobPath(id))
	}
	s.saveLocked()
	s.mu.Unlock()
	s.hub.broadcast()
	return true
}

func (s *store) clear() {
	s.mu.Lock()
	for _, it := range s.items {
		if it.Kind == "file" {
			_ = os.Remove(s.blobPath(it.ID))
		}
	}
	s.items = nil
	s.saveLocked()
	s.mu.Unlock()
	s.hub.broadcast()
}

func (s *store) pruneLoop() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		s.sortAndPruneLocked()
		s.saveLocked()
		s.mu.Unlock()
		s.hub.broadcast()
	}
}

// ---------- SSE hub (live updates) ----------

type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

const maxSubscribers = 200

func newHub() *hub { return &hub{subs: make(map[chan struct{}]struct{})} }

func (h *hub) subscribe() (chan struct{}, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= maxSubscribers {
		return nil, false
	}
	ch := make(chan struct{}, 1)
	h.subs[ch] = struct{}{}
	return ch, true
}

func (h *hub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *hub) broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default: // a refresh is already pending for this subscriber
		}
	}
}

// ---------- auth ----------

type auth struct {
	pin     string
	token   string // expected cookie value; empty when no pin configured
	limiter *loginLimiter
}

const cookieName = "clip_session"

func newAuth(cfg config) (*auth, error) {
	a := &auth{pin: cfg.pin, limiter: newLoginLimiter()}
	if cfg.pin == "" {
		return a, nil
	}
	secret, err := loadOrCreateSecret(filepath.Join(cfg.dataDir, ".secret"))
	if err != nil {
		return nil, err
	}
	// Bind the token to the PIN so that changing CLIP_PIN invalidates all
	// existing sessions (the cookie value depends on both secret and PIN).
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("clip-auth-v1|" + cfg.pin))
	a.token = hex.EncodeToString(mac.Sum(nil))
	return a, nil
}

func loadOrCreateSecret(path string) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil && len(data) >= 32 {
		return data, nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		return nil, err
	}
	return secret, nil
}

func (a *auth) enabled() bool { return a.pin != "" }

func (a *auth) authed(r *http.Request) bool {
	if !a.enabled() {
		return true
	}
	if a.token == "" {
		return false
	}
	match := func(s string) bool {
		return s != "" && subtle.ConstantTimeCompare([]byte(s), []byte(a.token)) == 1
	}
	// 1) Authorization: Bearer <token> (default for fetch requests)
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if match(strings.TrimPrefix(h, "Bearer ")) {
			return true
		}
	}
	// 2) X-Clip-Token header
	if match(r.Header.Get("X-Clip-Token")) {
		return true
	}
	// 3) ?t= query param — for EventSource and <img>/<a> that can't send headers
	if match(r.URL.Query().Get("t")) {
		return true
	}
	// 4) cookie — used where the browser allows cookies
	if c, err := r.Cookie(cookieName); err == nil && match(c.Value) {
		return true
	}
	return false
}

// login limiter: simple per-IP failed-attempt throttle.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*attemptRec
}

type attemptRec struct {
	count   int
	blocked time.Time
	seen    time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: make(map[string]*attemptRec)}
}

const (
	maxLoginFails = 5
	loginBlockFor = 5 * time.Minute
)

func (l *loginLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.attempts[ip]
	return r != nil && time.Now().Before(r.blocked)
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.attempts[ip]
	if r == nil {
		r = &attemptRec{}
		l.attempts[ip] = r
	}
	r.count++
	r.seen = time.Now()
	if r.count >= maxLoginFails {
		r.blocked = time.Now().Add(loginBlockFor)
		r.count = 0
	}
}

func (l *loginLimiter) success(ip string) {
	l.mu.Lock()
	delete(l.attempts, ip)
	l.mu.Unlock()
}

// sweep drops stale records so the map can't grow without bound.
func (l *loginLimiter) sweep() {
	l.mu.Lock()
	now := time.Now()
	for ip, r := range l.attempts {
		if now.After(r.blocked) && now.Sub(r.seen) > time.Hour {
			delete(l.attempts, ip)
		}
	}
	l.mu.Unlock()
}

// clientIP returns the caller's IP. X-Forwarded-For is honored only when
// CLIP_TRUST_PROXY is set, because otherwise any client could spoof it to
// defeat the login rate limiter and grow its bookkeeping map without bound.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// ---------- server ----------

type server struct {
	cfg  config
	st   *store
	auth *auth
}

func main() {
	cfg := loadConfig()
	if cfg.pin == placeholderPIN {
		log.Fatalf("CLIP_PIN ainda está no valor de exemplo %q. Defina um PIN forte e único antes de subir.", placeholderPIN)
	}
	if cfg.pin == "" && !cfg.allowNoPin {
		log.Fatalf("CLIP_PIN está vazio. Defina um PIN (recomendado). Para rodar SEM autenticação numa rede confiável, suba com CLIP_ALLOW_NO_PIN=1.")
	}
	if err := os.MkdirAll(cfg.dataDir, 0o750); err != nil {
		log.Fatalf("cannot create data dir %s: %v", cfg.dataDir, err)
	}
	h := newHub()
	st := &store{cfg: cfg, hub: h}
	if err := st.load(); err != nil {
		log.Fatalf("cannot load store: %v", err)
	}
	a, err := newAuth(cfg)
	if err != nil {
		log.Fatalf("cannot init auth: %v", err)
	}
	if !a.enabled() {
		log.Printf("WARNING: rodando SEM PIN (CLIP_ALLOW_NO_PIN) — qualquer um que alcançar a porta tem acesso total. NÃO exponha à internet assim.")
	}
	go st.pruneLoop()
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for range t.C {
			a.limiter.sweep()
		}
	}()

	srv := &server{cfg: cfg, st: st, auth: a}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/api/me", srv.handleMe)
	mux.HandleFunc("/api/login", srv.handleLogin)
	mux.HandleFunc("/api/logout", srv.handleLogout)
	mux.HandleFunc("/api/items", srv.handleItems)
	mux.HandleFunc("/api/upload", srv.handleUpload)
	mux.HandleFunc("/api/item/", srv.handleItemDelete)
	mux.HandleFunc("/api/clear", srv.handleClear)
	mux.HandleFunc("/api/events", srv.handleEvents)
	mux.HandleFunc("/b/", srv.handleBlob)
	mux.Handle("/static/", noStore(http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("/manifest.webmanifest", srv.serveAsset("static/manifest.webmanifest", "application/manifest+json"))
	mux.HandleFunc("/sw.js", srv.serveServiceWorker)
	mux.HandleFunc("/", srv.handleRoot)

	handler := securityHeaders(mux)
	addr := ":" + cfg.port
	log.Printf("clip listening on %s (data=%s, auth=%v)", addr, cfg.dataDir, a.enabled())
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}
	log.Fatal(httpSrv.ListenAndServe())
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; "+
				"style-src 'self'; script-src 'self'; connect-src 'self'; "+
				"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// noStore wraps a handler so the app shell is never cached by the browser.
// The shell is tiny and embedded, so always fetching the latest avoids stale
// JS/HTML getting stuck after an update.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.auth.authed(r) {
		return true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "auth required", "needPin": true})
	return false
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"auth":    s.auth.authed(r),
		"needPin": s.auth.enabled(),
	})
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.auth.enabled() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	ip := clientIP(r, s.cfg.trustProxy)
	if s.auth.limiter.blocked(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many attempts, wait a few minutes"})
		return
	}
	var body struct {
		Pin string `json:"pin"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Pin), []byte(s.auth.pin)) != 1 {
		s.auth.limiter.fail(ip)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "wrong PIN"})
		return
	}
	s.auth.limiter.success(ip)
	// Cookie for browsers that allow it; the token in the body is the primary
	// mechanism (stored in localStorage, sent as an Authorization header) so the
	// app works even where cookies are blocked.
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    s.auth.token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24 * 365,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": s.auth.token})
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleItems(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.st.list())
	case http.MethodPost:
		var body struct {
			Text string `json:"text"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.maxText+4096)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request or text too large"})
			return
		}
		text := strings.TrimRight(body.Text, "\n")
		if strings.TrimSpace(text) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty text"})
			return
		}
		if int64(len(text)) > s.cfg.maxText {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "text too large"})
			return
		}
		it := &Item{ID: newID(), Kind: "text", Text: text, Size: int64(len(text)), Created: nowMS()}
		s.st.add(it)
		writeJSON(w, http.StatusOK, it)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.maxUpload+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "expected multipart form"})
		return
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad upload"})
			return
		}
		if part.FormName() != "file" {
			part.Close()
			continue
		}
		origName := sanitizeName(part.FileName())
		id := newID()
		tmp := s.st.blobPath(id) + ".tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			part.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "cannot store file"})
			return
		}
		limited := io.LimitReader(part, s.cfg.maxUpload+1)
		n, copyErr := io.Copy(f, limited)
		f.Close()
		part.Close()
		if copyErr != nil {
			_ = os.Remove(tmp)
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "upload failed (too large?)"})
			return
		}
		if n == 0 {
			_ = os.Remove(tmp)
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty file"})
			return
		}
		if n > s.cfg.maxUpload {
			_ = os.Remove(tmp)
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "file too large"})
			return
		}
		if err := os.Rename(tmp, s.st.blobPath(id)); err != nil {
			_ = os.Remove(tmp)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "cannot store file"})
			return
		}
		mimeType := detectMime(s.st.blobPath(id), origName)
		if origName == "" {
			origName = "arquivo-" + id[:8] + extFor(mimeType)
		}
		it := &Item{ID: id, Kind: "file", Name: origName, Mime: mimeType, Size: n, Created: nowMS()}
		s.st.add(it)
		writeJSON(w, http.StatusOK, it)
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no file part"})
}

func (s *server) handleItemDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/item/")
	if !idRe.MatchString(id) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !s.st.delete(id) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleClear(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.st.clear()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, ok := s.st.hub.subscribe()
	if !ok {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	defer s.st.hub.unsubscribe(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			if _, err := io.WriteString(w, "event: update\ndata: 1\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *server) handleBlob(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/b/")
	if !idRe.MatchString(id) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	it, ok := s.st.get(id)
	if !ok || it.Kind != "file" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	f, err := os.Open(s.st.blobPath(id))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	disposition := "attachment"
	contentType := "application/octet-stream"
	if inlineMime(it.Mime) {
		contentType = it.Mime
		disposition = "inline"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", contentDisposition(disposition, it.Name))
	// http.ServeContent sets Content-Length and handles Range/If-Modified-Since.
	fi, statErr := f.Stat()
	modTime := time.UnixMilli(it.Created)
	if statErr == nil {
		modTime = fi.ModTime()
	}
	http.ServeContent(w, r, "", modTime, f)
}

func (s *server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *server) serveAsset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := staticFS.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", contentType)
		w.Write(data)
	}
}

func (s *server) serveServiceWorker(w http.ResponseWriter, r *http.Request) {
	data, err := staticFS.ReadFile("static/sw.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Service-Worker-Allowed", "/")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

// ---------- helpers ----------

var unsafeName = regexp.MustCompile(`[\x00-\x1f\x7f/\\]`)

func sanitizeName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = unsafeName.ReplaceAllString(name, "_")
	if name == "." || name == ".." {
		name = ""
	}
	if len(name) > 200 {
		name = name[len(name)-200:]
	}
	return name
}

func detectMime(path, name string) string {
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		buf := make([]byte, 512)
		n, _ := f.Read(buf)
		ct := http.DetectContentType(buf[:n])
		if ct != "application/octet-stream" {
			return ct
		}
	}
	// fall back to extension-based guess for types DetectContentType misses
	switch strings.ToLower(filepath.Ext(name)) {
	case ".pdf":
		return "application/pdf"
	case ".txt", ".log", ".md":
		return "text/plain; charset=utf-8"
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".webp":
		return "image/webp"
	case ".mp4":
		return "video/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".zip":
		return "application/zip"
	}
	return "application/octet-stream"
}

func extFor(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "image/png"):
		return ".png"
	case strings.HasPrefix(mimeType, "image/jpeg"):
		return ".jpg"
	case strings.HasPrefix(mimeType, "image/gif"):
		return ".gif"
	case strings.HasPrefix(mimeType, "image/webp"):
		return ".webp"
	case strings.HasPrefix(mimeType, "text/plain"):
		return ".txt"
	}
	return ""
}

// inlineMime reports whether a stored file is safe to serve inline for preview.
// Anything not on this allowlist is forced to download to avoid HTML/SVG XSS.
func inlineMime(m string) bool {
	base := m
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = base[:i]
	}
	switch strings.TrimSpace(base) {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp",
		"video/mp4", "video/webm", "audio/mpeg", "audio/ogg", "audio/wav",
		"text/plain":
		return true
	}
	return false
}

func contentDisposition(disp, name string) string {
	if name == "" {
		name = "arquivo"
	}
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r > 0x7e {
			return '_'
		}
		return r
	}, name)
	return disp + `; filename="` + ascii + `"; filename*=UTF-8''` + url.PathEscape(name)
}
