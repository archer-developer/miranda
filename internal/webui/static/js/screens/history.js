// Full-screen, read-only version of the dialog history browser: lists the
// logged-in user's own conversations (server already scopes /api/dialogs to
// them — see internal/webui.handleDialogs), click to expand messages.
import { t } from "../i18n.js";
import { icon } from "../icons.js";
import { extractDownloadBlocks, downloadChip, extractAttachmentBlocks, attachmentChip } from "../downloads.js";
import { renderInlineText } from "../inline-text.js";

let listEl;

const SOURCE_LABELS = {
  ha_assist: { key: "history_source_ha", fallback: "Voice" },
  web_ui: { key: "history_source_web", fallback: "Web" },
};

function formatDate(iso) {
  try {
    return new Date(iso).toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
  } catch {
    return iso;
  }
}

function sourceLabel(source) {
  const known = SOURCE_LABELS[source];
  return known ? t(known.key, known.fallback) : source;
}

function renderMessages(container, messages) {
  container.innerHTML = "";
  const turns = messages.filter((m) => m.role === "user" || m.role === "assistant");
  if (turns.length === 0) {
    container.innerHTML = `<p class="py-2 text-xs text-(--color-text-faint)">${t("no_conversations", "No conversations yet.")}</p>`;
    return;
  }
  for (const m of turns) {
    const line = document.createElement("div");
    line.className = "py-1.5";

    const row = document.createElement("div");
    row.className = "flex gap-2 text-sm leading-relaxed";

    const roleSpan = document.createElement("span");
    roleSpan.className = `mt-0.5 shrink-0 text-xs font-medium ${m.role === "user" ? "text-(--color-text-muted)" : "text-(--color-accent-emphasis)"}`;
    roleSpan.textContent = m.role === "user" ? t("history_role_user", "You") : "Miranda";

    // extractDownloadBlocks/extractAttachmentBlocks strip the server's
    // <download>{json}</download> / <attachment>{json}</attachment>
    // markers (internal/httpapi/agent_loop.go's appendDownloadMarkers,
    // internal/httpapi/attachments.go's appendAttachmentMarker) before
    // display — otherwise a past conversation containing a download or
    // upload would show the raw marker/content-block text verbatim instead
    // of a chip (see downloads.js).
    let content = m.content;
    let downloads = [];
    let attachments = [];
    if (m.role === "assistant") {
      const extracted = extractDownloadBlocks(content);
      content = extracted.displayText;
      downloads = extracted.downloads;
    } else if (m.role === "user") {
      const extracted = extractAttachmentBlocks(content);
      content = extracted.displayText;
      attachments = extracted.attachments;
    }

    // Content is untrusted user/assistant text — rendered via renderInlineText
    // (textContent/createTextNode only, never interpolated into innerHTML), so
    // it can never be interpreted as markup beyond the one backtick-code-span
    // construct that function explicitly handles.
    const contentSpan = document.createElement("span");
    contentSpan.className = "min-w-0 flex-1 whitespace-pre-wrap text-(--color-text-muted)";
    renderInlineText(contentSpan, content);

    row.append(roleSpan, contentSpan);
    line.appendChild(row);
    for (const { fileId, filename, sizeBytes } of downloads) {
      line.appendChild(downloadChip(fileId, filename, sizeBytes));
    }
    for (const { filename, sizeBytes } of attachments) {
      line.appendChild(attachmentChip(filename, sizeBytes ?? null));
    }
    container.appendChild(line);
  }
}

function statusBadge(conv) {
  if (conv.ended_at) return "";
  return `<span class="inline-flex items-center gap-1.5 rounded-full bg-(--color-warning-bg)/10 px-2 py-0.5 text-xs font-medium text-(--color-warning-text)">
    <span class="h-1.5 w-1.5 rounded-full bg-(--color-warning-dot)"></span>${t("history_open_badge", "open")}
  </span>`;
}

async function toggle(conv, card, chevron, detail) {
  const willOpen = detail.classList.contains("hidden");
  card.setAttribute("aria-expanded", String(willOpen));
  chevron.style.transform = willOpen ? "rotate(90deg)" : "rotate(0deg)";

  if (detail.dataset.loaded === "true") {
    detail.classList.toggle("hidden", !willOpen);
    return;
  }
  detail.classList.remove("hidden");
  detail.innerHTML = `<p class="py-2 text-xs text-(--color-text-faint)">${t("loading_ellipsis", "Loading…")}</p>`;
  try {
    const res = await fetch(`/api/dialogs/${encodeURIComponent(conv.id)}`);
    if (!res.ok) throw new Error(String(res.status));
    const messages = await res.json();
    renderMessages(detail, messages);
    detail.dataset.loaded = "true";
  } catch (err) {
    detail.innerHTML = `<p class="py-2 text-xs text-(--color-danger-text)"></p>`;
    detail.querySelector("p").textContent = `${t("failed_to_load", "Failed to load:")} ${err}`;
  }
}

