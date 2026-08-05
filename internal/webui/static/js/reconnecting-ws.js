// Shared exponential-backoff reconnecting WebSocket driver used by both
// ws.js (the global /ws/logs tail) and chat-ws.js (the per-user
// /ws/chat/{username} stream) — the two need identical backoff/reconnect
// mechanics, differing only in the URL and what they do with each message,
// so that logic lives here once instead of drifting between two copies.
const RECONNECT_BASE_MS = 2000;
const RECONNECT_MAX_MS = 30000;

/**
 * Opens a WebSocket to `url` and keeps it open for the page's lifetime,
 * reconnecting with capped exponential backoff on every close (a real
 * outage logs one growing-delay line per attempt instead of spamming at a
 * fixed interval). There's no way to stop it — every current caller is a
 * single app-lifetime connection, matching ws.js's/chat-ws.js's own
 * `connect()` singleton guards.
 *
 * handlers:
 *   onOpen() — called every time a connection (including reconnects) opens.
 *   onClose({ upSecs, code, reason, wasClean, nextDelayMs }) — called on
 *     every close, before the reconnect it schedules.
 *   onMessage(data) — called with the parsed JSON payload of each message;
 *     malformed frames are silently dropped.
 */
export function connectReconnecting(url, handlers) {
  let reconnectDelay = RECONNECT_BASE_MS;

  function open() {
    const ws = new WebSocket(url);
    let openedAt = null;

    ws.onopen = () => {
      openedAt = Date.now();
      reconnectDelay = RECONNECT_BASE_MS; // a successful connection resets the backoff
      handlers.onOpen?.();
    };
    ws.onclose = (event) => {
      const upSecs = openedAt ? ((Date.now() - openedAt) / 1000).toFixed(1) : "?";
      const delay = reconnectDelay;
      reconnectDelay = Math.min(reconnectDelay * 2, RECONNECT_MAX_MS);
      handlers.onClose?.({ upSecs, code: event.code, reason: event.reason, wasClean: event.wasClean, nextDelayMs: delay });
      // A server-side session that died (e.g. a process restart wiping
      // internal/session.Store — see auth-fetch.js) makes every future
      // Upgrade request here get rejected before the WS handshake even
      // completes, which the browser exposes to us as nothing more than an
      // opaque, reason-less close (code 1006, no `reason` — WebSocket
      // doesn't carry the HTTP status the server actually sent). Left
      // alone, that's indistinguishable from a plain network blip and this
      // driver would just keep retrying forever at growing backoff,
      // explaining nothing. Piggyback a cheap authenticated GET on the
      // fetch() this app already has wrapped (see auth-fetch.js): if the
      // session really is gone, its own 401/403 handling redirects to
      // /login on its own — nothing to do here either way, so the result
      // is intentionally ignored and never blocks the reconnect below.
      fetch("/api/session").catch(() => {});
      setTimeout(open, delay);
    };
    // onerror fires alongside (just before) onclose on a failed/dropped
    // connection, but the Event object carries no useful detail of its own
    // (no code/reason) beyond what onclose already reports, and the browser
    // already runs the close sequence on its own after a connection error —
    // this close() call is a defensive no-op, not worth its own handler.
    ws.onerror = () => ws.close();
    ws.onmessage = (event) => {
      try {
        handlers.onMessage?.(JSON.parse(event.data));
      } catch {
        /* ignore malformed frames */
      }
    };
  }

  open();
}
