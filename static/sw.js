// Self-healing, network-only service worker.
//
// An earlier version cached the app shell, which could pin a stale app.js after
// an update (e.g. the cookie-based login sticking around). This version caches
// NOTHING: on activation it deletes every old cache, takes control, and reloads
// any open tab so the newest app is always served straight from the network.
self.addEventListener("install", () => self.skipWaiting());

self.addEventListener("activate", (event) => {
  event.waitUntil((async () => {
    try {
      const keys = await caches.keys();
      await Promise.all(keys.map((k) => caches.delete(k)));
    } catch (e) { /* ignore */ }
    await self.clients.claim();
    try {
      const clients = await self.clients.matchAll({ type: "window" });
      for (const c of clients) {
        c.navigate(c.url);
      }
    } catch (e) { /* ignore */ }
  })());
});

// A fetch listener keeps the app installable as a PWA, but it never intercepts:
// every request goes to the network, so nothing stale is ever served.
self.addEventListener("fetch", () => {});
