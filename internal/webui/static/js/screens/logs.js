// Real-time log viewer: tabs between the app log and LLM trace, both fed by
// the one shared WebSocket connection (see ../ws.js) — internal/hub.Writer
// mirrors both log files into hub events tagged "app_log"/"llm_log", so
// there's no separate WS endpoint and no file-tailing/rotation to handle
// here at all. ws.js's replay() already has history from before this
// screen mounted (the hub's own ring buffer), so the pane isn't empty on
// first visit.
import { t } from "../i18n.js";
import { on, replay } from "../ws.js";

const TABS = [
  { source: "app_log", key: "logs_tab_app", fallback: "App log" },
  { source: "llm_log", key: "logs_tab_llm", fallback: "LLM trace" },
];

let activeSource = TABS[0].source;
let paneEl;
let unsubscribers = [];

function appendLine(ev) {
  if (ev.source !== activeSource) return;
  const line = document.createElement("div");
  line.className = "whitespace-pre-wrap break-words";
  line.textContent = ev.message;
  paneEl.appendChild(line);
  paneEl.scrollTop = paneEl.scrollHeight;
}

function renderActiveTab() {
  paneEl.innerHTML = "";
  for (const ev of replay(activeSource)) appendLine(ev);
}

function selectTab(source, container) {
  activeSource = source;
  container.querySelectorAll("[data-log-tab]").forEach((btn) => {
    const isActive = btn.dataset.logTab === source;
    btn.classList.toggle("bg-slate-800", isActive);
    btn.classList.toggle("text-white", isActive);
  });
  renderActiveTab();
}

export function mount(container) {
  container.innerHTML = `
    <div class="flex h-full flex-col p-4 sm:p-6">
      <div class="mb-3 flex gap-2">
        ${TABS.map(
          (tab) =>
            `<button data-log-tab="${tab.source}" class="rounded-md border border-slate-700 px-3 py-1.5 text-sm text-slate-300 hover:bg-slate-800">${t(tab.key, tab.fallback)}</button>`,
        ).join("")}
      </div>
      <div id="logs-pane" class="flex-1 overflow-y-auto rounded-md border border-slate-800 bg-slate-950 p-3 font-mono text-xs text-slate-300"></div>
    </div>`;

  paneEl = container.querySelector("#logs-pane");
  container.querySelectorAll("[data-log-tab]").forEach((btn) => {
    btn.addEventListener("click", () => selectTab(btn.dataset.logTab, container));
  });

  selectTab(activeSource, container);
  unsubscribers = TABS.map((tab) => on(tab.source, appendLine));
}

export function unmount() {
  unsubscribers.forEach((fn) => fn());
  unsubscribers = [];
}
