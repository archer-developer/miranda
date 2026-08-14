// PWA install prompt: shows a dismissible banner when the browser fires
// beforeinstallprompt (Chrome/Edge on Android, desktop Chrome). The event
// is captured early in index.html's <head> so it isn't missed if it fires
// before this deferred module executes; we pick it up here via window._pwaPrompt.

let deferredPrompt = window._pwaPrompt ?? null;

function showBanner() {
  const banner = document.getElementById('pwa-install-banner');
  if (banner) banner.hidden = false;
}

function hideBanner() {
  const banner = document.getElementById('pwa-install-banner');
  if (banner) banner.hidden = true;
}

// Show immediately if the event was already captured before this module ran.
if (deferredPrompt) showBanner();

window.addEventListener('beforeinstallprompt', e => {
  e.preventDefault();
  deferredPrompt = e;
  showBanner();
});

window.addEventListener('appinstalled', () => {
  deferredPrompt = null;
  hideBanner();
});

document.getElementById('pwa-install-btn')?.addEventListener('click', async () => {
  if (!deferredPrompt) return;
  deferredPrompt.prompt();
  await deferredPrompt.userChoice;
  deferredPrompt = null;
  hideBanner();
});

document.getElementById('pwa-install-dismiss')?.addEventListener('click', hideBanner);

// Register the service worker. Served at /sw.js (not /static/sw.js) so its
// default scope covers the entire app at /, not just /static/.
if ('serviceWorker' in navigator) {
  navigator.serviceWorker.register('/sw.js').catch(err => {
    console.warn('Miranda: SW registration failed:', err);
  });
}
