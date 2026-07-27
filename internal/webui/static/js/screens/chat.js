// Messenger-style chat screen: restores the user's current open
// conversation (if any) as history, then lets them keep talking to
// Miranda — same POST /api/v1/input the debug box always used, just
// rendered as a proper message thread instead of a single reply box.
import { t } from "../i18n.js";
import { icon } from "../icons.js";
import * as chatWs from "../chat-ws.js";

let messagesEl, scrollEl, formEl, textEl, sendBtn, unsubscribeWs, unsubscribeReconnect;

// message id (real history.Message.ID, or a "local:" temp key for a
// not-yet-confirmed optimistic bubble) -> its rendered DOM node. This is
// what lets a ChatEvent arriving over chat-ws.js — for a turn this tab
// itself triggered, or one that arrived via HA/Telegram/another tab — be
// rendered exactly once no matter which of the two (HTTP response, WS
// event) gets there first; see CLAUDE.md's "Session ownership".
const rendered = new Map();

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

/** Same "is this a bubble a person should see" rule loadHistory() has
 * always applied to GET /api/dialogs/{id} rows — tool-call/tool-result
 * turns (see internal/httpapi's recordAssistantToolCallMessage/
 * recordToolCall) are recorded and streamed over chat-ws.js too (for a
 * future debug view), but never rendered as chat bubbles. */
function isChatBubble(m) {
  return (m.role === "user" || m.role === "assistant") && Boolean(m.content?.trim());
}

function formatTime(iso) {
  try {
    return new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  } catch {
    return "";
  }
}

function bubble(role, text, timeIso) {
  const wrap = document.createElement("div");
  wrap.className = `flex flex-col ${role === "user" ? "items-end" : "items-start"}`;

  const b = document.createElement("div");
  b.className =
    role === "user"
      ? "max-w-[85%] whitespace-pre-wrap rounded-2xl rounded-br-md bg-(--color-accent) px-4 py-2.5 text-sm leading-relaxed text-white sm:max-w-[75%]"
      : "max-w-[85%] whitespace-pre-wrap rounded-2xl rounded-bl-md border border-(--color-border) bg-(--color-surface)/70 px-4 py-2.5 text-sm leading-relaxed text-(--color-text) sm:max-w-[75%]";
  b.textContent = text;
  wrap.appendChild(b);

  if (timeIso) {
    const time = document.createElement("span");
    time.className = "mt-1 px-1 text-xs text-(--color-text-muted)";
    time.textContent = formatTime(timeIso);
    wrap.appendChild(time);
  }

  return wrap;
}

/** The three-dot "Miranda is thinking" indicator shown while a request is in flight. */
function typingIndicator() {
  const wrap = document.createElement("div");
  wrap.className = "flex items-start";
  wrap.innerHTML = `
    <div class="flex items-center gap-1 rounded-2xl rounded-bl-md border border-(--color-border) bg-(--color-surface)/70 px-4 py-3">
      <span class="h-1.5 w-1.5 animate-bounce rounded-full bg-(--color-text-faint) [animation-delay:-0.3s]"></span>
      <span class="h-1.5 w-1.5 animate-bounce rounded-full bg-(--color-text-faint) [animation-delay:-0.15s]"></span>
      <span class="h-1.5 w-1.5 animate-bounce rounded-full bg-(--color-text-faint)"></span>
    </div>`;
  return wrap;
}

/** A visually distinct error bubble with an inline retry action — errors
 * must never look like just another assistant message (persona doc: "be
 * visually distinguishable" + "provide recovery action"). */
