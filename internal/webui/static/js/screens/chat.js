// Messenger-style chat screen: restores the user's current open
// conversation (if any) as history, then lets them keep talking to
// Miranda — same POST /api/v1/input the debug box always used, just
// rendered as a proper message thread instead of a single reply box.
import { t } from "../i18n.js";
import { icon, iconNode } from "../icons.js";
import * as chatWs from "../chat-ws.js";
import * as chatTurnStatus from "../chat-turn-status.js";
import { downloadChip, extractAttachmentBlocks, attachmentChip } from "../downloads.js";
import { renderInlineText } from "../inline-text.js";
import { isChatBubble } from "../message-filter.js";
import { html, render, nothing } from "../vendor/lit-html/lit-html.mjs";
import { blocksTemplate } from "../segment-template.js";

let messagesEl, scrollEl, formEl, textEl, sendBtn, fileInput, attachBtn, attachChip;
let unsubscribeWs, unsubscribeReconnect, unsubscribeTurnStatus;
// Non-null while a file upload XHR is in flight — aborted by clearAttachment().
let currentUploadXHR = null;

// True while a POST /api/v1/input fetch is in flight. Survives unmount/remount
// so loadHistory() can restore the typing indicator when the user navigates
// away and back to the chat screen during a long response.
let isSending = false;

// The current typing indicator DOM node, or null. Module-level so clearMessages()
// can null it out when the DOM is wiped, and loadHistory() can re-add it if
// isSending is still true when history finishes loading.
let thinkingEl = null;

// Holds the metadata of the file that was selected and successfully uploaded,
// ready to attach to the next send(). Cleared after a successful send or when
// the user removes the chip.
let pendingAttachment = null; // {file_id, filename, mime_type, size_bytes}

// message id (real history.Message.ID, or a "local:" temp key for a
// not-yet-confirmed optimistic bubble) -> its rendered DOM node. This is
// what lets a ChatEvent arriving over chat-ws.js — for a turn this tab
// itself triggered, or one that arrived via HA/Telegram/another tab — be
// rendered exactly once no matter which of the two (HTTP response, WS
// event) gets there first; see CLAUDE.md's "Session ownership".
const rendered = new Map();

// The merge group currently at the tail of the thread — { role, minuteKey,
// wrap, pill, timeSpan } — or null when the thread is empty or the last
// bubble isn't eligible to receive more lines (see renderChatMessage()).
// Consulted every time a new message is about to be rendered, from any of
// the three paths that add one (loadHistory's batch render, upsertMessage's
// live WS events, send()'s own optimistic bubbles): a same-sender message
// landing in the same clock minute is folded into this bubble as an extra
// line instead of starting a new one, so a burst of back-to-back
// tool-narration messages collapses into one bubble with a single trailing
// timestamp rather than one noisy near-identical bubble per message.
let lastGroup = null;

// The `rendered` key of the current turn's own optimistic user bubble,
// while it's still waiting for its real id — set by send(), consumed by
// upsertMessage() if the WS event for that exact message arrives before
// the HTTP response does (Orchestrator.Handle publishes it right after
// AppendMessage, well before the agent loop — and thus the response —
// finishes), so the WS path re-keys the *same* bubble instead of creating
// a duplicate one next to it. Only one send() can be in flight at a time
// (the composer is disabled while sending), so a single slot is enough.
let pendingUserKey = null;

// Bumped every time clearMessages() runs (conversation_deleted/ended, or a
// fresh loadHistory()) so a send() whose thread was wiped out from under it
// mid-flight — because its own turn ended/forgot the conversation, or the
// screen remounted — knows not to append a now-orphaned reply into an
// empty-state thread; see send()'s use of it below.
let renderGeneration = 0;

// Resolver callbacks waiting on notifyReplyArrivedViaWS() — see
// waitForReplyOrTurnEnd's doc comment on what this guards.
let replyArrivedResolvers = [];

// When Miranda's "thinking" indicator is showing, the wall-clock time the
// turn it represents actually started — either this tab's own send() (set
// when isSending flips true) or, when onTurnStatusChange picks up a turn
// started elsewhere (HA/Telegram/another tab), the server-reported
// TurnTracker start time. Read by showThinking() to drive the elapsed-time
// display; null whenever nothing is in flight.
let activeTurnStartedAt = null;

// Called by upsertMessage whenever it freshly appends an assistant message
// (i.e. one this tab didn't already know about) — send()'s catch block
// races this against waitForReplyOrTurnEnd's turn-status signal to tell a
// genuine failed turn apart from a fetch() that merely died on this end.
function notifyReplyArrivedViaWS() {
  const resolvers = replyArrivedResolvers;
  replyArrivedResolvers = [];
  for (const resolve of resolvers) resolve(true);
}

/** Resolves true the instant notifyReplyArrivedViaWS() fires (the fast
 * path — a healthy WS delivered the actual reply, already rendered by
 * upsertMessage), or false once the server confirms — via a live WS
 * turn_ended/turn_in_progress push, or ~5s REST polling
 * (chat-turn-status.js) when the WS itself is down — that no turn is in
 * flight for this user, without ever having delivered a WS reply.
 *
 * Exists because a fetch() rejection (e.g. Safari's "NetworkError when
 * attempting to fetch resource", thrown when the tab backgrounds or the
 * network interface changes mid-request on mobile) only means *this tab's*
 * connection died — the server can still finish the turn and broadcast the
 * reply over chat-ws.js later (see the 2026-08-20 incident this guards: a
 * file uploaded and was processed correctly, but the user still saw a
 * "request failed" error from a fixed 10s timeout that was wrong for a
 * legitimately long turn). Unlike that old fixed timeout, this is bounded
 * by the server's actual state, not a guess — the only timeout left is a
 * generous last-resort backstop against a truly wedged loop, comfortably
 * past Orchestrator.Handle's own 5-minute TurnTimeout. */
