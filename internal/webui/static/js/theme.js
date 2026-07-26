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

/** True if <html> currently carries the .dark class. */
function isDark() {
  return document.documentElement.classList.contains("dark");
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
  try {
    localStorage.setItem(STORAGE_KEY, theme);
  } catch {
    // localStorage can throw in private-browsing/storage-restricted
    // contexts — the toggle still works for the rest of this page load, it
    // just won't be remembered on the next one.
  }
}

/**
 * Wires a theme toggle `button` (an icon-only button already present in the
 * template): syncs its icon/label to whatever the pre-paint inline script
 * already applied, then flips the theme on every click. No-ops if `button`
 * is missing so callers don't need to guard the lookup themselves.
 */
export function init(button) {
  if (!button) return;
  syncButton(button);
  button.addEventListener("click", () => {
    applyTheme(isDark() ? "light" : "dark");
    syncButton(button);
  });
}
