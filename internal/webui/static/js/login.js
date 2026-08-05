// Wires the login page's biometric button. Only loaded at all when the
// server has WebAuthn enabled (see templates/login.html); the button
// itself stays hidden unless this browser context actually supports it
// (window.PublicKeyCredential — false on any insecure, non-HTTPS origin),
// so there's never a control that would just fail if clicked.
import { t } from "./i18n.js";
import { isSupported, loginWithPasskey, hasRememberedPasskey } from "./webauthn.js";

const button = document.getElementById("biometric-login");
const errorEl = document.getElementById("biometric-error");
const passwordSection = document.getElementById("password-section");
const passwordDivider = document.getElementById("password-divider");
const passwordToggle = document.getElementById("password-toggle-link");

// The accent-filled "primary" look biometric-login takes on once the
// password form is collapsed behind it; SECONDARY_CLASSES is just whatever
// the button already ships with in login.html (its "alternative method"
// look), captured here so re-expanding the password form can restore it
// exactly rather than duplicating that class list a second time.
const PRIMARY_CLASSES =
  "flex w-full items-center justify-center gap-2 rounded-lg bg-(--color-accent) px-3.5 py-2.5 text-sm font-medium text-white transition-colors hover:bg-(--color-accent-hover) focus-visible:outline-none active:bg-(--color-accent-active)";
const SECONDARY_CLASSES = button ? button.className : "";

if (button && isSupported()) {
  button.hidden = false;

  // hasRememberedPasskey() is a browser-local guess, not proof this
  // specific visitor has a passkey (login here is usernameless, so the
  // server has no per-account signal to offer before the ceremony even
  // starts) — see webauthn.js's REMEMBER_KEY doc comment. Collapsing on a
  // wrong guess costs one click on passwordToggle below, never a dead end.
  if (hasRememberedPasskey() && passwordSection && passwordToggle) {
    passwordSection.hidden = true;
    if (passwordDivider) passwordDivider.hidden = true;
    passwordToggle.hidden = false;
    button.className = PRIMARY_CLASSES;
  }

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
      const { redirect } = await loginWithPasskey();
      location.href = redirect || "/";
    } catch (err) {
      errorEl.textContent = t("login_biometric_error", "Biometric sign-in failed. Try your password instead.");
      errorEl.classList.remove("hidden");
      button.disabled = false;
    }
  });
}
