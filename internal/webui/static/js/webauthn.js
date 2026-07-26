// Vanilla-JS glue between the browser's native WebAuthn API
// (navigator.credentials — no library needed on this side at all) and
// Miranda's /api/webauthn/* endpoints (internal/webui/webauthn.go). Shared
// by the profile screen (registering a new passkey) and the login page
// (passwordless/usernameless sign-in).
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

function prepareRequestOptions(publicKey) {
  publicKey.challenge = base64urlToBuffer(publicKey.challenge);
  if (publicKey.allowCredentials) {
    publicKey.allowCredentials = publicKey.allowCredentials.map((c) => ({ ...c, id: base64urlToBuffer(c.id) }));
  }
  return publicKey;
}

// credentialToJSON mirrors the shape internal/webauthn's Service expects on
// finish — the same PublicKeyCredential JSON shape the WebAuthn spec itself
// defines, whether this came from create() (registration) or get() (login).
function credentialToJSON(credential) {
  const response = credential.response;
  const out = {
    id: credential.id,
    rawId: bufferToBase64url(credential.rawId),
    type: credential.type,
    clientExtensionResults: credential.getClientExtensionResults ? credential.getClientExtensionResults() : {},
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

/** Registers a new passkey for the logged-in user (profile screen). */
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
  return finishRes.json();
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
