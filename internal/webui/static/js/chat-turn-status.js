// Tracks whether the logged-in user currently has an Orchestrator.Handle
// turn in flight (on any channel — HA, web, Telegram, scheduled), combining
// chat-ws.js's live turn_in_progress/turn_started/turn_ended events with
// ~5s REST polling of GET /api/turn-status as a fallback that converges on
// the server's actual state regardless of this tab's own WS health.
//
// This replaces the fixed client-side timeout chat.js used to guess with
// (waitForReplyViaWS(10000)) after a fetch() to /api/v1/input threw: that
// approach couldn't tell "the server is still working" from "the server is
// done and something else went wrong", and picked an arbitrary bound that
// was wrong for legitimately long turns (tool calls, web search,
// escalation). Server state is authoritative here — a dropped WS
// connection or a missed poll tick is never a problem, since the next
// signal (from either source) always converges on the truth.
import * as chatWs from "./chat-ws.js";

const POLL_INTERVAL_MS = 5000;
let pollTimer = null;
const listeners = new Set();

// { inProgress, startedAt: Date|null, confirmed }. `confirmed` is false only
// before the very first real signal (WS connect snapshot or REST poll)
// arrives — needed so a caller can tell "we know nothing yet" apart from
// "we know it's not in progress" (both look like inProgress:false
// otherwise), see currentStatus()'s doc comment.
let last = { inProgress: false, startedAt: null, confirmed: false };

// Bumped by every applied setStatus() — lets pollOnce() detect whether a
// fresher signal (a WS push, or another poll response) landed while its
// own request was still in flight, so it can discard its now-stale answer
// instead of regressing `last` backward (see pollOnce's doc comment).
let seq = 0;

function setStatus(inProgress, startedAt) {
  seq++;
  last = {
    inProgress,
    startedAt: inProgress ? (startedAt ?? last.startedAt ?? new Date()) : null,
    confirmed: true,
  };
  for (const fn of listeners) fn(last);
}

/** Subscribe to every status change (including the first confirmed reading).
 * Returns an unsubscribe fn. */
export function onStatusChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/** Returns the last known { inProgress, startedAt, confirmed } — see
 * `confirmed`'s doc comment above before trusting `inProgress` on its own;
 * an unconfirmed reading is a "don't know yet", not a "false". */
export function currentStatus() {
  return last;
}

/** Polls GET /api/turn-status once. A poll response can arrive *after* a
 * fresher WS event already updated `last` (the HTTP round trip has no
 * ordering guarantee relative to a WS push that started later but arrived
 * sooner) — applying it unconditionally could regress a correct "turn just
 * ended" back to a stale "in progress", which is exactly what produced a
 * spurious waiting bubble in chat.js. Snapshotting `seq` before the await
 * and comparing after detects that race: if anything else already updated
 * status while this request was in flight, this response is discarded as
 * stale rather than applied — the next poll tick or WS event will still
 * converge on the truth. */
async function pollOnce() {
  const startSeq = seq;
  try {
    const res = await fetch("/api/turn-status");
    if (res.ok) {
      const data = await res.json();
      if (seq !== startSeq) return;
      setStatus(!!data.in_progress, data.started_at ? new Date(data.started_at) : null);
    }
  } catch {
    // Network hiccup — the next tick, or a WS event, will resolve it; this
    // is exactly the kind of transient failure this module exists to be
    // resilient to.
  }
}

/** Starts ~5s REST polling (idempotent — a second call while already
 * polling is a no-op). Callers should pair this with stopPolling() so a
 * screen doesn't leak a timer across navigations. */
export function startPolling() {
  if (pollTimer) return;
  pollOnce();
  pollTimer = setInterval(pollOnce, POLL_INTERVAL_MS);
}

export function stopPolling() {
  clearInterval(pollTimer);
  pollTimer = null;
}

chatWs.on((ev) => {
  const d = ev.data;
  if (!d) return;
  if (d.type === "turn_in_progress") setStatus(!!d.in_progress, d.started_at ? new Date(d.started_at) : null);
  else if (d.type === "turn_started") setStatus(true, d.started_at ? new Date(d.started_at) : new Date());
  else if (d.type === "turn_ended") setStatus(false, null);
});
