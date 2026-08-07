package webui

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-webauthn/webauthn/protocol"

	"github.com/archer-developer/miranda/internal/session"
)

// handleWebAuthnRegisterBegin starts an "add a passkey" ceremony for the
// logged-in user, from the profile screen. Keyed by the caller's own
// session token — registration only ever happens once already
// authenticated, so there's already a stable per-request key to stash the
// pending ceremony under; no separate ceremony id needs to round-trip to
// the client the way login's does (see handleWebAuthnLoginBegin).
func (h *Handler) handleWebAuthnRegisterBegin(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	cookie, err := r.Cookie(session.CookieName)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	creation, err := h.webauthn.BeginRegistration(r.Context(), user.Username, cookie.Value)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, creation)
}

// webauthnRegisterFinishRequest is the browser's PublicKeyCredential JSON
// from navigator.credentials.create(), plus one sibling field this
// endpoint needs and the WebAuthn spec doesn't define. Only Nickname is
// read here — the rest of the body is re-read whole by
// webauthn.Service.FinishRegistration, which needs the full standard
// shape, not just this one field.
type webauthnRegisterFinishRequest struct {
	Nickname string `json:"nickname"`
}

// handleWebAuthnRegisterFinish completes a passkey registration ceremony.
func (h *Handler) handleWebAuthnRegisterFinish(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	cookie, err := r.Cookie(session.CookieName)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	var req webauthnRegisterFinishRequest
	_ = json.Unmarshal(body, &req) // nickname is optional; malformed JSON surfaces below anyway

	info, err := h.webauthn.FinishRegistration(r.Context(), user.Username, cookie.Value, req.Nickname, body)
	if err != nil {
		h.logger.Warn("webauthn: registration failed",
			"error", err, "username", user.Username, "remote_addr", r.RemoteAddr, "user_agent", r.UserAgent())
		http.Error(w, "registration failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, info)
}

// handleWebAuthnLoginBegin starts a passwordless, usernameless login
// ceremony for the login page's biometric button. Unauthenticated by
// design — this *is* the login mechanism.
func (h *Handler) handleWebAuthnLoginBegin(w http.ResponseWriter, r *http.Request) {
	assertion, ceremonyID, err := h.webauthn.BeginDiscoverableLogin(r.Context())
	if err != nil {
		h.logger.Warn("webauthn: discoverable login begin failed",
			"error", err, "remote_addr", r.RemoteAddr, "user_agent", r.UserAgent())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, webauthnLoginBeginResponse{CeremonyID: ceremonyID, CredentialAssertion: assertion})
}

// webauthnLoginBeginResponse embeds the standard WebAuthn assertion options
// at the top level (so the browser's navigator.credentials.get() can use
// the JSON as-is) alongside the one sibling field — the ceremony id — the
// client must echo back on finish, since there's no session cookie yet to
// key the pending challenge by.
type webauthnLoginBeginResponse struct {
	CeremonyID string `json:"ceremonyId"`
	*protocol.CredentialAssertion
}

// handleWebAuthnLoginFinish completes a passwordless login ceremony and, on
// success, issues a session cookie exactly like password login does (see
// auth.go's issueSessionCookie) — the two auth methods share that one code
// path so they can never drift apart.
func (h *Handler) handleWebAuthnLoginFinish(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	var req struct {
		CeremonyID string `json:"ceremonyId"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.CeremonyID == "" {
		http.Error(w, "missing ceremonyId", http.StatusBadRequest)
		return
	}

	username, credentialID, prfOutput, err := h.webauthn.FinishDiscoverableLogin(r.Context(), req.CeremonyID, body)
	if err != nil {
		// The client only ever shows a generic "biometric sign-in failed"
		// message (see login.js) — this is the only place the actual
		// failure reason (which credential, which validation step) is
		// visible at all, which matters a lot for a ceremony this hard to
		// reproduce from a desk (it depends on a specific phone's
		// authenticator/OS behavior, e.g. Android's Google Password
		// Manager — see Store.ReconcileFlags for one such quirk already
		// worked around).
		h.logger.Warn("webauthn: discoverable login failed",
			"error", err, "remote_addr", r.RemoteAddr, "user_agent", r.UserAgent())
		http.Error(w, "login failed: "+err.Error(), http.StatusUnauthorized)
		return
	}

	user, ok := h.users.Get(username)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Best-effort, same reasoning as handleLoginSubmit's password path:
	// prfOutput is nil whenever the authenticator/browser doesn't support
	// PRF, which UnlockWithPRF treats as a normal no-op, not an error.
	if h.keyring != nil {
		if err := h.keyring.UnlockWithPRF(r.Context(), username, credentialID, prfOutput); err != nil {
			h.logger.Warn("keyring: unlock with prf failed", "username", username, "error", err)
		}
	}

	if err := h.issueSessionCookie(w, r, user); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"redirect": "/"})
}

// handleWebAuthnListCredentials lists the logged-in user's registered
// passkeys for the profile screen's "manage passkeys" section.
func (h *Handler) handleWebAuthnListCredentials(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	list, err := h.webauthn.ListCredentials(r.Context(), user.Username)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, list)
}

// handleWebAuthnDeleteCredential removes one of the logged-in user's own
// passkeys — Service.DeleteCredential scopes the delete by username, so
// this can never remove another account's credential even given a guessed id.
func (h *Handler) handleWebAuthnDeleteCredential(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id, err := base64.RawURLEncoding.DecodeString(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid credential id", http.StatusBadRequest)
		return
	}

	if err := h.webauthn.DeleteCredential(r.Context(), user.Username, id); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Best-effort: drop this credential's now-orphaned wrapped-key slot too,
	// if it had one. Never fails the delete itself — the passkey is already
	// gone from the auth flow at this point regardless.
	if h.keyring != nil {
		if err := h.keyring.RemoveSlotForCredential(r.Context(), user.Username, id); err != nil {
			h.logger.Warn("keyring: remove slot for deleted credential failed", "username", user.Username, "error", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// webauthnProbeRequest carries which credential a probe ceremony targets —
// the request body for probe-begin, and one sibling field alongside the
// full navigator.credentials.get() response for probe-finish, same
// convention as webauthnRegisterFinishRequest's nickname field.
type webauthnProbeRequest struct {
	CredentialID string `json:"credentialId"` // base64url, matches CredentialInfo.ID
}

// parseProbeRequest reads and validates the request body shared by
// handleWebAuthnRegisterProbeBegin/Finish: the logged-in user, the session
// cookie (ceremonies are keyed by it, same as registration), the raw body
// (probe-finish needs it whole, to hand to FinishKeyProbe), and the decoded
// credentialId. Writes the appropriate HTTP error and returns ok=false on
// any failure — callers should return immediately in that case.
func (h *Handler) parseProbeRequest(w http.ResponseWriter, r *http.Request) (username, ceremonyKey string, body, credentialID []byte, ok bool) {
	user := currentUser(r)
	cookie, err := r.Cookie(session.CookieName)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", "", nil, nil, false
	}

	body, err = io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return "", "", nil, nil, false
	}
	var req webauthnProbeRequest
	if err := json.Unmarshal(body, &req); err != nil || req.CredentialID == "" {
		http.Error(w, "missing credentialId", http.StatusBadRequest)
		return "", "", nil, nil, false
	}
	credentialID, err = base64.RawURLEncoding.DecodeString(req.CredentialID)
	if err != nil {
		http.Error(w, "invalid credentialId", http.StatusBadRequest)
		return "", "", nil, nil, false
	}
	return user.Username, cookie.Value, body, credentialID, true
}

// handleWebAuthnRegisterProbeBegin starts the follow-up assertion ceremony
// (see internal/webauthn.Service.BeginKeyProbe) that captures a
// just-registered passkey's PRF output, since registration alone doesn't
// reliably return it across every browser/authenticator. Called by
// webauthn.js's registerPasskey() immediately after register/finish
// succeeds — a failure here must never be treated as the passkey
// registration itself failing (see handleWebAuthnRegisterProbeFinish).
func (h *Handler) handleWebAuthnRegisterProbeBegin(w http.ResponseWriter, r *http.Request) {
	username, ceremonyKey, _, credentialID, ok := h.parseProbeRequest(w, r)
	if !ok {
		return
	}

	assertion, err := h.webauthn.BeginKeyProbe(r.Context(), username, ceremonyKey, credentialID)
	if err != nil {
		h.logger.Warn("webauthn: begin key probe failed", "username", username, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, assertion)
}

// handleWebAuthnRegisterProbeFinish completes the probe ceremony and hands
// its PRF output (if any) to KeyringService.AddPasskeySlot. Any failure here
// — the ceremony itself, or AddPasskeySlot's ErrNotUnlocked when the caller's
// session never bootstrapped a master key — is reported to the client so
// the profile screen can say "this passkey works for login, but not for
// encrypted-data unlock," but is deliberately never surfaced as the passkey
// registration having failed; that already succeeded before this ever ran.
func (h *Handler) handleWebAuthnRegisterProbeFinish(w http.ResponseWriter, r *http.Request) {
	username, ceremonyKey, body, _, ok := h.parseProbeRequest(w, r)
	if !ok {
		return
	}

	// credentialID here is the id FinishKeyProbe cryptographically validated
	// the assertion against — deliberately not the credentialId field
	// parseProbeRequest also decoded from this same request body, which is
	// just an unverified client claim. Binding the keyring slot to that
	// claim instead would let a client that legitimately owns credential A
	// mislabel its PRF output as belonging to credential B.
	credentialID, prfOutput, err := h.webauthn.FinishKeyProbe(r.Context(), username, ceremonyKey, body)
	if err != nil {
		h.logger.Warn("webauthn: finish key probe failed", "username", username, "error", err)
		http.Error(w, "key probe failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(prfOutput) == 0 {
		http.Error(w, "authenticator did not return a prf result", http.StatusUnprocessableEntity)
		return
	}

	if h.keyring == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.keyring.AddPasskeySlot(r.Context(), username, credentialID, prfOutput); err != nil {
		h.logger.Warn("keyring: add passkey slot failed", "username", username, "error", err)
		http.Error(w, "add passkey slot failed: "+err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