function waitForReplyOrTurnEnd() {
  return new Promise((resolve) => {
    let settled = false;
    const finish = (v) => {
      if (settled) return;
      settled = true;
      clearTimeout(backstop);
      unsubscribeStatus();
      resolve(v);
    };

    replyArrivedResolvers.push(finish);

    const onStatus = (status) => {
      // `confirmed` guards against chat-turn-status.js's pre-first-signal
      // default (inProgress:false before anything real has arrived), which
      // would otherwise be indistinguishable from a genuine "not running".
      if (status.confirmed && !status.inProgress) finish(false);
    };
    const unsubscribeStatus = chatTurnStatus.onStatusChange(onStatus);
    onStatus(chatTurnStatus.currentStatus());
    chatTurnStatus.startPolling();

    const backstop = setTimeout(() => finish(false), 10 * 60 * 1000);
  });
}

function formatTime(iso) {
  try {
    return new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  } catch {
    return "";
  }
}

/** Groups messages for merge purposes: same calendar minute in the
 * viewer's local time (matching what formatTime() actually displays, so
 * two messages that render the same "20:18" always group together and
 * never split across a UTC-boundary quirk). Returns null for a missing or
 * unparsable timestamp — treated as "never merges" by renderChatMessage(),
 * which covers the optimistic bubble send() renders before the server has
 * confirmed a real timestamp for it. */
function minuteKey(iso) {
  if (!iso) return null;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return null;
  return `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}-${d.getHours()}-${d.getMinutes()}`;
}

/** Scale an image File down to at most maxPx on both axes and return a compact
 * JPEG data URL for an inline thumbnail. Resolves to null on any canvas error
 * so the caller can silently fall back to a chip-only render. */
function thumbnailDataURL(file, maxPx) {
  return new Promise((resolve) => {
    const img = new Image();
    const objURL = URL.createObjectURL(file);
    img.onload = () => {
      URL.revokeObjectURL(objURL);
      const scale = Math.min(maxPx / img.naturalWidth, maxPx / img.naturalHeight, 1);
      const w = Math.round(img.naturalWidth * scale);
      const h = Math.round(img.naturalHeight * scale);
      const canvas = document.createElement("canvas");
      canvas.width = w;
      canvas.height = h;
      canvas.getContext("2d").drawImage(img, 0, 0, w, h);
      resolve(canvas.toDataURL("image/jpeg", 0.8));
    };
    img.onerror = () => { URL.revokeObjectURL(objURL); resolve(null); };
    img.src = objURL;
  });
}

/** Sentinel thrown when the user cancels an in-flight upload via clearAttachment(). */
class UploadCancelledError extends Error {}

/** POST a blob to /api/upload under filename via XHR so we can track
 * byte-level progress. Calls onProgress(0..100) as data is sent; resolves
 * with the parsed JSON response on success. Rejects with
 * UploadCancelledError on abort (user clicked ×), or a plain Error on
 * network failure / non-2xx status.
 *
 * Takes a Blob rather than the original File deliberately — see
 * handleFileSelected's doc comment on why a retry must never hand Safari
 * the same File object a second time. */
function uploadWithProgress(blob, filename, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    currentUploadXHR = xhr;
    xhr.open("POST", "/api/upload");
    xhr.upload.addEventListener("progress", (e) => {
      if (e.lengthComputable) onProgress(Math.round((e.loaded / e.total) * 100));
    });
    xhr.addEventListener("load", () => {
      currentUploadXHR = null;
      if (xhr.status >= 200 && xhr.status < 300) {
        try { resolve(JSON.parse(xhr.responseText)); }
        catch { reject(new Error("Invalid JSON response")); }
      } else {
        reject(new Error(xhr.responseText || String(xhr.status)));
      }
    });
    xhr.addEventListener("error", () => { currentUploadXHR = null; reject(new Error("Network error")); });
    xhr.addEventListener("abort", () => { currentUploadXHR = null; reject(new UploadCancelledError()); });
    const formData = new FormData();
    formData.append("file", blob, filename);
    xhr.send(formData);
  });
}

/** downloads is the message's own structured file list (history.Message.Downloads /
 * InputResponse.Downloads / ChatEvent.Message.Downloads) — an array of
 * {file_id, filename, size_bytes, mime_type}, never parsed out of text.
 *
 * blocks is the server-parsed markdown-lite structure for this message
 * (internal/replyformat's Block[] — see webui's messageView.Blocks /
 * InputResponse.Blocks / ChatEvent.Blocks). Only ever set for an assistant
 * message; when present and non-empty it's rendered via
 * segment-template.js's blocksTemplate. Every other case — user messages
 * (which never get server-computed blocks) and the rare WS-race edge case
 * where a live event's blocks haven't arrived yet — falls back to the
 * original renderInlineText() (backtick-only) path, unchanged. */
function bubble(role, text, timeIso, downloads, blocks) {
  const wrap = document.createElement("div");
  wrap.className = `flex flex-col ${role === "user" ? "items-end" : "items-start"}`;

  let displayText = text;
  const attachmentNodes = [];
  if (role === "user") {
    const { displayText: dt, attachments } = extractAttachmentBlocks(text);
    displayText = dt;
    for (const { filename, sizeBytes } of attachments) {
      attachmentNodes.push(attachmentChip(filename, sizeBytes ?? null));
    }
  }

  // `pill` — the rounded, colored content box — and `timeSpan` are built as
  // plain nodes first, then interpolated as Nodes into the outer template
  // below (lit-html accepts a real DOM Node as a child value directly), and
  // returned to the caller alongside `wrap`: renderChatMessage() holds onto
  // these references so a later same-sender, same-minute message can be
  // folded into this exact bubble via appendBubbleLine()/appendBubbleContent()
  // instead of starting a whole new one.
  let textNode = nothing;
  let pill = null;
  if (displayText) {
    pill = document.createElement("div");
    pill.className =
      role === "user"
        ? // space-y-2: an 8px gap between this pill's line(s) — either
          // multiple <p>/<ul> from a single message's blocksTemplate, or
          // multiple merged messages' own line wrappers (see
          // appendBubbleContent) — harmless no-op for a single plain-text
          // line, whose only element children (<code> spans) are inline.
          "max-w-[85%] whitespace-pre-wrap rounded-2xl rounded-br-md bg-(--color-accent) px-4 py-2.5 text-sm leading-relaxed text-white sm:max-w-[75%] space-y-2"
        : "max-w-[85%] whitespace-pre-wrap rounded-2xl rounded-bl-md border border-(--color-border) bg-(--color-surface)/70 px-4 py-2.5 text-sm leading-relaxed text-(--color-text) sm:max-w-[75%] space-y-2";
    appendBubbleContent(pill, role, displayText, blocks);
    textNode = pill;
  }

  const downloadNodes = (downloads ?? []).map(({ file_id, filename, size_bytes }) =>
    downloadChip(file_id, filename, size_bytes ?? null)
  );

  let timeSpan = null;
  let timeNode = nothing;
  if (timeIso) {
    timeSpan = document.createElement("span");
    timeSpan.className = "mt-1 px-1 text-xs text-(--color-text-muted)";
    timeSpan.textContent = formatTime(timeIso);
    timeNode = timeSpan;
  }

  render(html`${attachmentNodes}${textNode}${downloadNodes}${timeNode}`, wrap);

  return { wrap, pill, timeSpan };
}

