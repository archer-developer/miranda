// Minimal service worker — satisfies Chrome's PWA installability requirement
// (a registered SW with a fetch event handler is mandatory). No caching
// strategy is applied: every request is forwarded to the network so the
// dashboard always shows fresh data. The SW's sole role here is to unlock
// the "Add to Home Screen" prompt on Android/Chrome.
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', e => e.waitUntil(clients.claim()));
self.addEventListener('fetch', e => e.respondWith(fetch(e.request)));
