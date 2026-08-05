// Wires the login page's biometric button and the password/passkey
// collapse behavior. Only loaded at all when the server has WebAuthn
// enabled (see templates/login.html); the biometric button itself stays
// hidden unless this browser context actually supports it
// (window.PublicKeyCredential — false on any insecure, non-HTTPS origin),
// so there's never a control that would just fail if clicked.
//
// The *initial* collapsed-vs-expanded layout is decided by a synchronous
// inline <script> in login.html, not here — this file is loaded as a
// deferred module, so it only runs after the document is already parsed
// and painted, which is too late to avoid a one-frame flash of whichever
// layout wasn't chosen. By the time this runs, that inline script has
// already applied the initial state; this file only wires up what happens
// after that (clicks, submit).
import { t } from "./i18n.js";
import { isSupported, loginWithPasskey, rememberPasswordLogin } from "./webauthn.js";

const button = document.getElementById("biometric-login");
const errorEl = document.getElementById("biometric-error");
const passwordSection = document.getElementById("password-section");
const passwordForm = document.getElementById("password-form");
const passwordDivider = document.getElementById("password-divider");
const passwordToggle = document.getElementById("password-toggle-link");

// The bordered/secondary look #biometric-login ships with in login.html —
// hardcoded here (not read from button.className) because by the time this
// module runs, the inline script may have already overwritten it with the
// accent-filled primary look; reading it back at this point would capture
// the wrong one. Keep in sync with login.html's markup and the inline
// script's own copy of the primary look by hand if either ever changes.
const SECONDARY_CLASSES =
  "flex w-full items-center justify-center gap-2 rounded-lg border border-(--color-border-strong) px-3.5 py-2.5 text-sm font-medium text-(--color-text) transition-colors hover:border-(--color-text-faint) hover:bg-(--color-surface-2)/60 focus-visible:outline-none";

// A full-page POST is the only way the password form submits (see
// templates/login.html — no fetch()), so this is the one hook available to
// record the attempt before the browser navigates away; the write is
// synchronous, so it lands even though the page unloads immediately after.
// Recording an *attempt* rather than a confirmed success means a mistyped
// password also gets remembered as "password" — harmless, since a failed
// attempt redirects back here with ?error=1, which the inline script's own
// forcePasswordVisible check already keeps expanded regardless of any
// remembered method.
if (passwordForm) {
  passwordForm.addEventListener("submit", () => rememberPasswordLogin());
}

if (button && isSupported()) {
  button.hidden = false;

  if (passwordToggle) {
    passwordToggle.addEventListener("click", () => {
      passwordSection.hidden = false;
      if (passwordDivider) passwordDivider.hidden = false;
      passwordToggle.hidden = true;
      button.className = SECONDARY_CLASSES;
      document.getElementById("username")?.focus();
    });
  }

  button.addEventListener("click", async () => {
    errorEl.classList.add("hidden");
    button.disabled = true;
    try {
      const { redirect } = await loginWithPasskeyRetryingStaleRequest();
      location.href = redirect || "/";
    } catch (err) {
      console.error("webauthn: biometric login failed", err);
      errorEl.textContent = t("login_biometric_error", "Biometric sign-in failed. Try your password instead.");
      errorEl.classList.remove("hidden");
      button.disabled = false;
    }
  });
}

// Some Android/Chrome builds (routed through Google Play Services' FIDO2
// API, out-of-process from the tab) leave a WebAuthn request "stuck
// pending" from an earlier ceremony — this ceremony's own registration, an
// earlier failed login, or a previous visit navigated away from mid-picker
// — which makes the *next* navigator.credentials.get() reject instantly,
// before the OS picker sheet ever opens. A second call right after clears
// it and opens the picker normally — this is the exact "first tap: red
// error with no picker, second tap: picker opens" bug report this works
// around, by making that second tap automatic instead of requiring the
// user to notice and retry it themselves.
//
// The under-800ms check is what tells that failure apart from a user who
// saw the picker and deliberately tapped Cancel (which also rejects with
// the same DOMException name, but only after the sheet has rendered and
// they've reacted to it) — only the near-instant, picker-never-appeared
// case is worth silently retrying; a deliberate cancel should surface as a
// real error immediately, not silently reopen the sheet on them.
async function loginWithPasskeyRetryingStaleRequest() {
  const start = performance.now();
  try {
    return await loginWithPasskey();
  } catch (err) {
    if (performance.now() - start > 800) throw err;
    return await loginWithPasskey();
  }
}
