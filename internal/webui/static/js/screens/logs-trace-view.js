// Rendering half of the Logs screen's "LLM trace" tab: turns one parsed call
// record (see logs-trace-parser.js) into a collapsed summary row that
// expands, on click, into a foldable JSON tree for its request/response —
// see logs.js for how these rows get built and appended.
import { t } from "../i18n.js";
import { icon } from "../icons.js";
import JSONFormatter from "../vendor/json-formatter-js/json-formatter.mjs";

// 0 = every node starts fully collapsed (see json-formatter-js's `open`
// constructor argument) — the whole point of this screen is to show the
// list of calls first and let a human drill into just the one field they
// need, not re-dump the same wall of JSON one level down.
const JSON_OPEN_DEPTH = 0;

// Gives each row's detail panel a unique id for aria-controls, since a
// traced call has no id of its own to key off (unlike, say,
// screens/history.js's conversation id) — a simple per-page counter is
// enough, it just needs to be unique among rows rendered in this tab.
let nextRowID = 0;

/** Formats an RFC3339 timestamp for display, falling back to the raw string
 * if it doesn't parse (defensive — this is server-generated, but a log
 * viewer should never itself throw on unexpected input). */
function formatTimestamp(iso) {
  try {
    return new Date(iso).toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
  } catch {
    return iso;
  }
}

/** Builds a foldable tree for `value` via the vendored json-formatter-js.
 * Colors come entirely from the `.trace-json` CSS override block in
 * static/css/input.css — see that file for why an override is needed at
 * all (the vendored script self-injects its own hardcoded, light-only
 * stylesheet on import). */
function buildJsonTree(value) {
  return new JSONFormatter(value, JSON_OPEN_DEPTH).render();
}

/** A muted `<pre>` fallback for content that isn't valid JSON (a parse
 * failure, or an empty section) — still readable, just not foldable. */
function plainTextBlock(text) {
  const pre = document.createElement("pre");
  pre.className = "whitespace-pre-wrap break-words font-mono text-xs leading-relaxed text-(--color-text-muted)";
  pre.textContent = text;
  return pre;
}

/** One labeled "Request"/"Response" subsection inside an expanded row. */
function buildSection(label, contentEl) {
  const wrap = document.createElement("div");
  const heading = document.createElement("h3");
  heading.className = "text-xs font-semibold uppercase tracking-wide text-(--color-text-faint)";
  heading.textContent = label;
  const body = document.createElement("div");
  body.className =
    "trace-json scrollbar-thin mt-2 max-h-96 overflow-auto rounded-lg border border-(--color-border) bg-(--color-bg)/60 p-3";
  body.appendChild(contentEl);
  wrap.append(heading, body);
  return wrap;
}

/** Builds the expanded detail panel for `block` — constructed lazily, once,
 * on first expand (see buildTraceRow) rather than eagerly for every row, so
 * a screen full of collapsed calls doesn't pay for building potentially
 * thousands of DOM nodes for trees nobody opens. */
function buildDetail(block) {
  const detail = document.createElement("div");
  detail.className = "space-y-4 border-t border-(--color-border)/70 px-4 py-4 sm:px-5";

  const requestContent =
    block.requestJson !== null
      ? buildJsonTree(block.requestJson)
      : plainTextBlock(block.requestRaw || t("logs_trace_empty", "(empty)"));
  detail.appendChild(buildSection(t("logs_trace_request", "Request"), requestContent));

  let responseContent;
  if (block.isError) {
    const p = document.createElement("p");
    p.className = "whitespace-pre-wrap font-mono text-xs leading-relaxed text-(--color-danger-text)";
    p.textContent = block.errorText;
    responseContent = p;
  } else if (block.responseJson !== null) {
    responseContent = buildJsonTree(block.responseJson);
  } else {
    responseContent = plainTextBlock(block.responseRaw || t("logs_trace_empty", "(empty)"));
  }
  detail.appendChild(buildSection(t("logs_trace_response", "Response"), responseContent));

  return detail;
}

/**
 * Builds one collapsed-by-default call-summary row for `block`. Clicking it
 * lazily builds (once) and toggles the detail panel — mirrors the same
 * lazy-load-on-first-expand pattern screens/history.js already uses for its
 * conversation cards.
 */
export function buildTraceRow(block) {
  const item = document.createElement("div");
  item.className =
    "rounded-xl border border-(--color-border) bg-(--color-surface)/40 transition-colors hover:border-(--color-border-strong)";

  const detailId = `trace-detail-${nextRowID++}`;
  const header = document.createElement("button");
  header.type = "button";
  header.setAttribute("aria-expanded", "false");
  header.setAttribute("aria-controls", detailId);
  header.className =
    "flex w-full items-center justify-between gap-3 px-4 py-3 text-left focus-visible:outline-none sm:px-5";

  // Model, when present, is a genuinely useful scanning aid (which call was
  // this?) beyond what was strictly asked for — both providers' native
  // request shapes (anthropic.MessageNewParams, OpenAI's
  // ChatCompletionNewParams) carry a top-level "model" field.
  const model = typeof block.requestJson?.model === "string" ? block.requestJson.model : null;

  header.innerHTML = `
    <div class="flex min-w-0 flex-1 flex-wrap items-center gap-x-3 gap-y-1">
      <span class="ts shrink-0 font-mono text-xs text-(--color-text-faint)"></span>
      <span class="provider shrink-0 rounded-full bg-(--color-surface-2) px-2 py-0.5 text-xs font-medium text-(--color-text)"></span>
      ${model ? `<span class="model min-w-0 truncate font-mono text-xs text-(--color-text-muted)"></span>` : ""}
      ${block.conversationId ? `<span class="conv min-w-0 truncate font-mono text-xs text-(--color-text-faint)"></span>` : ""}
      ${
        block.isError
          ? `<span class="inline-flex shrink-0 items-center gap-1 rounded-full bg-(--color-danger-bg) px-2 py-0.5 text-xs font-medium text-(--color-danger-text)">${icon("alert-circle", "h-3 w-3")}${t("logs_trace_error_badge", "error")}</span>`
          : ""
      }
    </div>
    <span class="chevron shrink-0 text-(--color-text-faint) transition-transform duration-200">${icon("chevron-right", "h-4 w-4")}</span>`;

  // Timestamp/provider/model/conversation id are all server-controlled
  // strings (a formatted date, a configured provider name, a model name, a
  // history-generated conversation id) rather than raw user/model text, but
  // they're still set via textContent rather than interpolated into the
  // innerHTML template above, matching this codebase's existing convention
  // (see screens/history.js) of never trusting *any* dynamic string enough
  // to skip it.
  header.querySelector(".ts").textContent = formatTimestamp(block.timestamp);
  header.querySelector(".provider").textContent = block.provider;
  if (model) header.querySelector(".model").textContent = model;
  if (block.conversationId) header.querySelector(".conv").textContent = `conversation=${block.conversationId}`;

  const chevron = header.querySelector(".chevron");
  let detail = null; // lazily built on first expand — see buildDetail's doc comment

  header.addEventListener("click", () => {
    const willOpen = header.getAttribute("aria-expanded") !== "true";
    header.setAttribute("aria-expanded", String(willOpen));
    chevron.style.transform = willOpen ? "rotate(90deg)" : "rotate(0deg)";

    if (!detail) {
      detail = buildDetail(block);
      detail.id = detailId;
      detail.classList.add("hidden");
      item.appendChild(detail);
    }
    detail.classList.toggle("hidden", !willOpen);
  });

  item.appendChild(header);
  return item;
}
