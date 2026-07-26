// One persistent WebSocket connection for the whole app lifetime — shared
// across every screen (unlike a per-page-load connection, this survives
// hash navigation between screens, matching the connection indicator in
// the nav bar and the Logs screen's requirement to keep tailing live).
// Consumers subscribe by event.source via on(); the hub replays its recent
// buffer on connect (see internal/hub), so a screen mounted after some
// events already happened still gets that history via replay(source).
import { t } from "./i18n.js";

const wsDot = document.getElementById("ws-dot");
const wsStatus = document.getElementById("ws-status");

const listeners = new Map(); // source -> Set<fn(event)>
const buffered = new Map(); // source -> event[] (everything seen so far, for late subscribers)

function dispatch(ev) {
  if (!buffered.has(ev.source)) buffered.set(ev.source, []);
  buffered.get(ev.source).push(ev);

  const fns = listeners.get(ev.source);
  if (fns) for (const fn of fns) fn(ev);
}

/** Subscribe to events for one source ("assistant", "app_log", "llm_log", ...). */
export function on(source, fn) {
  if (!listeners.has(source)) listeners.set(source, new Set());
  listeners.get(source).add(fn);
  return () => listeners.get(source)?.delete(fn);
}

/** Everything received for source so far (e.g. to render on screen mount). */
export function replay(source) {
  return buffered.get(source) || [];
}

function setStatus(connected) {
  // Sized/positioned to match the dot's initial markup in index.html
  // exactly (h-2 w-2 rounded-full) — only the color and the "still
  // settling in" pulse animation change once we know the real state.
  wsDot.className = `h-2 w-2 shrink-0 rounded-full ${connected ? "bg-(--color-success-dot)" : "bg-(--color-danger-icon) animate-pulse-soft"}`;
  wsStatus.textContent = connected ? t("ws_connected", "connected") : t("ws_disconnected", "disconnected — retrying…");
  wsDot.title = wsStatus.textContent;
}

// Logged at every open/close/error so intermittent drops show up in the
// browser console with enough detail (close code + reason + how long the
// connection lasted) to tell a server-side restart apart from a network
// blip or a proxy/idle timeout — all three look identical from setStatus()
// alone.
const RECONNECT_DELAY_MS = 2000;
let connectedAt = null;

export function connect() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(`${proto}//${location.host}/ws/logs`);

  ws.onopen = () => {
    connectedAt = Date.now();
    console.info("[ws] connected");
    setStatus(true);
  };
  ws.onclose = (event) => {
    const upSecs = connectedAt ? ((Date.now() - connectedAt) / 1000).toFixed(1) : "?";
    connectedAt = null;
    console.warn(
      `[ws] closed after ${upSecs}s (code=${event.code} reason=${JSON.stringify(event.reason)} wasClean=${event.wasClean}) — reconnecting in ${RECONNECT_DELAY_MS}ms`,
    );
    setStatus(false);
    setTimeout(connect, RECONNECT_DELAY_MS);
  };
  // onerror fires alongside (just before) onclose on a failed/dropped
  // connection, but carries no useful detail of its own (the Event object
  // has no code/reason) — the close handler above is where the actual
  // diagnostics are logged. This just forces the close so reconnect isn't
  // delayed waiting on the socket to notice on its own.
  ws.onerror = () => {
    console.warn("[ws] error — closing socket");
    ws.close();
  };
  ws.onmessage = (event) => {
    try {
      dispatch(JSON.parse(event.data));
    } catch {
      /* ignore malformed frames */
    }
  };
}
