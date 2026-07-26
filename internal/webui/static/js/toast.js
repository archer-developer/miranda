// Shared toast/snackbar component (see the persona doc's Components list —
// "Toast" — and Microinteractions — "Toast transitions"). Used sparingly:
// only for actions whose result isn't already obvious from the screen
// itself (saving memory, adding/removing a passkey). Chat's own send errors
// stay inline as a distinguishable message bubble instead, since a toast
// would just duplicate a signal that's already visible in the thread.
//
// Lazily creates and reuses a single fixed-position region on first use, so
// no screen or template needs to know this exists until it's called.
import { icon } from "./icons.js";
import { t } from "./i18n.js";

const AUTO_DISMISS_MS = 4000;

let region = null;

function ensureRegion() {
  if (region) return region;
  region = document.createElement("div");
  region.id = "toast-region";
  // aria-live="polite" + role="status": announced to screen readers without
  // interrupting whatever they're currently reading, matching how a toast
  // should behave visually (noticeable, not blocking).
  region.setAttribute("aria-live", "polite");
  region.setAttribute("role", "status");
  region.className =
    "pointer-events-none fixed inset-x-0 bottom-0 z-50 flex flex-col items-center gap-2 p-4 sm:items-end sm:p-6";
  document.body.appendChild(region);
  return region;
}

const VARIANTS = {
  success: { icon: "check", accent: "text-(--color-success-text)" },
  error: { icon: "alert-circle", accent: "text-(--color-danger-icon)" },
  info: { icon: "check", accent: "text-(--color-text-muted)" },
};

/**
 * Shows a single toast. `type` is "success" | "error" | "info" (default
 * "info"). Auto-dismisses after AUTO_DISMISS_MS, or immediately on click of
 * its close button.
 */
export function showToast(message, type = "info") {
  const container = ensureRegion();
  const variant = VARIANTS[type] || VARIANTS.info;

  const el = document.createElement("div");
  el.className =
    "animate-slide-up pointer-events-auto flex w-full max-w-sm items-start gap-3 rounded-xl border border-(--color-border) bg-(--color-surface)/95 p-4 shadow-lg shadow-black/10 backdrop-blur";
  el.innerHTML = `
    <span class="${variant.accent} shrink-0">${icon(variant.icon, "h-5 w-5")}</span>
    <p class="message-text min-w-0 flex-1 text-sm leading-snug text-(--color-text)"></p>
    <button type="button" aria-label="${t("close_button", "Close")}"
      class="shrink-0 rounded-md p-1 text-(--color-text-faint) transition-colors hover:bg-(--color-surface-2) hover:text-(--color-text-muted) focus-visible:outline-none">
      ${icon("close", "h-4 w-4")}
    </button>`;
  // `message` can carry a caught exception's text (e.g. from a failed
  // memory save or passkey action) — set via textContent, never
  // interpolated into innerHTML, so it can never be interpreted as markup.
  el.querySelector(".message-text").textContent = message;

  const dismiss = () => {
    el.style.animation = "none";
    el.classList.add("animate-fade-in");
    el.style.animationDirection = "reverse";
    el.addEventListener("animationend", () => el.remove(), { once: true });
    // Belt-and-suspenders: if animations are disabled (prefers-reduced-motion)
    // animationend still fires (duration collapses to ~0), so this always
    // resolves either way.
  };

  el.querySelector("button").addEventListener("click", dismiss);
  const timer = setTimeout(dismiss, AUTO_DISMISS_MS);
  el.addEventListener("mouseenter", () => clearTimeout(timer));

  container.appendChild(el);
}