/** Renders one message's text as a new line inside `pill` — either the
 * parsed block structure (blocksTemplate) or the plain-text/backtick
 * fallback (renderInlineText). Shared by bubble()'s first line and
 * appendBubbleLine()'s merged lines, so every line inside a bubble (whether
 * it's a single-message bubble or a merged one) goes through the exact same
 * rendering path. Each line gets its own space-y-2 wrapper so a single
 * message's own multi-block content (e.g. several <p>/<ul> from
 * blocksTemplate) keeps its internal spacing once nested a level deeper
 * than pill's own space-y-2 (which now spaces *lines* apart, not blocks). */
function appendBubbleContent(pill, role, displayText, blocks) {
  const line = document.createElement("div");
  line.className = "space-y-2";
  if (blocks && blocks.length > 0) {
    render(blocksTemplate(blocks), line);
  } else if (role === "user") {
    // renderInlineText()'s default <code> style (bg-(--color-surface-2)) is
    // tuned for a neutral surface and is nearly invisible against this
    // bubble's solid bg-(--color-accent) blue — a translucent white
    // treatment instead, same idea as attachmentChip's own blue-bubble
    // variant in downloads.js. No text-color override: inherits the
    // bubble's text-white.
    renderInlineText(line, displayText, "rounded border border-white/20 bg-white/15 px-1 py-0.5 font-mono text-[0.85em] break-all");
  } else {
    renderInlineText(line, displayText);
  }
  pill.appendChild(line);
}

/** True when a message is eligible to merge with (or receive a merge from)
 * a neighboring same-sender message — i.e. it's plain text with no file
 * download/attachment riding along. A message carrying a downloadChip or
 * attachmentChip gets its own standalone bubble instead, so that chip stays
 * visually tied to the one message it belongs to rather than floating
 * inside a merged multi-message bubble. */
function isMergeableCandidate(role, text, downloads) {
  if (!text) return false;
  if ((downloads?.length ?? 0) > 0) return false;
  if (role === "user" && text.includes("<attachment>")) return false;
  return true;
}

/** Folds another message's text into `group`'s already-rendered pill as an
 * additional line. Returns false (nothing rendered) if the group has no
 * pill to append to, or if the message turns out to have no visible text
 * once a user message's attachment marker is stripped — in either case the
 * caller falls back to a fresh standalone bubble. */
function appendBubbleLine(group, role, text, blocks) {
  if (!group.pill) return false;
  const displayText = role === "user" ? extractAttachmentBlocks(text).displayText : text;
  if (!displayText) return false;
  appendBubbleContent(group.pill, role, displayText, blocks);
  return true;
}

/**
 * Single entry point for adding one message to the visible thread — used by
 * loadHistory()'s initial batch render, upsertMessage()'s live WS events,
 * and send()'s own optimistic bubbles, so all three share the same grouping
 * behavior instead of three copies of it.
 *
 * When the message is plain text (isMergeableCandidate) and lands in the
 * same clock minute as the bubble currently at the tail of the thread, from
 * the same sender, it's folded into that bubble as an extra line instead of
 * starting a new one — this is what collapses a burst of back-to-back
 * tool-narration messages ("Загружу инструменты для учёта питания." /
 * "Найду нужные продукты в базе." / ...) into a single bubble with one
 * trailing timestamp, instead of one near-identical bubble per message.
 *
 * id (when given) is history.Message.ID or a "local:" temp key — recorded
 * in `rendered` against whichever DOM node ends up representing it (a fresh
 * bubble, or a shared merged one), so the existing per-id dedupe in
 * upsertMessage()/send() keeps working even though several ids can now
 * point at the same node.
 */
function renderChatMessage(role, text, timeIso, downloads, blocks, id) {
  const key = minuteKey(timeIso);
  const mergeable = isMergeableCandidate(role, text, downloads);

  if (mergeable && key !== null && lastGroup && lastGroup.role === role && lastGroup.minuteKey === key) {
    if (appendBubbleLine(lastGroup, role, text, blocks)) {
      // Reflects the newest message's time, not the group's first — the
      // one timestamp left visible should describe when the bubble's
      // content was last added to, matching how a person reads it.
      if (lastGroup.timeSpan) lastGroup.timeSpan.textContent = formatTime(timeIso);
      if (id != null) rendered.set(id, lastGroup.wrap);
      return lastGroup.wrap;
    }
  }

  const group = bubble(role, text, timeIso, downloads, blocks);
  messagesEl.appendChild(group.wrap);
  if (id != null) rendered.set(id, group.wrap);
  lastGroup = mergeable && key !== null && group.pill ? { role, minuteKey: key, ...group } : null;
  return group.wrap;
}

/** The three-dot "Miranda is thinking" indicator shown while a request is in
 * flight. Includes a data-elapsed span, hidden until updateElapsedDisplay()
 * (driven by activeTurnStartedAt) fills it in — only once a turn has run
 * long enough that a bare spinner becomes ambiguous about whether it's
 * stuck or just slow. */
function typingIndicator() {
  const wrap = document.createElement("div");
  wrap.className = "flex items-start";
  render(
    html`
      <div class="flex items-center gap-2 rounded-2xl rounded-bl-md border border-(--color-border) bg-(--color-surface)/70 px-4 py-3">
        <span class="flex items-center gap-1">
          <span class="h-1.5 w-1.5 animate-bounce rounded-full bg-(--color-text-faint) [animation-delay:-0.3s]"></span>
          <span class="h-1.5 w-1.5 animate-bounce rounded-full bg-(--color-text-faint) [animation-delay:-0.15s]"></span>
          <span class="h-1.5 w-1.5 animate-bounce rounded-full bg-(--color-text-faint)"></span>
        </span>
        <span data-elapsed class="hidden text-xs text-(--color-text-faint)"></span>
      </div>
    `,
    wrap
  );
  return wrap;
}

