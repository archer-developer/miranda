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

	username, err := h.webauthn.FinishDiscoverableLogin(r.Context(), req.CeremonyID, body)
	if err != nil {
		http.Error(w, "login failed: "+err.Error(), http.StatusUnauthorized)
		return
	}

	user, ok := h.users.Get(username)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
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
	w.WriteHeader(http.StatusNoContent)
}
