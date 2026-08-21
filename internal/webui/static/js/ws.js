// One persistent WebSocket connection for the whole app lifetime — shared
// across every screen (unlike a per-page-load connection, this survives
// hash navigation between screens, matching the Logs screen's requirement
// to keep tailing live). This is purely a log/trace feed: the nav bar's
// connection indicator is driven by chat-ws.js instead (see that file's
// header comment for why), since that's the connection whose health
// actually matters to using the app — this one only feeds the Logs screen,
// which shows its own "Live" badge.
// Consumers subscribe by event.source via on(); the hub replays its recent
// buffer on connect (see internal/hub), so a screen mounted after some
// events already happened still gets that history via replay(source).
import { connectReconnecting } from "./reconnecting-ws.js";

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

// Logged at every open/close so intermittent drops show up in the browser
// console with enough detail (close code + reason + how long the connection
// lasted) to tell a server-side restart apart from a network blip or a
// proxy/idle timeout. Reconnect/backoff mechanics live in
// reconnecting-ws.js, shared with chat-ws.js's /ws/chat/{username}
// connection.
export function connect() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  connectReconnecting(`${proto}//${location.host}/ws/logs`, {
    onOpen: () => console.info("[ws] connected"),
    onClose: ({ upSecs, code, reason, wasClean, nextDelayMs }) => {
      console.warn(
        `[ws] closed after ${upSecs}s (code=${code} reason=${JSON.stringify(reason)} wasClean=${wasClean}) — reconnecting in ${nextDelayMs}ms`,
      );
    },
    onMessage: dispatch,
  });
}