// A quick reply shouldn't show a stopwatch — only once a turn has run long
// enough that the plain three-dot bubble alone becomes ambiguous about
// whether it's stuck (tool calls, web search, escalation can legitimately
// take a while; see Orchestrator.Handle's 5-minute TurnTimeout).
const ELAPSED_GRACE_MS = 6000;
let elapsedTimer = null;

function formatElapsed(ms) {
  const totalSec = Math.max(0, Math.floor(ms / 1000));
  const m = Math.floor(totalSec / 60);
  const s = totalSec % 60;
  return m > 0 ? `${m}м ${s}с` : `${s}с`;
}

function updateElapsedDisplay() {
  const span = thinkingEl?.querySelector("[data-elapsed]");
  if (!span || !activeTurnStartedAt) return;
  const elapsedMs = Date.now() - activeTurnStartedAt.getTime();
  if (elapsedMs < ELAPSED_GRACE_MS) {
    span.classList.add("hidden");
    return;
  }
  span.textContent = `${t("chat_waiting_elapsed", "Waiting")} — ${formatElapsed(elapsedMs)}`;
  span.classList.remove("hidden");
}

function showThinking() {
  if (!thinkingEl) thinkingEl = typingIndicator();
  // Unconditional, even when reusing an already-shown indicator:
  // appendChild on a node that already has a parent *moves* it (DOM spec),
  // so this guarantees thinkingEl is always the last child — otherwise a
  // stale indicator left over from onTurnStatusChange (shown before the
  // user's own new message existed) would sit frozen above a newly
  // appended bubble instead of trailing it.
  messagesEl.appendChild(thinkingEl);
  scrollToBottom();
  activeTurnStartedAt ??= new Date();
  clearInterval(elapsedTimer);
  updateElapsedDisplay();
  elapsedTimer = setInterval(updateElapsedDisplay, 1000);
}

function hideThinking() {
  thinkingEl?.remove();
  thinkingEl = null;
  activeTurnStartedAt = null;
  clearInterval(elapsedTimer);
  elapsedTimer = null;
}

/** A visually distinct error bubble with an inline retry action — errors
 * must never look like just another assistant message (persona doc: "be
 * visually distinguishable" + "provide recovery action"). */
function errorBubble(detail, onRetry) {
  const wrap = document.createElement("div");
  wrap.className = "flex flex-col items-start gap-2";
  render(
    html`
      <div class="flex max-w-[85%] items-start gap-2 rounded-2xl rounded-bl-md border border-(--color-danger-border) bg-(--color-danger-bg) px-4 py-2.5 text-sm text-(--color-danger-text) sm:max-w-[75%]">
        ${iconNode("alert-circle", "mt-0.5 h-4 w-4 shrink-0")}
        <!-- detail wraps a caught exception's message or an HTTP status —
             a plain lit-html child expression, auto-escaped exactly like
             the textContent-only assignment this replaces, so it can
             never be interpreted as markup no matter what a failing
             request or an unusual Error object happens to stringify to. -->
        <span>${t("request_failed", "Request failed:")} ${detail}</span>
      </div>
      <button
        type="button"
        class="ml-1 rounded-md px-2 py-1 text-xs font-medium text-(--color-text-muted) underline decoration-(--color-border-strong) underline-offset-2 transition-colors hover:text-(--color-text) focus-visible:outline-none"
        @click=${onRetry}
      >
        ${t("retry_button", "Try again")}
      </button>
    `,
    wrap
  );
  return wrap;
}

function emptyState() {
  const wrap = document.createElement("div");
  wrap.dataset.emptyState = "true";
  wrap.className = "flex flex-col items-center gap-3 px-6 py-20 text-center";
  render(
    html`
      <span class="flex h-12 w-12 items-center justify-center rounded-full bg-(--color-surface-2) text-(--color-text-faint)"
        >${iconNode("chat", "h-5 w-5")}</span
      >
      <div class="space-y-1">
        <p class="text-sm font-medium text-(--color-text-muted)">${t("chat_empty_title", "Start a conversation")}</p>
        <p class="max-w-xs text-sm text-(--color-text-faint)">
          ${t("chat_empty_subtitle", "Ask Miranda anything — she remembers context across the conversation.")}
        </p>
      </div>
    `,
    wrap
  );
  return wrap;
}

function skeleton() {
  const wrap = document.createElement("div");
  wrap.className = "flex flex-col gap-4";
  render(
    html`
      <div class="flex justify-start"><div class="skeleton animate-shimmer h-9 w-48 rounded-2xl"></div></div>
      <div class="flex justify-end"><div class="skeleton animate-shimmer h-9 w-32 rounded-2xl"></div></div>
      <div class="flex justify-start"><div class="skeleton animate-shimmer h-9 w-56 rounded-2xl"></div></div>
    `,
    wrap
  );
  return wrap;
}

function scrollToBottom() {
  scrollEl.scrollTop = scrollEl.scrollHeight;
}

function clearMessages() {
  messagesEl.innerHTML = "";
  rendered.clear();
  pendingUserKey = null;
  thinkingEl = null; // detached by the innerHTML wipe above
  lastGroup = null; // its bubble was just wiped along with everything else
  clearInterval(elapsedTimer);
  elapsedTimer = null;
  renderGeneration++;
}

/** Build and return the attachment chip shown below the composer while a file
 * is uploading or ready to send. The chip starts with a hidden progress bar
 * that setAttachChipProgress() activates; finalizeAttachChip() hides it and
 * optionally injects an image thumbnail once the upload completes. */
