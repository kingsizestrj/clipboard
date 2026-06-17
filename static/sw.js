// Minimal service worker: caches the app shell so the PWA opens offline.
// Never caches API responses or blobs — those must always be fresh.
const CACHE = "clip-v1";
const SHELL = [
  "/",
  "/static/app.js",
  "/static/style.css",
  "/static/icon.svg",
  "/manifest.webmanifest",
];

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k)))
    ).then(() => self.clients.claim())
  );
});

self.addEventListener("fetch", (e) => {
  const req = e.request;
  if (req.method !== "GET") return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;
  // Never cache dynamic endpoints.
  if (url.pathname.startsWith("/api/") || url.pathname.startsWith("/b/") || url.pathname === "/sw.js") return;

  // App shell: network-first, fall back to cache when offline.
  e.respondWith(
    fetch(req)
      .then((res) => {
        if (res && res.ok) {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {});
        }
        return res;
      })
      .catch(() => caches.match(req).then((hit) => hit || caches.match("/")))
  );
});
