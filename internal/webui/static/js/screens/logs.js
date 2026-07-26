// Real-time log viewer: tabs between the app log and LLM trace, both fed by
// the one shared WebSocket connection (see ../ws.js) — internal/hub.Writer
// mirrors both log files into hub events tagged "app_log"/"llm_log", so
// there's no separate WS endpoint and no file-tailing/rotation to handle
// here at all. ws.js's replay() already has history from before this
// screen mounted (the hub's own ring buffer), so the pane isn't empty on
// first visit.
import { t } from "../i18n.js";
import { icon } from "../icons.js";
import { on, replay } from "../ws.js";

const TABS = [
  { source: "app_log", key: "logs_tab_app", fallback: "App log" },
  { source: "llm_log", key: "logs_tab_llm", fallback: "LLM trace" },
];

// How close to the bottom (in px) the pane has to be for a new line to
// auto-scroll it further — otherwise a line arriving while someone's
// scrolled up reading earlier output would yank them back down.
const AUTOSCROLL_THRESHOLD = 48;

let activeSource = TABS[0].source;
let paneEl, emptyEl;
let unsubscribers = [];

function isNearBottom() {
  return paneEl.scrollHeight - paneEl.scrollTop - paneEl.clientHeight < AUTOSCROLL_THRESHOLD;
}

function appendLine(ev) {
  if (ev.source !== activeSource) return;
  const stick = isNearBottom();

  emptyEl.classList.add("hidden");
  const line = document.createElement("div");
  line.className = "whitespace-pre-wrap break-words py-0.5 text-slate-300";
  line.textContent = ev.message;
  paneEl.appendChild(line);

  if (stick) paneEl.scrollTop = paneEl.scrollHeight;
}

function renderActiveTab() {
  paneEl.innerHTML = "";
  const lines = replay(activeSource);
  if (lines.length === 0) {
    paneEl.appendChild(emptyEl);
    emptyEl.classList.remove("hidden");
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
    btn.classList.toggle("bg-slate-800", isActive);
    btn.classList.toggle("text-white", isActive);
    btn.classList.toggle("text-slate-400", !isActive);
    btn.setAttribute("aria-selected", String(isActive));
  });
  renderActiveTab();
}

export function mount(container) {
  container.innerHTML = `
    <div class="flex h-full flex-col">
      <div class="mx-auto flex w-full max-w-5xl flex-1 flex-col px-4 py-6 sm:px-6 sm:py-8">
        <div class="flex items-center justify-between gap-3">
          <h1 class="text-2xl font-semibold tracking-tight text-white">${t("nav_logs", "Logs")}</h1>
          <span class="flex items-center gap-1.5 text-xs font-medium text-emerald-400">
            <span class="h-1.5 w-1.5 rounded-full bg-emerald-400 animate-pulse-soft"></span>${t("logs_live_badge", "Live")}
          </span>
        </div>

        <div role="tablist" aria-label="${t("nav_logs", "Logs")}" class="mt-5 flex gap-1 rounded-lg border border-slate-800 bg-slate-900/40 p-1 text-sm">
          ${TABS.map(
            (tab) =>
              `<button type="button" role="tab" data-log-tab="${tab.source}" aria-selected="false"
                 class="flex-1 rounded-md px-3 py-1.5 font-medium text-slate-400 transition-colors focus-visible:outline-none">${t(tab.key, tab.fallback)}</button>`,
          ).join("")}
        </div>

        <div id="logs-pane" class="scrollbar-thin relative mt-3 min-h-0 flex-1 overflow-y-auto rounded-xl border border-slate-800 bg-slate-950/60 p-4 font-mono text-xs leading-relaxed"></div>
      </div>
    </div>`;

  paneEl = container.querySelector("#logs-pane");

  emptyEl = document.createElement("div");
  emptyEl.className = "flex h-full flex-col items-center justify-center gap-3 py-12 text-center font-sans";
  emptyEl.innerHTML = `
    <span class="flex h-10 w-10 items-center justify-center rounded-full bg-slate-900 text-slate-600">${icon("logs", "h-4 w-4")}</span>
    <p class="text-sm text-slate-500">${t("logs_empty", "No log lines yet.")}</p>`;

  container.querySelectorAll("[data-log-tab]").forEach((btn) => {
    btn.addEventListener("click", () => selectTab(btn.dataset.logTab, container));
  });

  // selectTab() no-ops on an unchanged source, so drive the very first
  // render directly instead.
  container.querySelectorAll("[data-log-tab]").forEach((btn) => {
    const isActive = btn.dataset.logTab === activeSource;
    btn.classList.toggle("bg-slate-800", isActive);
    btn.classList.toggle("text-white", isActive);
    btn.classList.toggle("text-slate-400", !isActive);
    btn.setAttribute("aria-selected", String(isActive));
  });
  renderActiveTab();

  unsubscribers = TABS.map((tab) => on(tab.source, appendLine));
}

export function unmount() {
  unsubscribers.forEach((fn) => fn());
  unsubscribers = [];
}
