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

// Logged at every open/close so intermittent drops show up in the browser
// console with enough detail (close code + reason + how long the connection
// lasted) to tell a server-side restart apart from a network blip or a
// proxy/idle timeout — all three look identical from setStatus() alone.
// Reconnect backs off exponentially (capped) instead of retrying at a fixed
// interval forever: during a real outage a fixed 2s retry would otherwise
// log indefinitely and bury the one transition that matters (down → up) in
// near-duplicate noise — exactly the opposite of what this logging is for.
const RECONNECT_BASE_MS = 2000;
const RECONNECT_MAX_MS = 30000;
let reconnectDelay = RECONNECT_BASE_MS;

export function connect() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(`${proto}//${location.host}/ws/logs`);
  let openedAt = null; // this connection's own lifetime, not shared across reconnects

  ws.onopen = () => {
    openedAt = Date.now();
    reconnectDelay = RECONNECT_BASE_MS; // a successful connection resets the backoff
    console.info("[ws] connected");
    setStatus(true);
  };
  ws.onclose = (event) => {
    const upSecs = openedAt ? ((Date.now() - openedAt) / 1000).toFixed(1) : "?";
    const delay = reconnectDelay;
    reconnectDelay = Math.min(reconnectDelay * 2, RECONNECT_MAX_MS);
    console.warn(
      `[ws] closed after ${upSecs}s (code=${event.code} reason=${JSON.stringify(event.reason)} wasClean=${event.wasClean}) — reconnecting in ${delay}ms`,
    );
    setStatus(false);
    setTimeout(connect, delay);
  };
  // onerror fires alongside (just before) onclose on a failed/dropped
  // connection, but the Event object carries no useful detail of its own
  // (no code/reason) beyond what onclose already logs above, and the
  // browser already runs the close sequence on its own after a connection
  // error — this close() call is a defensive no-op, not worth a second log
  // line for the same failed attempt.
  ws.onerror = () => ws.close();
  ws.onmessage = (event) => {
    try {
      dispatch(JSON.parse(event.data));
    } catch {
      /* ignore malformed frames */
    }
  };
}
