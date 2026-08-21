// One persistent WebSocket connection to this user's own chat channel
// (GET /ws/chat/{username}, scoped server-side to window.MIRANDA_USER —
// see internal/httpapi.Server.handleWSChat), separate from ws.js's
// broadcast-to-everyone /ws/logs connection: this one only ever carries
// ChatEvent payloads for the logged-in user's own conversation, so the chat
// screen can stay in sync with turns that arrived over HA, Telegram, or
// another tab instead of only ever learning about its own POST
// /api/v1/input responses. Reconnect/backoff mechanics live in
// reconnecting-ws.js, shared with ws.js.
//
// This connection also drives the nav bar's ws-dot/ws-status indicator
// (rather than ws.js's /ws/logs socket): that indicator is shown on every
// screen, so it needs to reflect the connection whose health actually
// affects using the app — whether the user's own messages/replies are
// flowing — not the logs tail, which only the Logs screen cares about and
// which already shows its own "Live" badge there.
import { t } from "./i18n.js";
import { connectReconnecting } from "./reconnecting-ws.js";

const wsDot = document.getElementById("ws-dot");
const wsStatus = document.getElementById("ws-status");

function setStatus(connected) {
  // Sized/positioned to match the dot's initial markup in index.html
  // exactly (h-2 w-2 rounded-full) — only the color and the "still
  // settling in" pulse animation change once we know the real state.
  wsDot.className = `h-2 w-2 shrink-0 rounded-full ${connected ? "bg-(--color-success-dot)" : "bg-(--color-danger-icon) animate-pulse-soft"}`;
  wsStatus.textContent = connected ? t("ws_connected", "connected") : t("ws_disconnected", "disconnected — retrying…");
  wsDot.title = wsStatus.textContent;
}

const listeners = new Set();
// Fired on every *re*connection — deliberately not the very first connect,
// since chat.js's mount() already hydrates history itself for that case
// (see connect()'s `everConnected` guard below). What matters is every
// *later* reconnect (network blip, laptop sleep, server restart) fires
// this: since handleWSChat deliberately doesn't replay a backlog (see its
// doc comment), a subscriber must re-fetch history itself to recover
// anything published while this socket was down, rather than silently
// staying stale forever.
const reconnectListeners = new Set();

function dispatch(ev) {
  for (const fn of listeners) fn(ev);
}

/** Subscribe to every ChatEvent for this user. Returns an unsubscribe fn. */
export function on(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/** Subscribe to every *re*connection of this socket (not the initial connect — see `everConnected` below). Returns an unsubscribe fn. */
export function onReconnect(fn) {
  reconnectListeners.add(fn);
  return () => reconnectListeners.delete(fn);
}

let started = false;
// Set after the first successful open, so that open is never mistaken for
// a reconnect — app.js's router.start() mounts chat.js (which subscribes
// via onReconnect) synchronously, well before this connect() call's
// WebSocket handshake can possibly complete, so without this guard every
// initial page load would fire reconnectListeners once for real (mount's
// own loadHistory()) and once more here, visibly re-clearing and
// re-rendering the just-loaded thread.
let everConnected = false;

export function connect() {
  if (started) return; // app.js-lifetime singleton, same as ws.js's connect()
  started = true;

  const username = window.MIRANDA_USER?.username;
  if (!username) return; // not logged in (e.g. login page never calls this)

  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  connectReconnecting(`${proto}//${location.host}/ws/chat/${encodeURIComponent(username)}`, {
    onOpen: () => {
      console.info("[chat-ws] connected");
      setStatus(true);
      if (everConnected) {
        for (const fn of reconnectListeners) fn();
      }
      everConnected = true;
    },
    onClose: ({ upSecs, code, reason, wasClean, nextDelayMs }) => {
      console.warn(
        `[chat-ws] closed after ${upSecs}s (code=${code} reason=${JSON.stringify(reason)} wasClean=${wasClean}) — reconnecting in ${nextDelayMs}ms`,
      );
      setStatus(false);
    },
    onMessage: dispatch,
  });
}
