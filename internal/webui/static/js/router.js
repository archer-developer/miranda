// Minimal hash router: maps location.hash to a screen module and swaps the
// #app container's content, without a full page reload — that's what keeps
// the single WebSocket connection (see ws.js) and nav state alive across
// navigation. No history-API routing: hash-only keeps this a static file
// server with zero server-side route awareness beyond serving index.html.
const routes = new Map(); // "#/chat" -> { mount(container), unmount? }
let current = null;
let defaultRoute = null;

export function register(hash, screen) {
  routes.set(hash, screen);
}

export function setDefault(hash) {
  defaultRoute = hash;
}

function resolve() {
  const hash = location.hash || defaultRoute;
  return routes.has(hash) ? hash : defaultRoute;
}

function render() {
  const hash = resolve();
  if (hash === current) return;

  const container = document.getElementById("app");
  const prev = routes.get(current);
  if (prev && prev.unmount) prev.unmount();

  current = hash;
  container.innerHTML = "";
  const screen = routes.get(hash);
  if (screen) screen.mount(container);

  document.querySelectorAll("[data-nav-link]").forEach((el) => {
    el.classList.toggle("bg-slate-800", el.getAttribute("href") === hash);
    el.classList.toggle("text-white", el.getAttribute("href") === hash);
  });
}

export function start() {
  window.addEventListener("hashchange", render);
  render();
}
