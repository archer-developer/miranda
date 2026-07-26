// Wires the header: renders the nav links (desktop pill row + mobile
// drawer list) and drives the mobile drawer's open/close animation and
// accessibility state. See index.html for the load-bearing ids this reads
// (#nav-links, #nav-links-mobile, #nav-burger, #nav-drawer).
import { t } from "./i18n.js";
import { icon } from "./icons.js";

const NAV_ITEMS = [
  { hash: "#/chat", key: "nav_chat", fallback: "Chat", icon: "chat" },
  { hash: "#/history", key: "nav_history", fallback: "History", icon: "history" },
  { hash: "#/memory", key: "nav_memory", fallback: "Memory", icon: "memory" },
  { hash: "#/logs", key: "nav_logs", fallback: "Logs", icon: "logs" },
  { hash: "#/profile", key: "nav_profile", fallback: "Profile", icon: "profile" },
];

// router.js owns toggling the active-link surface/text tokens and
// aria-current on the active link (it already knows the current hash); this
// module only owns the classes that never change once rendered.
//
// min-h-11 (44px) on both variants: the desktop pill row isn't
// desktop-exclusive in practice — it renders starting at the `sm` (640px)
// breakpoint, which plenty of touch tablets in portrait exceed, so it needs
// a real touch target too, not just the burger-menu/drawer variant.
function linkClass(mobile) {
  return mobile
    ? "flex min-h-11 items-center gap-3 rounded-lg px-3 py-2.5 text-(--color-text-muted) transition-colors hover:bg-(--color-surface-2) hover:text-(--color-text)"
    : "flex min-h-11 items-center gap-2 rounded-full px-3.5 text-(--color-text-muted) transition-colors hover:bg-(--color-surface-2) hover:text-(--color-text)";
}

function renderLinks(container, mobile) {
  container.innerHTML = "";
  for (const item of NAV_ITEMS) {
    const a = document.createElement("a");
    a.href = item.hash;
    a.dataset.navLink = "true";
    a.className = linkClass(mobile);
    a.innerHTML = `${icon(item.icon, "h-4 w-4 shrink-0")}<span>${t(item.key, item.fallback)}</span>`;
    container.appendChild(a);
  }
}

const MENU_ICON = '<path d="M3.75 6.75h16.5M3.75 12h16.5M3.75 17.25h16.5"/>';
const CLOSE_ICON = '<line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/>';

export function init() {
  renderLinks(document.getElementById("nav-links"), false);
  renderLinks(document.getElementById("nav-links-mobile"), true);

  const burger = document.getElementById("nav-burger");
  const drawer = document.getElementById("nav-drawer");
  const burgerIconPath = burger.querySelector("svg");

  function setOpen(open) {
    drawer.classList.toggle("drawer-open", open);
    drawer.classList.toggle("drawer-closed", !open);
    drawer.setAttribute("aria-hidden", String(!open));
    burger.setAttribute("aria-expanded", String(open));
    burgerIconPath.innerHTML = open ? CLOSE_ICON : MENU_ICON;
  }

  burger.addEventListener("click", () => {
    setOpen(drawer.classList.contains("drawer-closed"));
  });

  // Close the mobile drawer after navigating, so it doesn't stay open over
  // the newly rendered screen.
  window.addEventListener("hashchange", () => setOpen(false));

  // Close on Escape and on an outside click/tap — both are expected
  // affordances for any dropdown/menu-like overlay (WAI-ARIA menu pattern).
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && drawer.classList.contains("drawer-open")) {
      setOpen(false);
      burger.focus();
    }
  });
  document.addEventListener("click", (e) => {
    if (!drawer.classList.contains("drawer-open")) return;
    if (drawer.contains(e.target) || burger.contains(e.target)) return;
    setOpen(false);
  });
}
