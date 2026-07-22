// Miranda web UI: WebSocket log tail, debug command form, dialog browser.
// Deliberately dependency-free (no build step) — see internal/webui.
(() => {
  const logEl = document.getElementById("log");
  const wsDot = document.getElementById("ws-dot");
  const wsStatus = document.getElementById("ws-status");

  const sourceColor = {
    error: "text-red-400",
    tts: "text-amber-400",
    assistant: "text-emerald-400",
  };

  function appendLogLine(ev) {
    const line = document.createElement("div");
    const color = sourceColor[ev.source] || "text-slate-300";
    const time = new Date().toLocaleTimeString();
    line.className = color;
    line.textContent = `[${time}] ${ev.source}: ${ev.message}`;
    logEl.appendChild(line);
    logEl.scrollTop = logEl.scrollHeight;
  }

  function connectLogs() {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const ws = new WebSocket(`${proto}//${location.host}/ws/logs`);

    ws.onopen = () => {
      wsDot.className = "h-2.5 w-2.5 rounded-full bg-emerald-500";
      wsStatus.textContent = "connected";
    };
    ws.onclose = () => {
      wsDot.className = "h-2.5 w-2.5 rounded-full bg-red-500";
      wsStatus.textContent = "disconnected — retrying…";
      setTimeout(connectLogs, 2000);
    };
    ws.onerror = () => ws.close();
    ws.onmessage = (event) => {
      try {
        appendLogLine(JSON.parse(event.data));
      } catch {
        /* ignore malformed frames */
      }
    };
  }

  function authHeaders() {
    const token = document.getElementById("debug-token").value.trim();
    const headers = { "Content-Type": "application/json" };
    if (token) headers["Authorization"] = `Bearer ${token}`;
    return headers;
  }

  document.getElementById("debug-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const text = document.getElementById("debug-text").value.trim();
    if (!text) return;
    const userID = document.getElementById("debug-user").value.trim();
    const replyEl = document.getElementById("debug-reply");

    replyEl.classList.remove("hidden");
    replyEl.textContent = "Sending…";

    try {
      const res = await fetch("/api/v1/input", {
        method: "POST",
        headers: authHeaders(),
        body: JSON.stringify({ source: "web_ui", user_id: userID, text }),
      });
      if (!res.ok) {
        replyEl.textContent = `Error: ${res.status} ${await res.text()}`;
        return;
      }
      const data = await res.json();
      replyEl.textContent = data.reply;
      document.getElementById("debug-text").value = "";
    } catch (err) {
      replyEl.textContent = `Request failed: ${err}`;
    }
  });

  document.getElementById("dialogs-load").addEventListener("click", async () => {
    const userID = document.getElementById("dialogs-user").value.trim();
    const listEl = document.getElementById("dialogs-list");
    if (!userID) return;

    listEl.textContent = "Loading…";
    try {
      const res = await fetch(`/api/dialogs?user_id=${encodeURIComponent(userID)}`, {
        headers: authHeaders(),
      });
      const conversations = await res.json();
      listEl.innerHTML = "";
      if (!conversations || conversations.length === 0) {
        listEl.textContent = "No conversations yet.";
        return;
      }
      for (const c of conversations) {
        const item = document.createElement("div");
        item.className = "cursor-pointer rounded-md border border-slate-800 p-2 hover:bg-slate-800";
        item.textContent = `${c.started_at} — ${c.source}`;
        item.addEventListener("click", () => loadConversation(c.id, item));
        listEl.appendChild(item);
      }
    } catch (err) {
      listEl.textContent = `Failed to load: ${err}`;
    }
  });

  async function loadConversation(id, container) {
    const res = await fetch(`/api/dialogs/${encodeURIComponent(id)}`, { headers: authHeaders() });
    const messages = await res.json();
    const detail = document.createElement("div");
    detail.className = "mt-2 space-y-1 border-t border-slate-800 pt-2 text-xs text-slate-400";
    for (const m of messages) {
      const line = document.createElement("div");
      line.textContent = `${m.role}: ${m.content}`;
      detail.appendChild(line);
    }
    container.appendChild(detail);
  }

  connectLogs();
})();
