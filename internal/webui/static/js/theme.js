// Light/dark theme toggle: adds/removes a `.dark` class on <html> (see
// input.css's `@custom-variant dark`, which keys every `dark:` utility off
// that class instead of prefers-color-scheme) and persists the choice in
// localStorage under STORAGE_KEY.
//
// The *first* paint is handled separately by a small inline, non-module
// <script> at the very top of each template's <head>, before the
// stylesheet <link> — duplicated in both templates since login.html and
// index.html are independent page loads with no shared layout. That script
// needs to run synchronously, before this module (deferred by the browser
// like every `type="module"` script) has even started loading, to avoid a
// flash of the wrong theme. It reads the same STORAGE_KEY directly (it
// can't import a constant from here), so keep the two in sync by hand if
// this ever changes.
import { icon } from "./icons.js";
import { t } from "./i18n.js";

const STORAGE_KEY = "miranda-theme"; // localStorage value: "light" | "dark"

// Mirrors the two values the pre-paint inline <script> (see the comment
// above) writes into <meta name="theme-color" id="theme-color-meta">
// before first paint — kept here too so an explicit toggle (as opposed to
// the OS-preference default the inline script applies) updates the
// browser-chrome color immediately instead of leaving it stuck on
// whichever theme was active at page load.
const THEME_COLORS = { light: "#f8fafc", dark: "#160b2e" };

/** True if <html> currently carries the .dark class. */
function isDark() {
  return document.documentElement.classList.contains("dark");
}

function syncThemeColorMeta(theme) {
  const meta = document.getElementById("theme-color-meta");
  if (meta) meta.content = THEME_COLORS[theme];
}

// The button's icon always reflects the *current* theme (sun while dark is
// active, moon while light is active) rather than the theme a click would
// switch to — the aria-label/title carry the destination instead, so both
// sighted and screen-reader users get an unambiguous "what happens if I
// press this" signal.
function syncButton(button) {
  const dark = isDark();
  button.innerHTML = icon(dark ? "sun" : "moon", "h-4 w-4");
  const label = dark
    ? t("theme_toggle_light", "Switch to light theme")
    : t("theme_toggle_dark", "Switch to dark theme");
  button.setAttribute("aria-label", label);
  button.title = label;
}

/** Applies `theme` ("light" | "dark") to <html> and persists it. */
function applyTheme(theme) {
  document.documentElement.classList.toggle("dark", theme === "dark");
  syncThemeColorMeta(theme);
  try {
    localStorage.setItem(STORAGE_KEY, theme);
  } catch {
    // localStorage can throw in private-browsing/storage-restricted
    // contexts — the toggle still works for the rest of this page load, it
    // just won't be remembered on the next one.
  }
}

/**
 * Re-syncs the theme-color meta tag (defensively — the pre-paint inline
 * script should have already set it correctly), then wires a theme toggle
 * `button` (an icon-only button already present in the template): syncs its
 * icon/label to whatever the pre-paint inline script already applied, and
 * flips the theme on every click. The button-wiring half no-ops if `button`
 * is missing so callers don't need to guard the lookup themselves.
 */
export function init(button) {
  syncThemeColorMeta(isDark() ? "dark" : "light");
  if (!button) return;
  syncButton(button);
  button.addEventListener("click", () => {
    applyTheme(isDark() ? "light" : "dark");
    syncButton(button);
  });
}