function conversationCard(conv) {
  const item = document.createElement("div");
  item.className =
    "rounded-xl border border-(--color-border) bg-(--color-surface)/40 transition-colors hover:border-(--color-border-strong)";

  const card = document.createElement("button");
  card.type = "button";
  card.setAttribute("aria-expanded", "false");
  card.className =
    "flex w-full items-start justify-between gap-3 px-4 py-3.5 text-left focus-visible:outline-none sm:px-5 sm:py-4";

  const detailId = `history-detail-${conv.id}`;
  card.setAttribute("aria-controls", detailId);
  // conv.summary is model-generated text that may echo arbitrary
  // user-supplied content, so it's set via textContent below rather than
  // interpolated into this innerHTML template — everything else here
  // (formatted date, a fixed set of source labels, our own badge markup)
  // is derived/controlled, not raw conversation content.
  card.innerHTML = `
    <div class="min-w-0 flex-1">
      <div class="flex flex-wrap items-center gap-2">
        <span class="text-sm font-medium text-(--color-text)">${formatDate(conv.started_at)}</span>
        ${statusBadge(conv)}
        <span class="text-xs text-(--color-text-faint)">${sourceLabel(conv.source)}</span>
      </div>
      <p class="summary mt-1.5 hidden truncate text-sm text-(--color-text-faint)"></p>
    </div>
    <span class="chevron mt-1 shrink-0 text-(--color-text-faint) transition-transform duration-200">${icon("chevron-right", "h-4 w-4")}</span>`;

  if (conv.summary) {
    const summaryEl = card.querySelector(".summary");
    summaryEl.textContent = conv.summary;
    summaryEl.classList.remove("hidden");
  }

  const detail = document.createElement("div");
  detail.id = detailId;
  detail.className = "hidden border-t border-(--color-border)/70 px-4 pb-3.5 pt-1 sm:px-5";

  const chevron = card.querySelector(".chevron");
  card.addEventListener("click", () => toggle(conv, card, chevron, detail));

  item.append(card, detail);
  return item;
}

function skeletonCard() {
  const item = document.createElement("div");
  item.className = "rounded-xl border border-(--color-border) bg-(--color-surface)/40 px-4 py-4 sm:px-5";
  item.innerHTML = `
    <div class="skeleton animate-shimmer h-4 w-40 rounded"></div>
    <div class="skeleton animate-shimmer mt-2.5 h-3 w-64 rounded"></div>`;
  return item;
}

function emptyState() {
  return `
    <div class="flex flex-col items-center gap-3 px-6 py-20 text-center">
      <span class="flex h-12 w-12 items-center justify-center rounded-full bg-(--color-surface-2) text-(--color-text-faint)">${icon("inbox", "h-5 w-5")}</span>
      <div class="space-y-1">
        <p class="text-sm font-medium text-(--color-text-muted)">${t("no_conversations", "No conversations yet.")}</p>
        <p class="max-w-xs text-sm text-(--color-text-faint)">${t("history_empty_subtitle", "Conversations with Miranda will show up here.")}</p>
      </div>
    </div>`;
}

function errorState(message, onRetry) {
  const wrap = document.createElement("div");
  wrap.className =
    "flex flex-col items-center gap-3 rounded-xl border border-(--color-danger-border) bg-(--color-danger-bg) px-6 py-12 text-center";
  wrap.innerHTML = `
    <span class="text-(--color-danger-icon)">${icon("alert-circle", "h-6 w-6")}</span>
    <p class="message-text text-sm text-(--color-danger-text)"></p>`;
  wrap.querySelector(".message-text").textContent = `${t("failed_to_load", "Failed to load:")} ${message}`;
  const retry = document.createElement("button");
  retry.type = "button";
  retry.className =
    "rounded-lg border border-(--color-danger-border) px-3 py-1.5 text-xs font-medium text-(--color-danger-text) transition-colors hover:bg-(--color-danger-bg) focus-visible:outline-none";
  retry.textContent = t("retry_button", "Try again");
  retry.addEventListener("click", onRetry);
  wrap.appendChild(retry);
  return wrap;
}

async function load() {
  listEl.innerHTML = "";
  for (let i = 0; i < 4; i++) listEl.appendChild(skeletonCard());

  try {
    const res = await fetch("/api/dialogs?limit=10");
    if (!res.ok) throw new Error(String(res.status));
    const conversations = await res.json();
    listEl.innerHTML = "";
    if (!conversations || conversations.length === 0) {
      listEl.innerHTML = emptyState();
      return;
    }
    for (const conv of conversations) listEl.appendChild(conversationCard(conv));
  } catch (err) {
    listEl.innerHTML = "";
    listEl.appendChild(errorState(String(err), load));
  }
}

export function mount(container) {
  container.innerHTML = `
    <div class="scrollbar-thin h-full overflow-y-auto">
      <div class="mx-auto max-w-3xl px-4 py-6 sm:px-6 sm:py-8">
        <h1 class="text-2xl font-semibold tracking-tight text-(--color-text)">${t("dialog_history_title", "Dialog history")}</h1>
        <div id="history-list" class="mt-6 space-y-2.5"></div>
      </div>
    </div>`;
  listEl = container.querySelector("#history-list");
  load();
}

export function unmount() {}
