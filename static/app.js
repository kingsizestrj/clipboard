"use strict";

// Bump this when changing auth/caching so we can confirm in DevTools which
// build the browser actually loaded.
console.info("clip ui build: token-auth-2 (no-cookie + no-store + self-healing sw)");

const $ = (id) => document.getElementById(id);
const listEl = $("list");
const emptyEl = $("empty");
const statusDot = $("status-dot");
let evtSource = null;
let reconnectTimer = null;

// Session token sent via Authorization header, persisted in localStorage when
// allowed. This avoids depending on cookies, which some browsers / privacy
// settings (e.g. Firefox blocking cookies) refuse to store.
let token = "";
try { token = localStorage.getItem("clip_token") || ""; } catch (e) { token = ""; }

function saveToken(t) {
  token = t || "";
  try {
    if (token) localStorage.setItem("clip_token", token);
    else localStorage.removeItem("clip_token");
  } catch (e) { /* storage blocked — keep token in memory for this session */ }
}

function authHeaders(extra) {
  const h = Object.assign({}, extra || {});
  if (token) h["Authorization"] = "Bearer " + token;
  return h;
}

// ?t=<token> for requests that can't send a header (EventSource, <img>, <a download>).
function tokenParam() {
  return token ? "?t=" + encodeURIComponent(token) : "";
}

function onAuthLost() {
  saveToken("");
  showPin();
}

// ---------- helpers ----------

function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toast._t);
  toast._t = setTimeout(() => { t.hidden = true; }, 1800);
}

function fmtSize(n) {
  if (!n) return "";
  const u = ["B", "KB", "MB", "GB"];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + " " + u[i];
}

function fmtTime(ms) {
  const d = new Date(ms);
  const diff = (Date.now() - ms) / 1000;
  if (diff < 60) return "agora";
  if (diff < 3600) return Math.floor(diff / 60) + " min atrás";
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const hm = d.toLocaleTimeString("pt-BR", { hour: "2-digit", minute: "2-digit" });
  if (d.getTime() >= today.getTime()) return "hoje " + hm;
  return d.toLocaleDateString("pt-BR", { day: "2-digit", month: "2-digit" }) + " " + hm;
}

async function api(path, opts) {
  opts = opts || {};
  const res = await fetch(path, Object.assign({ credentials: "same-origin" }, opts, {
    headers: authHeaders(opts.headers),
  }));
  if (res.status === 401) { onAuthLost(); throw new Error("auth"); }
  return res;
}

// ---------- rendering ----------

function makeButton(label, cls, onClick) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = cls;
  b.textContent = label;
  b.addEventListener("click", onClick);
  return b;
}

function renderItem(it) {
  const el = document.createElement("article");
  el.className = "item";

  const head = document.createElement("div");
  head.className = "item-head";
  const meta = document.createElement("div");
  meta.className = "item-meta";
  const icon = document.createElement("span");
  icon.textContent = it.kind === "text" ? "📝"
    : (it.mime || "").startsWith("image/") ? "🖼️"
    : (it.mime || "").startsWith("video/") ? "🎬"
    : (it.mime || "").startsWith("audio/") ? "🎵" : "📎";
  meta.appendChild(icon);
  if (it.kind === "file") {
    const name = document.createElement("span");
    name.className = "name";
    name.textContent = it.name || "arquivo";
    meta.appendChild(name);
  }
  const info = document.createElement("span");
  info.textContent = [fmtSize(it.size), fmtTime(it.created)].filter(Boolean).join(" · ");
  meta.appendChild(info);
  head.appendChild(meta);

  const del = makeButton("🗑", "icon-btn", async () => {
    try {
      await api("/api/item/" + encodeURIComponent(it.id), { method: "DELETE" });
    } catch (e) { /* ignore */ }
  });
  del.title = "Apagar";
  head.appendChild(del);
  el.appendChild(head);

  const actions = document.createElement("div");
  actions.className = "item-actions";

  if (it.kind === "text") {
    const pre = document.createElement("pre");
    pre.className = "item-text";
    pre.textContent = it.text;
    el.appendChild(pre);
    actions.appendChild(makeButton("Copiar", "primary", () => copyText(it.text, pre)));
  } else {
    const url = "/b/" + encodeURIComponent(it.id) + tokenParam();
    const mime = it.mime || "";
    if (mime.startsWith("image/")) {
      const img = document.createElement("img");
      img.className = "preview";
      img.loading = "lazy";
      img.src = url;
      img.alt = it.name || "imagem";
      el.appendChild(img);
      actions.appendChild(makeButton("Copiar imagem", "primary", () => copyImage(url)));
    } else if (mime.startsWith("video/")) {
      const v = document.createElement("video");
      v.className = "preview";
      v.controls = true;
      v.src = url;
      el.appendChild(v);
    } else if (mime.startsWith("audio/")) {
      const a = document.createElement("audio");
      a.controls = true;
      a.src = url;
      el.appendChild(a);
    }
    const dl = document.createElement("a");
    dl.href = url;
    dl.download = it.name || "arquivo";
    dl.textContent = "Baixar";
    dl.className = "primary";
    actions.appendChild(dl);
  }

  el.appendChild(actions);
  return el;
}

