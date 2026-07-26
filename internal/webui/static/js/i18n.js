// Thin wrapper around window.MIRANDA_I18N (injected server-side per the
// logged-in session's language — see internal/webui/view.go), shared by
// every screen module.
const strings = window.MIRANDA_I18N || {};

export function t(key, fallback) {
  return strings[key] || fallback;
}