function buildAttachChip(filename) {
  const chip = document.createElement("div");
  chip.className =
    "flex w-fit flex-col gap-1.5 rounded-lg border border-(--color-border) bg-(--color-surface)/80 px-3 py-2 mt-2 text-xs text-(--color-text-muted)";

  render(
    html`
      <!-- Top row: invisible spacer | filename (centred) | × button. The
           spacer mirrors the button's width so the name is truly centred.
           data-top-row is queried by finalizeAttachChip() below to prepend
           a thumbnail once the upload completes — keep it exact. -->
      <div class="flex items-center" data-top-row="true">
        <div class="w-6 shrink-0"></div>
        <span class="min-w-0 flex-1 truncate text-center" title=${filename}>${filename}</span>
        <button
          type="button"
          class="flex w-6 shrink-0 items-center justify-end rounded p-1 text-(--color-text-faint) hover:text-(--color-danger-text) focus-visible:outline-none"
          aria-label="Remove attachment"
          @click=${clearAttachment}
        >
          ${iconNode("close", "h-4 w-4")}
        </button>
      </div>
      <!-- Progress bar — hidden until setAttachChipProgress() reveals it;
           data-progress-track is queried by setAttachChipProgress() below,
           keep it exact. -->
      <div class="h-0.5 w-full overflow-hidden rounded-full bg-(--color-border)" data-progress-track="true" hidden>
        <div class="h-full rounded-full bg-(--color-accent) transition-[width] duration-100 ease-linear" style="width: 0%"></div>
      </div>
    `,
    chip
  );

  return chip;
}

/** Show the attachment chip for the given filename above the composer form. */
function showAttachChip(filename) {
  clearAttachChip();
  attachChip = buildAttachChip(filename);
  formEl.parentElement.insertBefore(attachChip, formEl);
}

/** Remove the chip DOM node if present. */
function clearAttachChip() {
  if (attachChip) {
    attachChip.remove();
    attachChip = null;
  }
}

/** Reveal the progress bar on the current chip and set its fill to percent (0–100). */
function setAttachChipProgress(percent) {
  if (!attachChip) return;
  const track = attachChip.querySelector("[data-progress-track]");
  if (!track) return;
  track.hidden = false;
  track.firstElementChild.style.width = `${percent}%`;
}

/** Called when upload completes successfully: hide the progress bar and
 * optionally prepend a thumbnail to the top row (images only). */
function finalizeAttachChip(previewDataURL) {
  if (!attachChip) return;
  const track = attachChip.querySelector("[data-progress-track]");
  if (track) track.hidden = true;
  if (previewDataURL) {
    const topRow = attachChip.querySelector("[data-top-row]");
    const img = document.createElement("img");
    img.src = previewDataURL;
    img.alt = "";
    img.className = "h-9 w-9 shrink-0 rounded object-cover";
    topRow?.prepend(img);
  }
}

/** Discard the pending attachment, abort any in-flight upload, and remove the chip. */
function clearAttachment() {
  pendingAttachment = null;
  currentUploadXHR?.abort();
  currentUploadXHR = null;
  clearAttachChip();
  // Reset the hidden file input so the same file can be re-selected.
  if (fileInput) fileInput.value = "";
}

/** Show a spinner on the attach button while a file is uploading. */
function setUploading(uploading) {
  if (!attachBtn) return;
  attachBtn.disabled = uploading;
  attachBtn.innerHTML = uploading
    ? '<span class="h-4 w-4 animate-spin rounded-full border-2 border-(--color-text-faint)/30 border-t-(--color-text-faint)"></span>'
    : icon("paperclip", "h-4 w-4");
}

/** Handle a File object selected by the user (via button or drag-and-drop).
 *
 * Reads file's bytes into memory once, up front, and uploads a fresh Blob
 * built from those bytes rather than passing file itself to
 * uploadWithProgress — including on a retry (errorBubble's onRetry below
 * calls this same function again with the same File). iOS Safari has a
 * known WebKit bug where handing the same File object to a second
 * FormData/XHR .send() (e.g. exactly this retry path) can produce a
 * truncated multipart body server-side, surfacing as "multipart:
 * NextPart: EOF" — reproducibly, even over a reliable connection (see the
 * 2026-08-20 incident this guards). file.arrayBuffer() doesn't share that
 * bug: it either resolves with the complete contents or rejects, so
 * reading it fresh on every attempt (this attempt included) and always
 * uploading a same-sized Blob built from those bytes keeps every attempt's
 * multipart framing correct regardless of how many times this file has
 * already been sent. */
async function handleFileSelected(file) {
  if (!file) return;
  clearAttachment();
  setUploading(true);
  // Show the chip immediately so the user sees the filename while uploading.
  showAttachChip(file.name);
  setAttachChipProgress(0);
  try {
    const bytes = await file.arrayBuffer();
    const blob = new Blob([bytes], file.type ? { type: file.type } : undefined);
    const data = await uploadWithProgress(blob, file.name, setAttachChipProgress);
    pendingAttachment = {
      file_id: data.file_id,
      filename: data.filename,
      mime_type: data.mime_type,
      size_bytes: data.size_bytes,
    };
    // Generate a local thumbnail for images — scaled down so the data URL
    // stays small (200px covers 2× retina at our 100px display size).
    if (data.mime_type?.startsWith("image/")) {
      pendingAttachment.previewDataURL = await thumbnailDataURL(file, 240);
    }
    finalizeAttachChip(pendingAttachment.previewDataURL ?? null);
  } catch (err) {
    // User clicked × — chip already gone, nothing to surface.
    if (err instanceof UploadCancelledError) return;
    clearAttachment();
    messagesEl.querySelector("[data-empty-state]")?.remove();
    messagesEl.appendChild(errorBubble(String(err), () => handleFileSelected(file)));
    // An error bubble is never part of a merge group, and nothing after it
    // should be folded backwards into whatever bubble preceded it.
    lastGroup = null;
  } finally {
    setUploading(false);
    if (fileInput) fileInput.value = "";
  }
}

/** Render a message delivered live over chat-ws.js, if it isn't already on
 * screen — a no-op for an id already in `rendered` (this tab's own HTTP
 * response beat the WS event, or a duplicate delivery), and for turns
 * isChatBubble filters out.
 *
 * blocks is the ChatEvent's own `blocks` field — a *sibling* of `message`
 * on the event, not nested inside it (see onChatEvent below and bubble()'s
 * doc comment). */
