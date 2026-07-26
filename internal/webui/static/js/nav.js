import { t } from "./i18n.js";

const NAV_ITEMS = [
  { hash: "#/chat", key: "nav_chat", fallback: "Chat" },
  { hash: "#/history", key: "nav_history", fallback: "History" },
  { hash: "#/memory", key: "nav_memory", fallback: "Memory" },
  { hash: "#/logs", key: "nav_logs", fallback: "Logs" },
  { hash: "#/profile", key: "nav_profile", fallback: "Profile" },
];

function linkClass(mobile) {
  return mobile
    ? "rounded-md px-3 py-2 text-slate-300 hover:bg-slate-800"
    : "rounded-md px-3 py-1.5 text-slate-300 hover:bg-slate-800";
}

function renderLinks(container, mobile) {
  container.innerHTML = "";
  for (const item of NAV_ITEMS) {
    const a = document.createElement("a");
    a.href = item.hash;
    a.dataset.navLink = "true";
    a.className = linkClass(mobile);
    a.textContent = t(item.key, item.fallback);
    container.appendChild(a);
  }
}

export function init() {
  renderLinks(document.getElementById("nav-links"), false);
  renderLinks(document.getElementById("nav-links-mobile"), true);

  const burger = document.getElementById("nav-burger");
  const drawer = document.getElementById("nav-drawer");
  burger.addEventListener("click", () => {
    const open = drawer.classList.toggle("flex");
    drawer.classList.toggle("hidden");
    burger.setAttribute("aria-expanded", String(open));
  });

  // Close the mobile drawer after navigating, so it doesn't stay open over
  // the newly rendered screen.
  window.addEventListener("hashchange", () => {
    if (!drawer.classList.contains("hidden")) {
      drawer.classList.add("hidden");
      drawer.classList.remove("flex");
      burger.setAttribute("aria-expanded", "false");
    }
  });
}
