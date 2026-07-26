// Full-screen, read-only version of the dialog history browser: lists the
// logged-in user's own conversations (server already scopes /api/dialogs to
// them — see internal/webui.handleDialogs), click to expand messages.
import { t } from "../i18n.js";

let listEl;

function formatDate(iso) {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

function renderMessages(container, messages) {
  container.innerHTML = "";
  for (const m of messages) {
    const line = document.createElement("div");
    line.className = "text-xs text-slate-400";
    const roleSpan = document.createElement("span");
    roleSpan.className = "font-medium text-slate-300";
    roleSpan.textContent = m.role + ": ";
    line.appendChild(roleSpan);
    line.appendChild(document.createTextNode(m.content));
    container.appendChild(line);
  }
}

async function toggle(conv, detail) {
  if (detail.dataset.loaded === "true") {
    detail.classList.toggle("hidden");
    return;
  }
  const res = await fetch(`/api/dialogs/${encodeURIComponent(conv.id)}`);
  const messages = await res.json();
  renderMessages(detail, messages);
  detail.dataset.loaded = "true";
  detail.classList.remove("hidden");
}

async function load() {
  listEl.textContent = t("loading_ellipsis", "Loading…");
  try {
    const res = await fetch("/api/dialogs?limit=100");
    const conversations = await res.json();
    listEl.innerHTML = "";
    if (!conversations || conversations.length === 0) {
      listEl.textContent = t("no_conversations", "No conversations yet.");
      return;
    }
    for (const conv of conversations) {
      const item = document.createElement("div");
      item.className = "rounded-md border border-slate-800 p-3";

      const header = document.createElement("div");
      header.className = "cursor-pointer text-sm text-slate-200";
      header.textContent = formatDate(conv.started_at) + (conv.ended_at ? "" : " · " + t("history_open_badge", "open"));
      item.appendChild(header);

      if (conv.summary) {
        const summary = document.createElement("div");
        summary.className = "mt-1 text-xs text-slate-500";
        summary.textContent = conv.summary;
        item.appendChild(summary);
      }

      const detail = document.createElement("div");
      detail.className = "mt-2 hidden space-y-1 border-t border-slate-800 pt-2";
      item.appendChild(detail);

      header.addEventListener("click", () => toggle(conv, detail));
      listEl.appendChild(item);
    }
  } catch (err) {
    listEl.textContent = `${t("failed_to_load", "Failed to load:")} ${err}`;
  }
}

export function mount(container) {
  container.innerHTML = `
    <div class="flex h-full flex-col overflow-y-auto p-4 sm:p-6">
      <h1 class="mb-4 text-lg font-semibold">${t("dialog_history_title", "Dialog history")}</h1>
      <div id="history-list" class="space-y-2"></div>
    </div>`;
  listEl = container.querySelector("#history-list");
  load();
}

export function unmount() {}
