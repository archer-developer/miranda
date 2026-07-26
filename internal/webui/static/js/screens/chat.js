// Messenger-style chat screen: restores the user's current open
// conversation (if any) as history, then lets them keep talking to
// Miranda — same POST /api/v1/input the debug box always used, just
// rendered as a proper message thread instead of a single reply box.
import { t } from "../i18n.js";

let messagesEl, formEl, textEl;

function bubble(role, text) {
  const wrap = document.createElement("div");
  wrap.className = `flex ${role === "user" ? "justify-end" : "justify-start"}`;

  const b = document.createElement("div");
  b.className =
    role === "user"
      ? "max-w-[80%] rounded-2xl rounded-br-sm bg-indigo-500 px-4 py-2 text-sm text-white"
      : "max-w-[80%] rounded-2xl rounded-bl-sm bg-slate-800 px-4 py-2 text-sm text-slate-100";
  b.textContent = text;

  wrap.appendChild(b);
  return wrap;
}

function scrollToBottom() {
  messagesEl.scrollTop = messagesEl.scrollHeight;
}

async function loadHistory() {
  messagesEl.innerHTML = "";
  try {
    const res = await fetch("/api/dialogs?limit=1");
    const conversations = await res.json();
    if (!conversations || conversations.length === 0 || conversations[0].ended_at) return;

    const msgsRes = await fetch(`/api/dialogs/${encodeURIComponent(conversations[0].id)}`);
    const messages = await msgsRes.json();
    for (const m of messages) {
      if (m.role !== "user" && m.role !== "assistant") continue;
      messagesEl.appendChild(bubble(m.role, m.content));
    }
    scrollToBottom();
  } catch {
    // Best-effort restore only — an empty pane just means starting fresh.
  }
}

async function send(text) {
  messagesEl.appendChild(bubble("user", text));
  scrollToBottom();

  const thinking = bubble("assistant", t("chat_thinking_ellipsis", "…"));
  messagesEl.appendChild(thinking);
  scrollToBottom();

  try {
    const res = await fetch("/api/v1/input", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ source: "web_ui", text }),
    });
    thinking.remove();
    if (!res.ok) {
      messagesEl.appendChild(bubble("assistant", `${t("request_failed", "Request failed:")} ${res.status}`));
    } else {
      const data = await res.json();
      messagesEl.appendChild(bubble("assistant", data.reply));
    }
  } catch (err) {
    thinking.remove();
    messagesEl.appendChild(bubble("assistant", `${t("request_failed", "Request failed:")} ${err}`));
  }
  scrollToBottom();
}

function onSubmit(e) {
  e.preventDefault();
  const text = textEl.value.trim();
  if (!text) return;
  textEl.value = "";
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
      <div id="chat-messages" class="flex-1 space-y-2 overflow-y-auto p-4"></div>
      <form id="chat-form" class="flex gap-2 border-t border-slate-800 p-3">
        <textarea id="chat-text" rows="1" placeholder="${t("chat_placeholder", "Message Miranda…")}"
          class="flex-1 resize-none rounded-md border border-slate-700 bg-slate-950 px-3 py-2 text-sm placeholder:text-slate-500 focus:border-slate-500 focus:outline-none"></textarea>
        <button type="submit" class="rounded-md bg-indigo-500 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-400 active:bg-indigo-600">
          ${t("send_button", "Send")}
        </button>
      </form>
    </div>`;

  messagesEl = container.querySelector("#chat-messages");
  formEl = container.querySelector("#chat-form");
  textEl = container.querySelector("#chat-text");
  formEl.addEventListener("submit", onSubmit);
  textEl.addEventListener("keydown", onKeydown);

  loadHistory();
}

export function unmount() {
  // Nothing to tear down — in-flight fetches are safe to let finish.
}
