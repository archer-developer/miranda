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

  // A short fade-in on the freshly mounted screen — cheap, GPU-friendly
  // (opacity only) motion that signals "new content arrived" on every
  // navigation, per the persona doc's page-transition guidance (250-350ms).
  // Re-triggered per navigation by removing/re-adding the class on the next
  // frame, since the container element itself is reused.
  container.classList.remove("animate-fade-in");
  // eslint-disable-next-line no-unused-expressions -- force reflow so the
  // animation restarts even though the class name didn't change.
  void container.offsetWidth;
  container.classList.add("animate-fade-in");

  // Move focus to the new screen for keyboard/screen-reader users — without
  // this, focus silently stays on whatever nav link was clicked (or nowhere,
  // after a hashchange from history navigation), which makes a single-page
  // app feel broken to anyone not using a mouse.
  container.focus({ preventScroll: true });

  document.querySelectorAll("[data-nav-link]").forEach((el) => {
    const active = el.getAttribute("href") === hash;
    el.classList.toggle("bg-(--color-surface-2)", active);
    el.classList.toggle("text-(--color-text)", active);
    el.classList.toggle("text-(--color-text-muted)", !active);
    if (active) {
      el.setAttribute("aria-current", "page");
    } else {
      el.removeAttribute("aria-current");
    }
  });
}

export function start() {
  window.addEventListener("hashchange", render);
  render();
}
