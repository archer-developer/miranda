// Real-time log viewer: tabs between the app log and LLM trace, both fed by
// the one shared WebSocket connection (see ../ws.js) — internal/hub mirrors
// both log files into hub events tagged "app_log"/"llm_log", so there's no
// separate WS endpoint and no file-tailing/rotation to handle here at all.
// ws.js's replay() already has history from before this screen mounted (the
// hub's own ring buffer), so the pane isn't empty on first visit.
//
// The two tabs render very differently. "app_log" stays exactly what it's
// always been: one plain-text line per hub.Event. "llm_log" is fed by
// hub.Hub.LLMTraceWriter instead of the plain per-line hub.Writer: each
// event's Data is already a whole, server-parsed
// miranda-llm/llmtrace/analyze.Block (one request/response per LLM call,
// reassembled backend-side from internal/llmtrace's block format) — see
// logs-trace-view.js, which renders it as a collapsed summary row that
// expands into a foldable JSON tree (see the vendored
// vendor/json-formatter-js/).
import { t } from "../i18n.js";
import { icon } from "../icons.js";
import { on, replay } from "../ws.js";
import { buildTraceRow } from "./logs-trace-view.js";

const TABS = [
  { source: "app_log", key: "logs_tab_app", fallback: "App log" },
  { source: "llm_log", key: "logs_tab_llm", fallback: "LLM trace" },
];

// How close to the bottom (in px) the pane has to be for a new line/call to
// auto-scroll it further — otherwise a line arriving while someone's
// scrolled up reading earlier output would yank them back down. Applied to
// both tabs: for "llm_log" this means a newly-completed call still appends
// at the bottom and pulls the view down with it, exactly like a new
// app_log line does today, as long as the user hasn't scrolled up to read
// an earlier one.
const AUTOSCROLL_THRESHOLD = 48;

let activeSource = TABS[0].source;
let paneEl, emptyEl;
let unsubscribers = [];

// The <div space-y-*> row list appendLlmEvent appends fresh rows into — kept
// as its own element (rather than appending rows straight into paneEl, like
// the plain-text app_log lines do) purely so the rows can have breathing
// room between them via `space-y` without also spacing out app_log's
// intentionally tight, single-line-height text rows.
let llmListEl = null;

function isNearBottom() {
  return paneEl.scrollHeight - paneEl.scrollTop - paneEl.clientHeight < AUTOSCROLL_THRESHOLD;
}

/** Shows the shared empty-state placeholder with tab-appropriate copy. */
function showEmptyState(message) {
  emptyEl.querySelector(".empty-text").textContent = message;
  paneEl.appendChild(emptyEl);
  emptyEl.classList.remove("hidden");
}

function appendLine(ev) {
  if (ev.source !== activeSource) return;
  const stick = isNearBottom();

  emptyEl.classList.add("hidden");
  const line = document.createElement("div");
  line.className = "whitespace-pre-wrap break-words py-0.5 text-(--color-text-muted)";
  line.textContent = ev.message;
  paneEl.appendChild(line);

  if (stick) paneEl.scrollTop = paneEl.scrollHeight;
}

/** Renders the "llm_log" tab from scratch: each replayed event's Data is
 * already a whole, server-parsed call record (see hub.Hub.LLMTraceWriter),
 * so there's no backlog re-parsing left to do here — just map and render. */
function renderLlmLog() {
  const blocks = replay("llm_log")
    .map((ev) => ev.data)
    .filter(Boolean);

  if (blocks.length === 0) {
    llmListEl = null;
    showEmptyState(t("logs_llm_empty", "No traced calls yet."));
    return;
  }
  emptyEl.classList.add("hidden");
  llmListEl = document.createElement("div");
  llmListEl.className = "space-y-2";
  for (const block of blocks) llmListEl.appendChild(buildTraceRow(block));
  paneEl.appendChild(llmListEl);
  paneEl.scrollTop = paneEl.scrollHeight;
}

/** Live handler for new "llm_log" events while that tab is active — appends
 * exactly one new row per event instead of rebuilding the whole list (which
 * would also collapse whatever row the user currently has open to read). */
function appendLlmEvent(ev) {
  if (ev.source !== "llm_log" || activeSource !== "llm_log" || !ev.data) return;

  const stick = isNearBottom();
  emptyEl.classList.add("hidden");
  if (!llmListEl) {
    // The very first call to arrive after the tab was shown empty —
    // renderLlmLog() left llmListEl unset in that case (see above).
    llmListEl = document.createElement("div");
    llmListEl.className = "space-y-2";
    paneEl.appendChild(llmListEl);
  }
  llmListEl.appendChild(buildTraceRow(ev.data));
  if (stick) paneEl.scrollTop = paneEl.scrollHeight;
}