function upsertMessage(message, blocks) {
  if (rendered.has(message.id) || !isChatBubble(message)) return;

  // This is the live echo of a user message this same tab is still waiting
  // on a response for — reuse its existing optimistic bubble instead of
  // adding a second one (see pendingUserKey's doc comment above).
  if (message.role === "user" && pendingUserKey !== null && rendered.has(pendingUserKey)) {
    const node = rendered.get(pendingUserKey);
    rendered.delete(pendingUserKey);
    rendered.set(message.id, node);
    pendingUserKey = null;
    return;
  }

  if (message.role === "assistant") notifyReplyArrivedViaWS();

  messagesEl.querySelector("[data-empty-state]")?.remove();
  renderChatMessage(message.role, message.content, message.created_at, message.downloads, blocks, message.id);
  scrollToBottom();
}

function onChatEvent(ev) {
  // The socket carries the raw hub.Event envelope ({source, message, data,
  // user_id} — see internal/hub.Event); the ChatEvent this screen cares
  // about is nested under `data`, not at the top level.
  const chatEvent = ev.data;
  if (!chatEvent) return;
  if (chatEvent.type === "message") {
    // chatEvent.blocks is a sibling of chatEvent.message, not nested
    // inside it — see bubble()'s doc comment on the `blocks` param.
    upsertMessage(chatEvent.message, chatEvent.blocks);
  } else if (chatEvent.type === "conversation_deleted" || chatEvent.type === "conversation_ended") {
    clearMessages();
    messagesEl.appendChild(emptyState());
  }
}

/** Reloads the visible thread from server-recorded truth. Returns the role
 * ("user"|"assistant") of the last rendered chat message, or null if the
 * thread ended up empty (no conversation, an ended one, or a fetch
 * failure) — send()'s catch block uses this as its reconciliation signal
 * when a turn ends without ever delivering a WS reply: "assistant" means
 * the turn actually succeeded (just missed by this tab's own connection),
 * "user" means it genuinely failed silently server-side. */
async function loadHistory() {
  clearMessages();
  messagesEl.appendChild(skeleton());
  let lastRole = null;
  try {
    const res = await fetch("/api/dialogs?limit=1");
    const conversations = await res.json();
    if (!conversations || conversations.length === 0 || conversations[0].ended_at) {
      clearMessages();
      messagesEl.appendChild(emptyState());
    } else {
      const msgsRes = await fetch(`/api/dialogs/${encodeURIComponent(conversations[0].id)}`);
      const messages = await msgsRes.json();
      // History interleaves plain chat turns with tool activity: role "tool"
      // holds a tool's result, and an assistant row that only requested a
      // tool call (no reply text yet) is stored with empty content — both
      // are recorded for history/logs (see internal/httpapi's
      // recordAssistantToolCallMessage/recordToolCall) but aren't part of
      // the conversation a person reads, so they must not render as bubbles.
      const chatMessages = messages.filter(isChatBubble);
      clearMessages();
      if (chatMessages.length === 0) {
        messagesEl.appendChild(emptyState());
      } else {
        for (const m of chatMessages) {
          // m.blocks is a top-level sibling of m.content/m.role/m.id on
          // each flattened message object (see webui's messageView) —
          // only present (non-empty) when m.role === "assistant".
          renderChatMessage(m.role, m.content, m.created_at, m.downloads, m.blocks, m.id);
        }
        scrollToBottom();
        lastRole = chatMessages.at(-1).role;
      }
    }
  } catch {
    // Best-effort restore only — a failed background history fetch just
    // means starting fresh, same as if there were no prior conversation.
    clearMessages();
    messagesEl.appendChild(emptyState());
  }
  // If a POST /api/v1/input request is still in flight (user navigated away
  // and back, or the WS reconnected), re-apply the disabled composer and
  // typing indicator — they were wiped when mount() rebuilt the DOM.
  // isSending is set to false in send()'s finally, so if the response
  // arrived before this point the check is a safe no-op.
  if (isSending) {
    setSending(true);
    showThinking();
  }
  return lastRole;
}

function setSending(sending) {
  textEl.disabled = sending;
  sendBtn.disabled = sending;
  sendBtn.innerHTML = sending
    ? '<span class="h-4 w-4 animate-spin rounded-full border-2 border-white/30 border-t-white"></span>'
    : icon("send", "h-4 w-4");
}

