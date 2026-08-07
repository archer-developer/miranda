// Vanilla-JS glue between the browser's native WebAuthn API
// (navigator.credentials — no library needed on this side at all) and
// Miranda's /api/webauthn/* endpoints (internal/webui/webauthn.go). Shared
// by the profile screen (registering a new passkey) and the login page
// (passwordless/usernameless sign-in). Also owns the browser-local "last
// login method" hint (see LOGIN_METHOD_KEY below) that login.js uses to
// decide which method to lead with.
//
// The server's JSON (github.com/go-webauthn/webauthn/protocol) encodes byte
// fields — challenge, user.id, credential ids — as base64url strings;
// navigator.credentials.create()/get() need real ArrayBuffers instead, and
// the reverse conversion applies to what they return. These are the only
// two directions that need converting.

function base64urlToBuffer(b64url) {
  const padded = b64url + "=".repeat((4 - (b64url.length % 4)) % 4);
  const base64 = padded.replace(/-/g, "+").replace(/_/g, "/");
  const raw = atob(base64);
  const bytes = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
  return bytes.buffer;
}

function bufferToBase64url(buffer) {
  const bytes = new Uint8Array(buffer);
  let str = "";
  for (const b of bytes) str += String.fromCharCode(b);
  return btoa(str).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function prepareCreationOptions(publicKey) {
  publicKey.challenge = base64urlToBuffer(publicKey.challenge);
  publicKey.user.id = base64urlToBuffer(publicKey.user.id);
  if (publicKey.excludeCredentials) {
    publicKey.excludeCredentials = publicKey.excludeCredentials.map((c) => ({ ...c, id: base64urlToBuffer(c.id) }));
  }
  return publicKey;
}

// PRF_SALT_B64URL is a fixed, non-secret, application-specific salt for the
// WebAuthn PRF extension's eval.first input — SHA-256("miranda:master-key:v1"),
// base64url-encoded (matches internal/keyring's docs/encryption.md). It
// doesn't need to be secret or per-user: PRF's security comes from the
// authenticator's own private key material, not this salt, which only
// domain-separates this specific derivation (like HKDF's "info" parameter).
// Requested client-side rather than round-tripped from the server to avoid
// a real encoding footgun — protocol.AuthenticationExtensions is untyped
// JSON, and a raw []byte value there would marshal as *standard* base64,
// not the base64url every other byte field in this protocol uses.
const PRF_SALT_B64URL = "1KUP_WqBdJvtc05WhVLE6ehg5_f950wDU5j-mzlvEnk";

function prepareRequestOptions(publicKey) {
  publicKey.challenge = base64urlToBuffer(publicKey.challenge);
  if (publicKey.allowCredentials) {
    publicKey.allowCredentials = publicKey.allowCredentials.map((c) => ({ ...c, id: base64urlToBuffer(c.id) }));
  }
  // Requested on every assertion (login and the post-registration PRF
  // probe below) — harmless no-op when the authenticator/browser doesn't
  // support PRF, or when internal/keyring isn't enabled server-side (the
  // output just goes unused in that case).
  publicKey.extensions = { ...(publicKey.extensions || {}), prf: { eval: { first: base64urlToBuffer(PRF_SALT_B64URL) } } };
  return publicKey;
}

// credentialToJSON mirrors the shape internal/webauthn's Service expects on
// finish — the same PublicKeyCredential JSON shape the WebAuthn spec itself
// defines, whether this came from create() (registration) or get() (login).
function credentialToJSON(credential) {
  const response = credential.response;
  const extensionResults = credential.getClientExtensionResults ? credential.getClientExtensionResults() : {};
  // The PRF extension's eval results arrive as raw ArrayBuffers, which
  // JSON.stringify silently serializes as "{}" (no enumerable own
  // properties) — encode them to base64url first, the same convention
  // every other binary field in this file already follows, or the server
  // never sees any bytes at all.
  if (extensionResults.prf && extensionResults.prf.results) {
    const results = extensionResults.prf.results;
    extensionResults.prf.results = {
      first: bufferToBase64url(results.first),
      ...(results.second ? { second: bufferToBase64url(results.second) } : {}),
    };
  }
  const out = {
    id: credential.id,
    rawId: bufferToBase64url(credential.rawId),
    type: credential.type,
    clientExtensionResults: extensionResults,
  };

  if (response.attestationObject) {
    out.response = {
      attestationObject: bufferToBase64url(response.attestationObject),
      clientDataJSON: bufferToBase64url(response.clientDataJSON),
      transports: response.getTransports ? response.getTransports() : [],
    };
  } else {
    out.response = {
      authenticatorData: bufferToBase64url(response.authenticatorData),
      clientDataJSON: bufferToBase64url(response.clientDataJSON),
      signature: bufferToBase64url(response.signature),
    };
    if (response.userHandle) out.response.userHandle = bufferToBase64url(response.userHandle);
  }
  return out;
}

/** Whether this browser/context supports WebAuthn at all — false on any
 * insecure (non-HTTPS, non-localhost) origin, which is exactly when the
 * passkey UI must stay hidden (see config.WebAuthnConfig's doc comment). */
export function isSupported() {
  return typeof window.PublicKeyCredential !== "undefined";
}

// LOGIN_METHOD_KEY backs a browser-local (not account-local) memory of
// which method last succeeded (or was last attempted, for "password" — see
// login.js's submit handler) from this browser — login.js's only way to
// guess, on the anonymous /login page, which method to lead with.
// Discoverable/usernameless login means the server can't tell who's about
// to sign in until the ceremony finishes, so there's no per-account signal
// to read here at all; a shared household device can carry one family
// member's choice into another member's visit, but that only costs them
// one click on the toggle button, never a dead end.
const LOGIN_METHOD_KEY = "miranda-last-login-method";

/** "passkey", "password", or null (no preference recorded yet) — see
 * LOGIN_METHOD_KEY. */
export function getLastLoginMethod() {
  try {
    return localStorage.getItem(LOGIN_METHOD_KEY);
  } catch {
    return null;
  }
}

// Best-effort: storage can throw in restricted contexts (e.g. Safari
// private browsing's quota), and the memory is a UX nicety, not required
// for correctness.
function setLastLoginMethod(method) {
  try {
    localStorage.setItem(LOGIN_METHOD_KEY, method);
  } catch {
    /* see doc comment above */
  }
}

/** Records a successful (or attempted — see login.js) passkey login, or a
 * successful passkey registration (profile screen), as the last-used
 * method. */
export function rememberPasskeyLogin() {
  setLastLoginMethod("passkey");
}

/** Records a password login attempt (login.js's form submit handler) as
 * the last-used method — see that handler for why an attempt, not just a
 * confirmed success, is what's recorded. */
export function rememberPasswordLogin() {
  setLastLoginMethod("password");
}

/** Clears the remembered method if it's "passkey" — called when the
 * profile screen finds this account has no passkeys left registered, since
 * that specific memory is now definitely stale for this account (leaves a
 * remembered "password" alone, which is still accurate either way). */
export function forgetPasskeyLogin() {
  try {
    if (localStorage.getItem(LOGIN_METHOD_KEY) === "passkey") {
      localStorage.removeItem(LOGIN_METHOD_KEY);
    }
  } catch {
    /* see setLastLoginMethod's doc comment */
  }
}

/** Runs the follow-up assertion ceremony that captures a just-registered
 * credential's PRF output for internal/keyring (see
 * internal/webauthn.Service.BeginKeyProbe's doc comment for why
 * registration alone can't reliably return it) — called by registerPasskey
 * immediately after registration succeeds. Deliberately fails soft: any
 * error here (no keyring configured server-side, the authenticator doesn't
 * support PRF, the ceremony itself fails) must never be treated as the
 * passkey registration having failed, since that already succeeded. Callers
 * that want to tell the user the outcome can inspect the resolved boolean.
 */
async function probeForEncryptionKey(credentialId) {
  try {
    const beginRes = await fetch("/api/webauthn/register/probe-begin", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ credentialId }),
    });
    if (!beginRes.ok) return false; // route not registered (keyring disabled) or begin failed
    const options = await beginRes.json();
    prepareRequestOptions(options.publicKey);

    const credential = await navigator.credentials.get({ publicKey: options.publicKey });
    const body = credentialToJSON(credential);
    body.credentialId = credentialId;

    const finishRes = await fetch("/api/webauthn/register/probe-finish", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    return finishRes.ok;
  } catch {
    return false;
  }
}

