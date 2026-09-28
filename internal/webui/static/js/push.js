// Vanilla-JS glue between the browser's native Push API
// (navigator.serviceWorker + PushManager) and Miranda's /api/push/*
// endpoints (internal/webui/notifications.go) — used by the profile
// screen's "Push notifications" toggle. See sw.js for the service worker's
// own push/notificationclick handlers, and
// docs/adr/native-notifications.md for the overall design.

// base64urlToBuffer mirrors webauthn.js's own helper exactly — kept as a
// separate copy rather than a shared import since it's a two-line pure
// function and the two modules otherwise have nothing to do with each
// other.
function base64urlToBuffer(b64url) {
  const padded = b64url + "=".repeat((4 - (b64url.length % 4)) % 4);
  const base64 = padded.replace(/-/g, "+").replace(/_/g, "/");
  const raw = atob(base64);
  const bytes = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
  return bytes.buffer;
}

/** Whether this browser/context supports Web Push at all — false on any
 * insecure (non-HTTPS, non-localhost) origin, or a browser without the
 * Push API (notably iOS Safari below 16.4, and even 16.4+ unless the PWA
 * was actually added to the home screen). */
export function isSupported() {
  return "serviceWorker" in navigator && "PushManager" in window;
}

/** Whether this browser currently holds an active push subscription. */
export async function isSubscribed() {
  if (!isSupported()) return false;
  const registration = await navigator.serviceWorker.ready;
  const sub = await registration.pushManager.getSubscription();
  return sub !== null;
}

/** Requests notification permission, subscribes this browser to Web Push,
 * and registers the subscription server-side. Throws if permission is
 * denied or any step fails — callers show that as an error, same as
 * webauthn.js's registerPasskey. */
export async function subscribe() {
  const keyRes = await fetch("/api/push/vapid-key");
  if (!keyRes.ok) throw new Error(`vapid key fetch failed: ${keyRes.status}`);
  const vapidKey = await keyRes.text();

  const permission = await Notification.requestPermission();
  if (permission !== "granted") throw new Error("notification permission denied");

  const registration = await navigator.serviceWorker.ready;
  const subscription = await registration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: base64urlToBuffer(vapidKey),
  });

  const res = await fetch("/api/push/subscribe", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(subscription.toJSON()),
  });
  if (!res.ok) throw new Error(`subscribe failed: ${res.status}`);
}

/** Unsubscribes this browser from Web Push, both locally and server-side.
 * A no-op (not an error) if there was no active subscription. */
export async function unsubscribe() {
  if (!isSupported()) return;
  const registration = await navigator.serviceWorker.ready;
  const subscription = await registration.pushManager.getSubscription();
  if (!subscription) return;

  const endpoint = subscription.endpoint;
  await subscription.unsubscribe();

  await fetch("/api/push/subscribe", {
    method: "DELETE",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ endpoint }),
  });
}