function errorBubble(detail, onRetry) {
  const wrap = document.createElement("div");
  wrap.className = "flex flex-col items-start gap-2";
  wrap.innerHTML = `
    <div class="flex max-w-[85%] items-start gap-2 rounded-2xl rounded-bl-md border border-(--color-danger-border) bg-(--color-danger-bg) px-4 py-2.5 text-sm text-(--color-danger-text) sm:max-w-[75%]">
      ${icon("alert-circle", "mt-0.5 h-4 w-4 shrink-0")}
      <span class="detail-text"></span>
    </div>`;
  // `detail` wraps a caught exception's message or an HTTP status — set via
  // textContent rather than interpolated into the innerHTML above, so it
  // can never be interpreted as markup no matter what a failing request or
  // an unusual Error object happens to stringify to.
  wrap.querySelector(".detail-text").textContent = `${t("request_failed", "Request failed:")} ${detail}`;
  const retry = document.createElement("button");
  retry.type = "button";
  retry.className =
    "ml-1 rounded-md px-2 py-1 text-xs font-medium text-(--color-text-muted) underline decoration-(--color-border-strong) underline-offset-2 transition-colors hover:text-(--color-text) focus-visible:outline-none";
  retry.textContent = t("retry_button", "Try again");
  retry.addEventListener("click", onRetry);
  wrap.appendChild(retry);
  return wrap;
}

function emptyState() {
  const wrap = document.createElement("div");
  wrap.dataset.emptyState = "true";
  wrap.className = "flex flex-col items-center gap-3 px-6 py-20 text-center";
  wrap.innerHTML = `
    <span class="flex h-12 w-12 items-center justify-center rounded-full bg-(--color-surface-2) text-(--color-text-faint)">${icon("chat", "h-5 w-5")}</span>
    <div class="space-y-1">
      <p class="text-sm font-medium text-(--color-text-muted)">${t("chat_empty_title", "Start a conversation")}</p>
      <p class="max-w-xs text-sm text-(--color-text-faint)">${t("chat_empty_subtitle", "Ask Miranda anything — she remembers context across the conversation.")}</p>
    </div>`;
  return wrap;
}

function skeleton() {
  const wrap = document.createElement("div");
  wrap.className = "flex flex-col gap-4";
  wrap.innerHTML = `
    <div class="flex justify-start"><div class="skeleton animate-shimmer h-9 w-48 rounded-2xl"></div></div>
    <div class="flex justify-end"><div class="skeleton animate-shimmer h-9 w-32 rounded-2xl"></div></div>
    <div class="flex justify-start"><div class="skeleton animate-shimmer h-9 w-56 rounded-2xl"></div></div>`;
  return wrap;
}

function scrollToBottom() {
  scrollEl.scrollTop = scrollEl.scrollHeight;
}

function clearMessages() {
  messagesEl.innerHTML = "";
  rendered.clear();
  pendingUserKey = null;
  renderGeneration++;
}

/** Render a message delivered live over chat-ws.js, if it isn't already on
 * screen — a no-op for an id already in `rendered` (this tab's own HTTP
 * response beat the WS event, or a duplicate delivery), and for turns
 * isChatBubble filters out. */
function upsertMessage(message) {
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

  messagesEl.querySelector("[data-empty-state]")?.remove();
  const node = bubble(message.role, message.content, message.created_at);
  messagesEl.appendChild(node);
  rendered.set(message.id, node);
  scrollToBottom();
}

function onChatEvent(ev) {
  // The socket carries the raw hub.Event envelope ({source, message, data,
  // user_id} — see internal/hub.Event); the ChatEvent this screen cares
  // about is nested under `data`, not at the top level.
  const chatEvent = ev.data;
  if (!chatEvent) return;
  if (chatEvent.type === "message") {
    upsertMessage(chatEvent.message);
  } else if (chatEvent.type === "conversation_deleted" || chatEvent.type === "conversation_ended") {
    clearMessages();
    messagesEl.appendChild(emptyState());
  }
}