/** Registers a new passkey for the logged-in user (profile screen).
 * Resolves to {id, nickname, ...} plus one extra field this function adds,
 * encryptionKeyEnabled, telling the caller whether probeForEncryptionKey
 * successfully added a wrapped-key slot for this passkey — purely
 * informational, never affects whether registration itself succeeded. */
export async function registerPasskey(nickname) {
  const beginRes = await fetch("/api/webauthn/register/begin", { method: "POST" });
  if (!beginRes.ok) throw new Error(`begin failed: ${beginRes.status}`);
  const options = await beginRes.json();
  prepareCreationOptions(options.publicKey);

  const credential = await navigator.credentials.create({ publicKey: options.publicKey });
  const body = credentialToJSON(credential);
  body.nickname = nickname;

  const finishRes = await fetch("/api/webauthn/register/finish", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!finishRes.ok) throw new Error(`registration failed: ${finishRes.status} ${await finishRes.text()}`);
  rememberPasskeyLogin();
  const info = await finishRes.json();

  info.encryptionKeyEnabled = info.id ? await probeForEncryptionKey(info.id) : false;
  return info;
}

/** Drives a passwordless, usernameless login from the login page's
 * biometric button. Resolves to the JSON {redirect} on success. */
export async function loginWithPasskey() {
  const beginRes = await fetch("/api/webauthn/login/begin", { method: "POST" });
  if (!beginRes.ok) throw new Error(`begin failed: ${beginRes.status}`);
  const options = await beginRes.json();
  const ceremonyId = options.ceremonyId;
  prepareRequestOptions(options.publicKey);

  const credential = await navigator.credentials.get({ publicKey: options.publicKey });
  const body = credentialToJSON(credential);
  body.ceremonyId = ceremonyId;

  const finishRes = await fetch("/api/webauthn/login/finish", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!finishRes.ok) throw new Error(`login failed: ${finishRes.status}`);
  rememberPasskeyLogin();
  return finishRes.json();
}

export async function listCredentials() {
  const res = await fetch("/api/webauthn/credentials");
  if (!res.ok) throw new Error(`list failed: ${res.status}`);
  return res.json();
}

export async function deleteCredential(id) {
  const res = await fetch(`/api/webauthn/credentials/${encodeURIComponent(id)}`, { method: "DELETE" });
  if (!res.ok) throw new Error(`delete failed: ${res.status}`);
}
