// Full-screen list of the logged-in user's own notifications (server scopes
// GET /api/notifications to currentUser(r) — see
// internal/webui/notifications.go), opened via the header bell rather than
// a persistent nav link. Structurally mirrors history.js's list screen, but
// every row is always fully rendered — no expand/collapse — since a
// notification's body is short by construction (see
// docs/adr/native-notifications.md).
//
// refreshAndMarkRead is exported (not just called from mount) because it's
// also what app.js calls when a "notification" chat-ws.js event arrives
// while this screen is already open — see that file's wiring comment:
// reloading the list *is* the read fact, which is why both the POST
// /api/notifications/read call AND clearing the bell's dot live inside this
// same function, rather than being separate steps a caller (mount() below,
// or app.js's live-event listener) must remember to also trigger.
import { t } from "../i18n.js";
import { icon } from "../icons.js";
import { renderInlineText } from "../inline-text.js";
import * as notifyBadge from "../notify-badge.js";

// mountedContainer is non-null only while this screen is the active route —
// set in mount(), cleared in unmount() — so a live event arriving after
// navigating away never tries to render into a detached DOM node. app.js's
// own location.hash check already avoids calling refreshAndMarkRead in that
// case; this is just the matching cleanup on this side.
let mountedContainer = null;

function formatDate(iso) {
  try {
    return new Date(iso).toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
  } catch {
    return iso;
  }
}

function notificationRow(n) {
  const row = document.createElement("div");
  row.className = "rounded-xl border border-(--color-border) bg-(--color-surface)/40 px-4 py-3.5 sm:px-5";

  const header = document.createElement("div");
  header.className = "flex items-baseline justify-between gap-3";

  const title = document.createElement("span");
  title.className = "text-sm font-medium text-(--color-text)";
  title.textContent = n.title;

  const time = document.createElement("span");
  time.className = "shrink-0 text-xs text-(--color-text-faint)";
  time.textContent = formatDate(n.created_at);

  header.append(title, time);

  const body = document.createElement("p");
  body.className = "mt-1 whitespace-pre-wrap text-sm text-(--color-text-muted)";
  renderInlineText(body, n.body);

  row.append(header, body);
  return row;
}

function emptyState() {
  return `
    <div class="flex flex-col items-center gap-3 px-6 py-20 text-center">
      <span class="flex h-12 w-12 items-center justify-center rounded-full bg-(--color-surface-2) text-(--color-text-faint)">${icon("bell", "h-5 w-5")}</span>
      <div class="space-y-1">
        <p class="text-sm font-medium text-(--color-text-muted)">${t("notifications_empty", "No notifications yet.")}</p>
      </div>
    </div>`;
}

function errorState(message) {
  const wrap = document.createElement("div");
  wrap.className =
    "flex flex-col items-center gap-3 rounded-xl border border-(--color-danger-border) bg-(--color-danger-bg) px-6 py-12 text-center";
  wrap.innerHTML = `<span class="text-(--color-danger-icon)">${icon("alert-circle", "h-6 w-6")}</span><p class="message-text text-sm text-(--color-danger-text)"></p>`;
  wrap.querySelector(".message-text").textContent = `${t("failed_to_load", "Failed to load:")} ${message}`;
  return wrap;
}

/** Fetches the list, renders it into whichever container is currently
 * mounted (a no-op if none is — see mountedContainer's doc comment above),
 * and marks everything read. Exported for app.js's live-update listener;
 * mount() below is just its first caller. */
export async function refreshAndMarkRead() {
  if (!mountedContainer) return;
  const listEl = mountedContainer.querySelector("#notifications-list");
  if (!listEl) return;

  try {
    const res = await fetch("/api/notifications");
    if (!res.ok) throw new Error(String(res.status));
    const { notifications } = await res.json();
    listEl.innerHTML = "";
    if (!notifications || notifications.length === 0) {
      listEl.innerHTML = emptyState();
    } else {
      for (const n of notifications) listEl.appendChild(notificationRow(n));
    }
  } catch (err) {
    listEl.innerHTML = "";
    listEl.appendChild(errorState(String(err)));
    return; // don't mark read on a failed load — nothing was actually shown
  }

  // The list just rendered — that's the read fact (see this file's
  // top-of-file comment). Best-effort: a failed mark-read leaves the bell
  // dot lit, which just means the user sees it again next time, not a
  // broken screen.
  try {
    await fetch("/api/notifications/read", { method: "POST" });
    notifyBadge.setUnread(false);
  } catch {
    /* best-effort, see above — dot deliberately left as-is on failure */
  }
}

export function mount(container) {
  container.innerHTML = `
    <div class="scrollbar-thin h-full overflow-y-auto">
      <div class="mx-auto max-w-3xl px-4 py-6 sm:px-6 sm:py-8">
        <h1 class="text-2xl font-semibold tracking-tight text-(--color-text)">${t("notifications_title", "Notifications")}</h1>
        <div id="notifications-list" class="mt-6 space-y-2.5"></div>
      </div>
    </div>`;
  mountedContainer = container;
  refreshAndMarkRead();
}

export function unmount() {
  mountedContainer = null;
}