function selectText(node) {
  const range = document.createRange();
  range.selectNodeContents(node);
  const sel = window.getSelection();
  sel.removeAllRanges();
  sel.addRange(range);
}

// Copy text. The async Clipboard API only exists in secure contexts (HTTPS or
// localhost); over plain HTTP on the LAN it is undefined, so fall back to a
// selection + execCommand("copy"), which still works in insecure contexts.
async function copyText(text, node) {
  if (navigator.clipboard && window.isSecureContext) {
    try { await navigator.clipboard.writeText(text); toast("Copiado"); return; }
    catch (e) { /* fall through to legacy path */ }
  }
  if (node) selectText(node);
  try {
    if (document.execCommand && document.execCommand("copy")) { toast("Copiado"); return; }
  } catch (e) { /* ignore */ }
  toast("Selecionado — copie manualmente");
}

async function copyImage(url) {
  try {
    const blob = await (await fetch(url, { credentials: "same-origin" })).blob();
    if (navigator.clipboard && window.ClipboardItem && window.isSecureContext) {
      await navigator.clipboard.write([new ClipboardItem({ [blob.type]: blob })]);
      toast("Imagem copiada");
      return;
    }
    throw new Error("no clipboard");
  } catch (e) {
    window.open(url, "_blank");
  }
}

function renderList(items) {
  listEl.replaceChildren();
  for (const it of items) listEl.appendChild(renderItem(it));
  emptyEl.hidden = items.length > 0;
}

async function loadItems() {
  try {
    const res = await api("/api/items");
    if (!res.ok) return;
    renderList(await res.json());
  } catch (e) { /* auth handled in api() */ }
}

// ---------- sending ----------

async function sendText() {
  const ta = $("text-input");
  const text = ta.value;
  if (!text.trim()) return;
  try {
    const res = await api("/api/items", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text }),
    });
    if (res.ok) { ta.value = ""; toast("Enviado"); }
    else { const j = await res.json().catch(() => ({})); toast(j.error || "Falhou"); }
  } catch (e) { /* ignore */ }
}

async function uploadFiles(files) {
  for (const file of files) {
    const fd = new FormData();
    fd.append("file", file, file.name || "arquivo");
    try {
      const res = await api("/api/upload", { method: "POST", body: fd });
      if (!res.ok) { const j = await res.json().catch(() => ({})); toast(j.error || "Upload falhou"); }
      else { toast("Enviado"); }
    } catch (e) { /* ignore */ }
  }
}

// ---------- live updates (SSE) ----------

