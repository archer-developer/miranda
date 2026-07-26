// Wires the login page's biometric button. Only loaded at all when the
// server has WebAuthn enabled (see templates/login.html); the button
// itself stays hidden unless this browser context actually supports it
// (window.PublicKeyCredential — false on any insecure, non-HTTPS origin),
// so there's never a control that would just fail if clicked.
import { t } from "./i18n.js";
import { isSupported, loginWithPasskey } from "./webauthn.js";

const button = document.getElementById("biometric-login");
const errorEl = document.getElementById("biometric-error");

if (button && isSupported()) {
  button.hidden = false;
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