async function send(text) {
  // Identifies "this thread" for the duration of this call — bumped by
  // clearMessages() (conversation_deleted/ended arriving live, or a
  // remount). If it no longer matches once the request settles, this turn's
  // outcome either already arrived over chat-ws.js before the thread was
  // cleared, or belongs to a screen state that's since moved on — either
  // way, writing into the current (unrelated) DOM would be wrong.
  const myGeneration = renderGeneration;

  // Snapshot and clear the pending attachment before going async — if the
  // send fails the user can try again (the chip is already gone, but the
  // retry button re-calls send(text) and the attachment is lost on retry,
  // which is acceptable: the file_id in the store is still valid for the
  // TTL window, so the user can re-attach if needed).
  const attachments = pendingAttachment ? [pendingAttachment] : [];
  clearAttachment();

  messagesEl.querySelector("[data-empty-state]")?.remove();
  // Keyed under a temporary id until the response tells us the real
  // history.Message.ID — see the `rendered`/`pendingUserKey` doc comments
  // above. No timeIso yet either (assigned once the server confirms this
  // message and a reload/WS echo re-renders it with a real timestamp), so
  // renderChatMessage() never merges this bubble into anything — see
  // minuteKey()'s null-timeIso case.
  const tempKey = `local:${Date.now()}:${Math.random()}`;
  const userNode = renderChatMessage("user", text, null, null, null, tempKey);
  // Chips for the attachment aren't in `text` yet (the server injects file
  // blocks after processing) — render them immediately from the local snapshot.
  for (const att of attachments) {
    userNode.prepend(attachmentChip(att.filename, att.size_bytes ?? null, att.previewDataURL ?? null));
  }
  pendingUserKey = tempKey;
  scrollToBottom();

  isSending = true;
  showThinking();
  setSending(true);

  try {
    const body = { source: "web_ui", text };
    if (attachments.length > 0) body.attachments = attachments;
    const res = await fetch("/api/v1/input", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    hideThinking();
    if (!res.ok) {
      // Shown even if renderGeneration has since moved on (e.g. a WS
      // reconnect — network blip, laptop sleep, backgrounded tab — ran
      // loadHistory() while this request was still stuck retrying
      // upstream, which can take minutes; see internal/llm/router's
      // escalation-on-error and internal/keyrotation's retry cycles). A
      // failed turn produces no chat-ws.js reply to fall back on, so
      // dropping the error here would leave the user with no sign the
      // message was ever lost — worse than an error bubble landing in a
      // thread that's moved on.
      messagesEl.querySelector("[data-empty-state]")?.remove();
      messagesEl.appendChild(errorBubble(String(res.status), () => send(text)));
      lastGroup = null; // see handleFileSelected's identical comment above
    } else if (renderGeneration !== myGeneration) {
      // The thread this bubble belonged to is gone (cleared by a
      // conversation_deleted/ended event that arrived over chat-ws.js while
      // this request was still in flight, or a remount) — nothing left to
      // update.
    } else {
      const data = await res.json();
      if (pendingUserKey === tempKey) {
        // The WS echo of our own message hasn't arrived (or ever will,
        // e.g. it raced ahead of a conversation_deleted that already
        // cleared it) — reconcile it here instead.
        rendered.delete(tempKey);
        rendered.set(data.user_message_id, userNode);
        pendingUserKey = null;
      }
      // A chat-ws.js event for the assistant reply can race ahead of this
      // HTTP response (e.g. a slow/dropped response body) — if it already
      // rendered the bubble, don't render it a second time.
      if (!rendered.has(data.assistant_message_id)) {
        // data.blocks is already top-level, sibling of data.reply.
        renderChatMessage("assistant", data.reply, new Date().toISOString(), data.downloads, data.blocks, data.assistant_message_id);
      }
    }
  } catch (err) {
    // fetch() itself failed (network error, not an HTTP error status) —
    // this only proves *this tab's* connection died, not that the server
    // never got or finished the turn (mobile Safari in particular tears
    // down an in-flight fetch when the tab backgrounds or the network
    // interface changes, even mid-turn). Wait for the server's own
    // authoritative signal (see waitForReplyOrTurnEnd) instead of guessing
    // off a fixed timeout — the incident this guards: a file uploaded and
    // processed fine, but the user still saw a scary "request failed"
    // error because the old fixed 10s wait was too short for this turn.
    const replyArrivedViaWS = await waitForReplyOrTurnEnd();
    // The server has now authoritatively confirmed the turn is no longer in
    // flight (either the reply arrived, or turn-status flipped to false) —
    // clear isSending before reconciling below, so loadHistory() (which
    // treats isSending as "a send() is still pending, restore the
    // indicator") doesn't mistake this now-resolved wait for one and
    // re-show it. finally below re-sets the same values regardless.
    isSending = false;
    hideThinking();
    if (!replyArrivedViaWS && renderGeneration === myGeneration) {
      // The server confirmed the turn ended without ever getting a WS
      // reply through — reconcile against its actual recorded state rather
      // than assuming failure: loadHistory() re-renders the thread from
      // truth and reports whether it ended on an assistant reply (this
      // tab's connection merely missed it) or not (a genuine silent
      // server-side failure).
      const lastRole = await loadHistory();
      if (lastRole === "user") {
        messagesEl.querySelector("[data-empty-state]")?.remove();
        messagesEl.appendChild(errorBubble(String(err), () => send(text)));
        lastGroup = null; // see handleFileSelected's identical comment above
      }
    }
  } finally {
    isSending = false;
    setSending(false);
    // setSending(true) disables the textarea to block a second send, which
    // drops focus from it — without this the user has to click back into
    // the field before typing their next message.
    textEl.focus();
  }
  scrollToBottom();
}

function autoResize() {
  textEl.style.height = "auto";
  textEl.style.height = `${Math.min(textEl.scrollHeight, 160)}px`;
}

function onSubmit(e) {
  e.preventDefault();
  const text = textEl.value.trim();
  if (!text) return;
  textEl.value = "";
  autoResize();
  send(text);
}

function onKeydown(e) {
  if (e.key === "Enter" && !e.shiftKey) {
    e.preventDefault();
    formEl.requestSubmit();
  }
}

export function mount(container) {
  container.innerHTML = `
    <div class="flex h-full flex-col">
      <h1 class="sr-only">${t("nav_chat", "Chat")}</h1>
      <!-- min-h-0 here (and on every flex-column ancestor between a fixed
           header and a flex-1 overflow-y-auto pane, see screens/logs.js's
           scroll wrapper for the same rule) is load-bearing: without it a
           flex child defaults to min-height:auto and grows to fit its
           content instead of shrinking to the available space, so
           overflow-y-auto never actually triggers and the pane just gets
           clipped by an ancestor's overflow-hidden instead of scrolling. -->
      <div id="chat-scroll" class="scrollbar-thin min-h-0 flex-1 overflow-y-auto">
        <div class="mx-auto flex min-h-full max-w-3xl flex-col justify-end px-4 py-6 sm:px-6">
          <div id="chat-messages" class="flex flex-col gap-4" aria-live="polite" aria-relevant="additions"></div>
        </div>
        <!-- #chat-composer lives *inside* #chat-scroll (sticky bottom-0), not
             beside it, so it stays glued to the bottom of the scrolling
             thread no matter how far up the user has scrolled through
             history — the standard pattern for an always-visible chat
             input bar. The on-screen-keyboard gap fix lives elsewhere
             (index.html's <head> script, 'resetScroll') and is unrelated to
             this placement. backdrop-blur matches the header (see
             index.html) since this bar genuinely overlaps scrolled message
             content instead of sitting beside it. -->
        <div class="sticky bottom-0 border-t border-(--color-border) bg-(--color-bg)/80 backdrop-blur supports-[backdrop-filter]:bg-(--color-bg)/60" id="chat-composer">
          <!-- pb-[max(0.75rem,env(safe-area-inset-bottom))]: on an installed
               iOS PWA (viewport-fit=cover in index.html's <head>) the inset
               resolves to the home-indicator height instead of 0, so the
               composer never sits flush against it. max() rather than adding
               the inset on top of the form's own py-3 — summing the two would
               double up the gap on notched devices (~46px) instead of just
               using the larger of "the normal resting padding" and "the
               physical inset", which is all that's actually needed; in a
               regular browser tab (inset 0) this is identical to plain py-3. -->
          <form id="chat-form" class="mx-auto flex max-w-3xl items-end gap-2 px-4 pt-3 pb-[max(0.75rem,env(safe-area-inset-bottom))] sm:px-6">
            <!-- Hidden file input — triggered by the attach button or drag-and-drop. -->
            <input type="file" id="chat-file" class="sr-only" accept="*/*" tabindex="-1" aria-hidden="true" />
            <label for="chat-text" class="sr-only">${t("chat_placeholder", "Message Miranda…")}</label>
            <textarea id="chat-text" rows="1" placeholder="${t("chat_placeholder", "Message Miranda…")}"
              class="scrollbar-thin max-h-40 flex-1 resize-none rounded-xl border border-(--color-border-strong) bg-(--color-surface)/60 px-4 py-2.5 text-sm text-(--color-text) transition-colors placeholder:text-(--color-text-faint) hover:border-(--color-text-faint) focus:border-(--color-accent-emphasis) focus:outline-none focus-visible:outline-none"></textarea>
            <button type="button" id="chat-attach" aria-label="${t("attach_button", "Attach file")}"
              class="flex h-11 w-11 shrink-0 items-center justify-center rounded-xl border border-(--color-border-strong) bg-(--color-surface)/60 text-(--color-text-muted) transition-colors hover:border-(--color-text-faint) hover:text-(--color-text) focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-60">
              ${icon("paperclip", "h-4 w-4")}
            </button>
            <button type="submit" id="chat-send" aria-label="${t("send_button", "Send")}"
              class="flex h-11 w-11 shrink-0 items-center justify-center rounded-xl bg-(--color-accent) text-white transition-colors hover:bg-(--color-accent-hover) focus-visible:outline-none active:bg-(--color-accent-active) disabled:cursor-not-allowed disabled:opacity-60">
              ${icon("send", "h-4 w-4")}
            </button>
          </form>
          <p class="mx-auto hidden max-w-3xl px-4 pb-3 text-xs text-(--color-text-faint) sm:block sm:px-6">${t("chat_hint", "Enter to send · Shift+Enter for a new line")}</p>
        </div>
      </div>
    </div>`;

  scrollEl = container.querySelector("#chat-scroll");
  messagesEl = container.querySelector("#chat-messages");
  formEl = container.querySelector("#chat-form");
  textEl = container.querySelector("#chat-text");
  sendBtn = container.querySelector("#chat-send");
  attachBtn = container.querySelector("#chat-attach");
  fileInput = container.querySelector("#chat-file");

  formEl.addEventListener("submit", onSubmit);
  textEl.addEventListener("keydown", onKeydown);
  textEl.addEventListener("input", autoResize);

  // Attach button opens the hidden file picker.
  attachBtn.addEventListener("click", () => fileInput.click());

  // File selected via the picker.
  fileInput.addEventListener("change", () => {
    if (fileInput.files && fileInput.files[0]) {
      handleFileSelected(fileInput.files[0]);
    }
  });

  // Drag-and-drop: accept files dropped anywhere on the chat scroll area.
  // Only handle files; text/URL drops are ignored.
  scrollEl.addEventListener("dragover", (e) => {
    if (e.dataTransfer.types.includes("Files")) {
      e.preventDefault();
      e.dataTransfer.dropEffect = "copy";
    }
  });
  scrollEl.addEventListener("drop", (e) => {
    if (!e.dataTransfer.files || e.dataTransfer.files.length === 0) return;
    e.preventDefault();
    handleFileSelected(e.dataTransfer.files[0]);
  });

  unsubscribeWs = chatWs.on(onChatEvent);
  // Re-hydrates history on every reconnect (network blip, laptop sleep,
  // server restart), not just at mount: handleWSChat deliberately never
  // replays a backlog (see its doc comment), so this is what recovers
  // anything published while the socket was down instead of leaving the
  // tab silently stale until a manual reload.
  unsubscribeReconnect = chatWs.onReconnect(loadHistory);
  // Picks up a turn already in flight for this user on ANY channel — not
  // just this tab's own send() — so reloading mid-turn, opening a second
  // tab, or a turn started via HA/Telegram while this tab is just sitting
  // open all show the waiting indicator too (see onTurnStatusChange).
  // startPolling() is paired with stopPolling() in unmount() so the timer
  // doesn't leak across navigations; its immediate pollOnce() plus
  // chat-ws.js's connect-time snapshot mean a fresh mount learns the real
  // state within one round trip, without racing loadHistory() below (both
  // signals arrive asynchronously, after this synchronous setup returns).
  unsubscribeTurnStatus = chatTurnStatus.onStatusChange(onTurnStatusChange);
  chatTurnStatus.startPolling();
  loadHistory();
}

export function unmount() {
  // In-flight fetches are safe to let finish; the WS subscriptions aren't —
  // chat-ws.js's connection outlives this screen (app.js starts it once,
  // module-lifetime), so an unmounted screen must stop listening or a
  // later remount would double-dispatch every event/reconnect through two
  // handlers.
  unsubscribeWs?.();
  unsubscribeReconnect?.();
  unsubscribeTurnStatus?.();
  chatTurnStatus.stopPolling();
  // Discard any pending attachment so a remounted screen starts clean.
  clearAttachment();
  pendingAttachment = null;
  attachBtn = null;
  fileInput = null;
  attachChip = null;
}

/** Reacts to chatTurnStatus's signal for turns NOT triggered by this tab's
 * own send() — isSending guards that case, since send()'s catch path has
 * its own dedicated wait/reconcile flow (waitForReplyOrTurnEnd) that must
 * not be second-guessed here. Covers: reloading the page mid-turn, a
 * second tab, or a turn started via HA/Telegram while this tab just sits
 * open (see mount()'s "full scope" wiring above). */
function onTurnStatusChange(status) {
  if (!status.confirmed || isSending) return;
  if (status.inProgress) {
    activeTurnStartedAt = status.startedAt;
    showThinking();
  } else if (thinkingEl) {
    hideThinking();
    loadHistory();
  }
}