function connectSSE() {
  if (evtSource) evtSource.close();
  evtSource = new EventSource("/api/events" + tokenParam());
  evtSource.addEventListener("open", () => { statusDot.className = "dot live"; statusDot.title = "ao vivo"; });
  evtSource.addEventListener("update", () => { loadItems(); });
  evtSource.addEventListener("error", async () => {
    statusDot.className = "dot off";
    statusDot.title = "reconectando…";
    if (evtSource) { evtSource.close(); evtSource = null; }
    clearTimeout(reconnectTimer);
    // If the session expired, surface the PIN modal instead of looping on 401.
    try {
      const me = await (await fetch("/api/me", { credentials: "same-origin", headers: authHeaders() })).json();
      if (me.needPin && !me.auth) { onAuthLost(); return; }
    } catch (e) { /* offline — keep retrying */ }
    reconnectTimer = setTimeout(connectSSE, 3000);
  });
}

// ---------- auth ----------

function showPin() {
  $("pin-modal").hidden = false;
  $("pin-input").focus();
}

async function start() {
  $("pin-modal").hidden = true;
  await loadItems();
  connectSSE();
}

async function init() {
  let me;
  try { me = await (await fetch("/api/me", { credentials: "same-origin", headers: authHeaders() })).json(); }
  catch (e) { me = { auth: true, needPin: false }; }
  if (me.needPin) $("btn-logout").hidden = false;
  if (me.needPin && !me.auth) { showPin(); return; }
  start();
}

// ---------- wire up events ----------

$("btn-send").addEventListener("click", sendText);
$("text-input").addEventListener("keydown", (e) => {
  if ((e.ctrlKey || e.metaKey) && e.key === "Enter") { e.preventDefault(); sendText(); }
});

$("file-input").addEventListener("change", (e) => {
  if (e.target.files && e.target.files.length) uploadFiles(e.target.files);
  e.target.value = "";
});

$("btn-clear").addEventListener("click", async () => {
  if (!confirm("Apagar TODOS os itens?")) return;
  try { await api("/api/clear", { method: "POST" }); } catch (e) { /* ignore */ }
});

$("btn-logout").addEventListener("click", async () => {
  saveToken("");
  try { await fetch("/api/logout", { method: "POST", credentials: "same-origin" }); } catch (e) {}
  location.reload();
});

$("pin-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const pin = $("pin-input").value;
  const errEl = $("pin-error");
  errEl.hidden = true;
  try {
    const res = await fetch("/api/login", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ pin }),
    });
    if (res.ok) {
      const j = await res.json().catch(() => ({}));
      if (j.token) saveToken(j.token);
      $("pin-input").value = "";
      start();
    } else {
      const j = await res.json().catch(() => ({}));
      errEl.textContent = j.error || "PIN incorreto";
      errEl.hidden = false;
    }
  } catch (e2) { errEl.textContent = "Erro de conexão"; errEl.hidden = false; }
});

// paste images anywhere
document.addEventListener("paste", (e) => {
  const items = e.clipboardData && e.clipboardData.items;
  if (!items) return;
  const files = [];
  for (const it of items) {
    if (it.kind === "file") { const f = it.getAsFile(); if (f) files.push(f); }
  }
  if (files.length) { e.preventDefault(); uploadFiles(files); }
});

// drag & drop
const overlay = $("drop-overlay");
let dragDepth = 0;
window.addEventListener("dragenter", (e) => { e.preventDefault(); dragDepth++; overlay.hidden = false; });
window.addEventListener("dragover", (e) => { e.preventDefault(); });
window.addEventListener("dragleave", (e) => { e.preventDefault(); dragDepth--; if (dragDepth <= 0) { overlay.hidden = true; dragDepth = 0; } });
window.addEventListener("drop", (e) => {
  e.preventDefault();
  dragDepth = 0; overlay.hidden = true;
  if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length) uploadFiles(e.dataTransfer.files);
});

if ("serviceWorker" in navigator) {
  window.addEventListener("load", () => navigator.serviceWorker.register("/sw.js").catch(() => {}));
}

init();
