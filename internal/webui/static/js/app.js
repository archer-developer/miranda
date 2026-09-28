// SPA entrypoint: wires up the nav bar, the hash router and its five
// screens, and the one persistent WebSocket connection. Native ES modules —
// no bundler, no build step (see internal/webui's package doc comment).
//
// auth-fetch.js is imported first, and only for its side effect (patching
// window.fetch) — every screen below calls the global fetch() directly, so
// installing the 401/403-redirect wrapper before any of them run is what
// makes it apply everywhere without touching each call site.
import "./auth-fetch.js";
import "./pwa.js";
import * as ws from "./ws.js";
import * as chatWs from "./chat-ws.js";
import * as router from "./router.js";
import * as nav from "./nav.js";
import * as theme from "./theme.js";
import * as notifyBadge from "./notify-badge.js";
import * as chat from "./screens/chat.js";
import * as history from "./screens/history.js";
import * as memory from "./screens/memory.js";
import * as logs from "./screens/logs.js";
import * as profile from "./screens/profile.js";
import * as notifications from "./screens/notifications.js";

nav.init();
theme.init(document.getElementById("theme-toggle"));

router.register("#/chat", chat);
router.register("#/history", history);
router.register("#/memory", memory);
router.register("#/logs", logs);
router.register("#/profile", profile);
router.register("#/notifications", notifications);
router.setDefault("#/chat");
router.start();

ws.connect();
chatWs.connect();

// Live notification delivery — see docs/adr/native-notifications.md.
// Orchestrator.notifyUser publishes a ChatEvent{Type: "notification"} on
// this same per-user chat-ws.js connection every time a notification is
// created (a fired reminder or the send_notification tool), so no separate
// WS/poll is needed here. Policy: always light the bell first — a new one
// arrived regardless of what's on screen — then, only if the notifications
// screen itself is the one currently open, reload it; refreshAndMarkRead
// itself is what turns the dot back off once its mark-read call succeeds
// (see screens/notifications.js), not the arrival of this event by itself.
notifyBadge.init();
chatWs.on(ev => {
  // The socket carries the raw hub.Event envelope ({source, message, data,
  // user_id} — see internal/hub.Event); the ChatEvent this cares about is
  // nested under `data`, not at the top level — same shape chat.js's own
  // onChatEvent unwraps.
  const chatEvent = ev.data;
  if (!chatEvent || chatEvent.type !== "notification") return;
  notifyBadge.setUnread(true);
  if (location.hash === "#/notifications") {
    notifications.refreshAndMarkRead();
  }
});
