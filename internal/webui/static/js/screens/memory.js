// Full-screen version of the memory editor: GET/PUT /api/memory, scoped to
// the logged-in user server-side (see internal/webui.handleGetMemory).
import { t } from "../i18n.js";

let textEl, statusEl;

async function load() {
  try {
    const res = await fetch("/api/memory");
    if (!res.ok) return;
    const data = await res.json();
    textEl.value = data.content || "";
  } catch {
    statusEl.textContent = t("failed_to_load", "Failed to load:");
  }
}

async function save() {
  statusEl.textContent = t("memory_saving_ellipsis", "Saving…");
  try {
    const res = await fetch("/api/memory", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ content: textEl.value }),
    });
    statusEl.textContent = res.ok
      ? t("memory_saved", "Saved")
      : `${t("request_failed", "Request failed:")} ${res.status}`;
  } catch (err) {
    statusEl.textContent = `${t("request_failed", "Request failed:")} ${err}`;
  }
}

export function mount(container) {
  container.innerHTML = `
    <div class="flex h-full flex-col p-4 sm:p-6">
      <div class="mb-3 flex items-center justify-between">
        <h1 class="text-lg font-semibold">${t("memory_title", "My memory")}</h1>
        <span id="memory-status" class="text-xs text-slate-500"></span>
      </div>
      <textarea id="memory-text" placeholder="${t("memory_placeholder", "Nothing remembered yet.")}"
        class="w-full flex-1 resize-none rounded-md border border-slate-700 bg-slate-950 px-3 py-2 font-mono text-xs placeholder:text-slate-500 focus:border-slate-500 focus:outline-none"></textarea>
      <button id="memory-save" class="mt-3 self-start rounded-md bg-indigo-500 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-400 active:bg-indigo-600">
        ${t("memory_save_button", "Save")}
      </button>
    </div>`;

  textEl = container.querySelector("#memory-text");
  statusEl = container.querySelector("#memory-status");
  container.querySelector("#memory-save").addEventListener("click", save);
  load();
}

export function unmount() {}
