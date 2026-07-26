// Full-screen version of the memory editor: GET/PUT /api/memory, scoped to
// the logged-in user server-side (see internal/webui.handleGetMemory).
import { t } from "../i18n.js";
import { showToast } from "../toast.js";

let textEl, saveBtn, formEl;

function setSaving(saving) {
  saveBtn.disabled = saving;
  saveBtn.innerHTML = saving
    ? `<span class="h-4 w-4 animate-spin rounded-full border-2 border-white/30 border-t-white"></span>${t("memory_saving_ellipsis", "Saving…")}`
    : t("memory_save_button", "Save");
}

async function load() {
  textEl.disabled = true;
  textEl.placeholder = t("loading_ellipsis", "Loading…");
  try {
    const res = await fetch("/api/memory");
    if (!res.ok) throw new Error(String(res.status));
    const data = await res.json();
    textEl.value = data.content || "";
  } catch (err) {
    showToast(`${t("failed_to_load", "Failed to load:")} ${err}`, "error");
  } finally {
    textEl.disabled = false;
    textEl.placeholder = t("memory_placeholder", "Nothing remembered yet.");
  }
}

async function save() {
  setSaving(true);
  try {
    const res = await fetch("/api/memory", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ content: textEl.value }),
    });
    if (!res.ok) throw new Error(String(res.status));
    showToast(t("memory_saved", "Saved"), "success");
  } catch (err) {
    showToast(`${t("request_failed", "Request failed:")} ${err}`, "error");
  } finally {
    setSaving(false);
  }
}

function onSubmit(e) {
  e.preventDefault();
  save();
}

// Cmd/Ctrl+S from inside the textarea saves without leaving the keyboard —
// a small but expected affordance for a screen that's essentially a text
// editor.
function onKeydown(e) {
  if ((e.metaKey || e.ctrlKey) && e.key === "s") {
    e.preventDefault();
    save();
  }
}

export function mount(container) {
  container.innerHTML = `
    <div class="scrollbar-thin h-full overflow-y-auto">
      <form id="memory-form" class="mx-auto flex h-full max-w-3xl flex-col px-4 py-6 sm:px-6 sm:py-8">
        <div class="mb-1 flex items-center justify-between gap-3">
          <h1 class="text-2xl font-semibold tracking-tight text-(--color-text)">${t("memory_title", "My memory")}</h1>
          <button type="submit" id="memory-save"
            class="flex items-center gap-2 rounded-lg bg-(--color-accent) px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-(--color-accent-hover) focus-visible:outline-none active:bg-(--color-accent-active) disabled:cursor-not-allowed disabled:opacity-60">
            ${t("memory_save_button", "Save")}
          </button>
        </div>
        <p class="mb-4 text-sm text-(--color-text-faint)">${t("memory_hint", "Miranda fills this in automatically as she learns about you. Edit or delete anything that's wrong.")}</p>
        <label for="memory-text" class="sr-only">${t("memory_title", "My memory")}</label>
        <textarea id="memory-text" placeholder="${t("memory_placeholder", "Nothing remembered yet.")}"
          class="scrollbar-thin min-h-[16rem] flex-1 resize-none rounded-xl border border-(--color-border-strong) bg-(--color-surface)/40 px-4 py-3.5 font-mono text-sm leading-relaxed text-(--color-text) transition-colors placeholder:text-(--color-text-faint) hover:border-(--color-text-faint) focus:border-(--color-accent-emphasis) focus:outline-none focus-visible:outline-none"></textarea>
      </form>
    </div>`;

  formEl = container.querySelector("#memory-form");
  textEl = container.querySelector("#memory-text");
  saveBtn = container.querySelector("#memory-save");

  formEl.addEventListener("submit", onSubmit);
  textEl.addEventListener("keydown", onKeydown);
  load();
}

export function unmount() {}
