// Shared inline-SVG icon set, Lucide-inspired (see the frontend-designer
// persona doc: "Lucide's icon set is fine as a visual reference; embed the
// specific icons you need as optimized inline <svg>, not as an installed
// package"). Every icon shares the same 24x24 stroke-based convention so
// they drop into any screen at a consistent visual weight — callers control
// size/color entirely through the className they pass in (typically a
// Tailwind `h-4 w-4` / `text-slate-400` pair), never inline style attributes.
//
// Kept deliberately small: only icons actually used somewhere in the
// dashboard belong here (persona doc: "do not decorate everything with
// icons").

const ICON_PATHS = {
  chat: '<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>',
  history: '<circle cx="12" cy="12" r="9"/><polyline points="12 7 12 12 16 14"/>',
  memory: '<path d="M19 21l-7-4-7 4V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2z"/>',
  logs: '<rect x="3" y="4" width="18" height="16" rx="2"/><polyline points="7.5 9 10.5 12 7.5 15"/><line x1="12.5" y1="15" x2="17" y2="15"/>',
  profile: '<circle cx="12" cy="8" r="4"/><path d="M4 20c0-4 4-6 8-6s8 2 8 6"/>',
  "chevron-down": '<polyline points="6 9 12 15 18 9"/>',
  "chevron-right": '<polyline points="9 6 15 12 9 18"/>',
  close: '<line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/>',
  plus: '<line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/>',
  check: '<polyline points="20 6 9 17 4 12"/>',
  trash:
    '<polyline points="3 6 5 6 21 6"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/><path d="M10 11v6"/><path d="M14 11v6"/><path d="M9 6V4a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2"/>',
  key: '<circle cx="7" cy="15" r="4"/><path d="M9.5 12.5 20 2"/><path d="M17 5l3 3"/><path d="M14 8l2.5 2.5"/>',
  send: '<line x1="21" y1="3" x2="10" y2="14"/><polygon points="21 3 14 21 10 14 3 10 21 3"/>',
  "alert-circle": '<circle cx="12" cy="12" r="9"/><line x1="12" y1="8" x2="12" y2="13"/><line x1="12" y1="16" x2="12" y2="16.01"/>',
  inbox: '<path d="M4 12h4l2 3h4l2-3h4"/><rect x="3" y="5" width="18" height="14" rx="2"/>',
  "log-out": '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" y1="12" x2="9" y2="12"/>',
  "refresh-cw":
    '<polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10"/><path d="M20.49 15a9 9 0 0 1-14.85 3.36L1 14"/>',
  eye: '<path d="M1 12s4-7 11-7 11 7 11 7-4 7-11 7-11-7-11-7z"/><circle cx="12" cy="12" r="3"/>',
  "eye-off":
    '<path d="M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 7 11 7a13.16 13.16 0 0 1-1.67 2.68"/><path d="M6.61 6.61C3.35 8.36 1 12 1 12s4 7 11 7a9.26 9.26 0 0 0 5.39-1.61"/><path d="M14.12 14.12a3 3 0 1 1-4.24-4.24"/><line x1="1" y1="1" x2="23" y2="23"/>',
  // Theme toggle (see static/js/theme.js): "sun" is shown while the dark
  // theme is active (click to switch to light), "moon" while the light
  // theme is active (click to switch to dark) — the icon always reflects
  // the *current* theme, not the destination.
  sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/>',
  moon: '<path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/>',
};

/**
 * Returns an inline `<svg>` string for `name` (see ICON_PATHS above).
 * `className` is applied verbatim to the `<svg>` element — pass sizing
 * (`h-4 w-4`) and color (`text-slate-400`) utilities, never inline styles.
 * Unknown names render nothing rather than throwing, so a typo degrades to
 * "missing icon" instead of a broken screen.
 */
export function icon(name, className = "h-4 w-4") {
  const path = ICON_PATHS[name];
  if (!path) return "";
  return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="${className}" aria-hidden="true" focusable="false">${path}</svg>`;
}
