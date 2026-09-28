// Minimal service worker — satisfies Chrome's PWA installability requirement
// (a registered SW with a fetch event handler is mandatory). No caching
// strategy is applied: every request is forwarded to the network so the
// dashboard always shows fresh data. Also handles Web Push delivery (push/
// notificationclick below) — see docs/adr/native-notifications.md.
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', e => e.waitUntil(clients.claim()));
self.addEventListener('fetch', e => e.respondWith(fetch(e.request)));

// A push message's body is the JSON {title, body} internal/notify.Service
// sends (see notify/service.go's payload type) — shown as a native OS
// notification even if no Miranda tab is open. waitUntil keeps the worker
// alive until showNotification's promise settles, per the Push API's own
// requirement that a push event handler not return before displaying
// something (browsers penalize "silent" pushes).
self.addEventListener('push', event => {
  let data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch {
    /* malformed payload: fall back to a bare notification below */
  }
  event.waitUntil(
    self.registration.showNotification(data.title || 'Miranda', {
      body: data.body || '',
      icon: '/static/brand/icon-192.png',
      badge: '/static/brand/icon-192.png',
    }),
  );
});

// Clicking the notification opens (or focuses, if already open) the
// notifications screen — the one place the tapped content actually lives.
self.addEventListener('notificationclick', event => {
  event.notification.close();
  event.waitUntil(
    clients.matchAll({ type: 'window', includeUncontrolled: true }).then(windows => {
      for (const win of windows) {
        if ('focus' in win) {
          win.focus();
          if ('navigate' in win) win.navigate('/#/notifications');
          return;
        }
      }
      return clients.openWindow('/#/notifications');
    }),
  );
});