async function loadHistory() {
  clearMessages();
  messagesEl.appendChild(skeleton());
  try {
    const res = await fetch("/api/dialogs?limit=1");
    const conversations = await res.json();
    if (!conversations || conversations.length === 0 || conversations[0].ended_at) {
      clearMessages();
      messagesEl.appendChild(emptyState());
      return;
    }

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
      return;
    }
    for (const m of chatMessages) {
      const node = bubble(m.role, m.content, m.created_at);
      messagesEl.appendChild(node);
      rendered.set(m.id, node);
    }
    scrollToBottom();
  } catch {
    // Best-effort restore only — a failed background history fetch just
    // means starting fresh, same as if there were no prior conversation.
    clearMessages();
    messagesEl.appendChild(emptyState());
  }
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

  messagesEl.querySelector("[data-empty-state]")?.remove();
  const userNode = bubble("user", text);
  messagesEl.appendChild(userNode);
  // Keyed under a temporary id until the response tells us the real
  // history.Message.ID — see the `rendered`/`pendingUserKey` doc comments
  // above.
  const tempKey = `local:${Date.now()}:${Math.random()}`;
  rendered.set(tempKey, userNode);
  pendingUserKey = tempKey;
  scrollToBottom();

  const thinking = typingIndicator();
  messagesEl.appendChild(thinking);
  scrollToBottom();
  setSending(true);

  try {
    const res = await fetch("/api/v1/input", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ source: "web_ui", text }),
    });
    thinking.remove();
    if (renderGeneration !== myGeneration) {
      // The thread this bubble belonged to is gone (cleared by a
      // conversation_deleted/ended event that arrived over chat-ws.js while
      // this request was still in flight, or a remount) — nothing left to
      // update.
      return;
    }
    if (!res.ok) {
      messagesEl.appendChild(errorBubble(String(res.status), () => send(text)));
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
        const assistantNode = bubble("assistant", data.reply, new Date().toISOString());
        messagesEl.appendChild(assistantNode);
        rendered.set(data.assistant_message_id, assistantNode);
      }
    }
  } catch (err) {
    thinking.remove();
    if (renderGeneration !== myGeneration) return;
    messagesEl.appendChild(errorBubble(String(err), () => send(text)));
  } finally {
    setSending(false);
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
      </div>
      <div class="border-t border-(--color-border) bg-(--color-bg)/70">
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
          <label for="chat-text" class="sr-only">${t("chat_placeholder", "Message Miranda…")}</label>
          <textarea id="chat-text" rows="1" placeholder="${t("chat_placeholder", "Message Miranda…")}"
            class="scrollbar-thin max-h-40 flex-1 resize-none rounded-xl border border-(--color-border-strong) bg-(--color-surface)/60 px-4 py-2.5 text-sm text-(--color-text) transition-colors placeholder:text-(--color-text-faint) hover:border-(--color-text-faint) focus:border-(--color-accent-emphasis) focus:outline-none focus-visible:outline-none"></textarea>
          <button type="submit" id="chat-send" aria-label="${t("send_button", "Send")}"
            class="flex h-11 w-11 shrink-0 items-center justify-center rounded-xl bg-(--color-accent) text-white transition-colors hover:bg-(--color-accent-hover) focus-visible:outline-none active:bg-(--color-accent-active) disabled:cursor-not-allowed disabled:opacity-60">
            ${icon("send", "h-4 w-4")}
          </button>
        </form>
        <p class="mx-auto hidden max-w-3xl px-4 pb-3 text-xs text-(--color-text-faint) sm:block sm:px-6">${t("chat_hint", "Enter to send · Shift+Enter for a new line")}</p>
      </div>
    </div>`;

  scrollEl = container.querySelector("#chat-scroll");
  messagesEl = container.querySelector("#chat-messages");
  formEl = container.querySelector("#chat-form");
  textEl = container.querySelector("#chat-text");
  sendBtn = container.querySelector("#chat-send");

  formEl.addEventListener("submit", onSubmit);
  textEl.addEventListener("keydown", onKeydown);
  textEl.addEventListener("input", autoResize);

  unsubscribeWs = chatWs.on(onChatEvent);
  // Re-hydrates history on every reconnect (network blip, laptop sleep,
  // server restart), not just at mount: handleWSChat deliberately never
  // replays a backlog (see its doc comment), so this is what recovers
  // anything published while the socket was down instead of leaving the
  // tab silently stale until a manual reload.
  unsubscribeReconnect = chatWs.onReconnect(loadHistory);
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
}
