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
import * as chat from "./screens/chat.js";
import * as history from "./screens/history.js";
import * as memory from "./screens/memory.js";
import * as logs from "./screens/logs.js";
import * as profile from "./screens/profile.js";

nav.init();
theme.init(document.getElementById("theme-toggle"));

router.register("#/chat", chat);
router.register("#/history", history);
router.register("#/memory", memory);
router.register("#/logs", logs);
router.register("#/profile", profile);
router.setDefault("#/chat");
router.start();

ws.connect();
chatWs.connect();
