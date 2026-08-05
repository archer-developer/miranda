// Installs a global fetch() wrapper so a session that died server-side
// (e.g. a process restart wiped internal/session.Store's in-memory map —
// see internal/webui/auth.go) sends the user back to /login instead of
// leaving every screen to show a bare "Error: 401" (see memory.js,
// history.js, chat.js, webauthn.js — none of them know what a 401 *means*).
//
// Only imported by app.js. The login page loads login.js instead, so this
// never wraps the login form's own fetch() calls to
// /api/webauthn/login/begin|finish — a wrong-password 401 there must stay
// on the login page and show its own error, not bounce through this path.
// Because of that same architectural split there's no need for a
// pathname-based "don't redirect again if we're already on /login" guard
// either: this module is never loaded on /login in the first place.
const nativeFetch = window.fetch.bind(window);

// Screens holding not-yet-persisted user input (e.g. the memory editor's
// textarea — see memory.js) can register a hook here to stash it somewhere
// that survives the redirect below, since a forced navigation away tears
// down the page's JS state before anything unsaved would otherwise get a
// chance to be preserved. Hooks run synchronously, in registration order,
// right before the redirect fires.
const beforeRedirectHooks = new Set();

export function onSessionExpired(hook) {
  beforeRedirectHooks.add(hook);
}

// Guards against redirecting (and re-running every hook) more than once
// per page life — several screens can each have a request in flight when
// the session dies, and every one of them will see the same 401/403.
let redirecting = false;

window.fetch = async (...args) => {
  const res = await nativeFetch(...args);
  if ((res.status === 401 || res.status === 403) && !redirecting) {
    redirecting = true;
    for (const hook of beforeRedirectHooks) {
      // One screen's save hook throwing must not stop the others from
      // running or block the redirect itself.
      try {
        hook();
      } catch {
        /* best-effort only */
      }
    }
    location.href = "/login?expired=1";
  }
  // Resolved normally (not left hanging) even on a redirect-triggering
  // response: the caller's own !res.ok handling still runs, which is what
  // lets its existing try/finally re-enable whatever button/spinner it set
  // before the request — otherwise that UI stays stuck disabled for
  // whatever's left of this page's life before the navigation above
  // actually takes effect.
  return res;
};