function renderActiveTab() {
  paneEl.innerHTML = "";
  if (activeSource === "llm_log") {
    renderLlmLog();
    return;
  }
  const lines = replay(activeSource);
  if (lines.length === 0) {
    showEmptyState(t("logs_empty", "No log lines yet."));
  } else {
    emptyEl.classList.add("hidden");
    for (const ev of lines) appendLine(ev);
  }
  paneEl.scrollTop = paneEl.scrollHeight;
}

function selectTab(source, container) {
  if (source === activeSource) return;
  activeSource = source;
  container.querySelectorAll("[data-log-tab]").forEach((btn) => {
    const isActive = btn.dataset.logTab === source;
    btn.classList.toggle("bg-(--color-surface-2)", isActive);
    btn.classList.toggle("text-(--color-text)", isActive);
    btn.classList.toggle("text-(--color-text-faint)", !isActive);
    btn.setAttribute("aria-selected", String(isActive));
  });
  renderActiveTab();
}

export function mount(container) {
  container.innerHTML = `
    <div class="flex h-full flex-col">
      <!-- min-h-0 here is load-bearing (see screens/chat.js's #chat-scroll
           for the same rule spelled out): every flex-column ancestor
           between a fixed header/tabs block and the #logs-pane's flex-1
           overflow-y-auto below needs it, or this wrapper grows to fit the
           pane's content instead of shrinking to the available height, and
           the pane never actually scrolls. -->
      <div class="mx-auto flex min-h-0 w-full max-w-5xl flex-1 flex-col px-4 py-6 sm:px-6 sm:py-8">
        <div class="flex items-center justify-between gap-3">
          <h1 class="text-2xl font-semibold tracking-tight text-(--color-text)">${t("nav_logs", "Logs")}</h1>
          <span class="flex items-center gap-1.5 text-xs font-medium text-(--color-success-text)">
            <span class="h-1.5 w-1.5 rounded-full bg-(--color-success-dot) animate-pulse-soft"></span>${t("logs_live_badge", "Live")}
          </span>
        </div>

        <div role="tablist" aria-label="${t("nav_logs", "Logs")}" class="mt-5 flex gap-1 rounded-lg border border-(--color-border) bg-(--color-surface)/40 p-1 text-sm">
          ${TABS.map(
            (tab) =>
              `<button type="button" role="tab" data-log-tab="${tab.source}" aria-selected="false"
                 class="flex-1 rounded-md px-3 py-1.5 font-medium text-(--color-text-faint) transition-colors focus-visible:outline-none">${t(tab.key, tab.fallback)}</button>`,
          ).join("")}
        </div>

        <div id="logs-pane" class="scrollbar-thin relative mt-3 min-h-0 flex-1 overflow-y-auto rounded-xl border border-(--color-border) bg-(--color-bg)/60 p-4 font-mono text-xs leading-relaxed"></div>
      </div>
    </div>`;

  paneEl = container.querySelector("#logs-pane");

  emptyEl = document.createElement("div");
  emptyEl.className = "flex h-full flex-col items-center justify-center gap-3 py-12 text-center font-sans";
  emptyEl.innerHTML = `
    <span class="flex h-10 w-10 items-center justify-center rounded-full bg-(--color-surface-2) text-(--color-text-faint)">${icon("logs", "h-4 w-4")}</span>
    <p class="empty-text text-sm text-(--color-text-faint)"></p>`;

  container.querySelectorAll("[data-log-tab]").forEach((btn) => {
    btn.addEventListener("click", () => selectTab(btn.dataset.logTab, container));
  });

  // selectTab() no-ops on an unchanged source, so drive the very first
  // render directly instead.
  container.querySelectorAll("[data-log-tab]").forEach((btn) => {
    const isActive = btn.dataset.logTab === activeSource;
    btn.classList.toggle("bg-(--color-surface-2)", isActive);
    btn.classList.toggle("text-(--color-text)", isActive);
    btn.classList.toggle("text-(--color-text-faint)", !isActive);
    btn.setAttribute("aria-selected", String(isActive));
  });
  renderActiveTab();

  // "app_log" keeps the original one-line-in-one-line-out subscription;
  // "llm_log" needs its own handler since each event is a whole call
  // record, not a plain-text line (see appendLlmEvent above).
  unsubscribers = [on("app_log", appendLine), on("llm_log", appendLlmEvent)];
}

export function unmount() {
  unsubscribers.forEach((fn) => fn());
  unsubscribers = [];
}
