// Owns the header bell's red unread-dot (see index.html's #notify-bell-dot)
// and its icon — the one piece of UI shown on every screen, so it lives
// here rather than inside screens/notifications.js, which only mounts when
// the bell is actually clicked. app.js is the single place that decides
// *when* to call setUnread (see its own top-of-file wiring comment) — this
// module only knows how to paint the result.
import { icon } from "./icons.js";

const bell = document.getElementById("notify-bell");
const bellIcon = document.getElementById("notify-bell-icon");
const dot = document.getElementById("notify-bell-dot");

/** Loads the current unread count once (app boot) and paints the dot
 * accordingly — a plain GET, not a live subscription; live updates after
 * this come from app.js's chat-ws.js "notification" event listener. */
export async function init() {
  if (!bell) return; // NotifyEnabled was false — the bell was never rendered
  bellIcon.innerHTML = icon("bell", "h-5 w-5");
  try {
    const res = await fetch("/api/notifications/unread-count");
    if (!res.ok) return;
    const { count } = await res.json();
    setUnread(count > 0);
  } catch {
    /* best-effort: a failed initial load just leaves the dot off */
  }
}

/** Shows or hides the bell's red dot. */
export function setUnread(hasUnread) {
  if (dot) dot.hidden = !hasUnread;
}
