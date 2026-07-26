package webauthn

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/go-webauthn/webauthn/protocol"
	webauthnlib "github.com/go-webauthn/webauthn/webauthn"

	"github.com/archer-developer/miranda/internal/users"
)

// Service orchestrates WebAuthn registration/login ceremonies: it wraps the
// go-webauthn/webauthn library, this package's Store (credential
// persistence) and CeremonyStore (transient challenge state), and the app's
// users.Registry. internal/webui only ever talks to Service — it never
// imports the webauthn library or this package's other types directly.
type Service struct {
	rp       *webauthnlib.WebAuthn
	rpid     string
	store    *Store
	ceremony *CeremonyStore
	users    *users.Registry
}

// NewService builds a Service for one Relying Party configuration. Fails if
// rpOrigins is empty — the library itself refuses to construct without at
// least one, and there's no safe default (see config.WebAuthnConfig).
func NewService(rpID, rpDisplayName string, rpOrigins []string, store *Store, ceremony *CeremonyStore, registry *users.Registry) (*Service, error) {
	requireResidentKey := true
	rp, err := webauthnlib.New(&webauthnlib.Config{
		RPID:          rpID,
		RPDisplayName: rpDisplayName,
		RPOrigins:     rpOrigins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			// Every registered credential must be discoverable/resident, or
			// login (a passwordless, usernameless flow — see
			// BeginDiscoverableLogin) has nothing to find. Set once here
			// rather than per registration call, so there's nothing to
			// forget at a future call site. RequireResidentKey is the
			// legacy WebAuthn L1 mirror of the same requirement, kept for
			// clients that only read that field.
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: &requireResidentKey,
			// The actual "biometric/PIN gate" — without this, a registered
			// authenticator could satisfy the ceremony with mere possession
			// (tapping a key), not verification (Face ID/Touch ID/PIN).
			UserVerification: protocol.VerificationRequired,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("webauthn: configure relying party: %w", err)
	}
	return &Service{rp: rp, rpid: rpID, store: store, ceremony: ceremony, users: registry}, nil
}

// BeginRegistration starts a "add a passkey" ceremony for an already
// logged-in username, keyed by ceremonyKey (the caller's session token —
// registration only ever happens from an authenticated profile screen, so
// there's already a stable per-session key to use; no separate ceremony id
// needs to round-trip to the client). Excludes the user's already-registered
// credentials so re-registering the same authenticator is rejected
// client-side instead of silently duplicating.
func (s *Service) BeginRegistration(ctx context.Context, username, ceremonyKey string) (*protocol.CredentialCreation, error) {
	cu, err := s.loadUser(ctx, username)
	if err != nil {
		return nil, err
	}

	exclude := make([]protocol.CredentialDescriptor, len(cu.credentials))
	for i := range cu.credentials {
		exclude[i] = cu.credentials[i].Descriptor()
	}

	creation, sessionData, err := s.rp.BeginRegistration(cu, webauthnlib.WithExclusions(exclude))
	if err != nil {
		return nil, fmt.Errorf("webauthn: begin registration: %w", err)
	}
	s.ceremony.Put(ceremonyKey, *sessionData)
	return creation, nil
}

// FinishRegistration completes a registration ceremony: body is the raw
// (still-unparsed) request body from the browser's
// navigator.credentials.create() response — the caller may have already
// peeled a sibling field (e.g. a nickname) out of a copy of these same
// bytes; this method reconstructs its own throwaway *http.Request from them
// rather than requiring the caller to manage a shared, single-read
// http.Request.Body.
func (s *Service) FinishRegistration(ctx context.Context, username, ceremonyKey, nickname string, body []byte) (CredentialInfo, error) {
	sessionData, ok := s.ceremony.Take(ceremonyKey)
	if !ok {
		return CredentialInfo{}, fmt.Errorf("webauthn: registration ceremony expired or not found")
	}

	cu, err := s.loadUser(ctx, username)
	if err != nil {
		return CredentialInfo{}, err
	}

	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if err != nil {
		return CredentialInfo{}, fmt.Errorf("webauthn: build verification request: %w", err)
	}

	cred, err := s.rp.FinishRegistration(cu, sessionData, req)
	if err != nil {
		return CredentialInfo{}, fmt.Errorf("webauthn: finish registration: %w", err)
	}

	if err := s.store.SaveCredential(ctx, s.rpid, username, nickname, cred); err != nil {
		return CredentialInfo{}, err
	}

	return CredentialInfo{Nickname: nickname}, nil
}

// BeginDiscoverableLogin starts a passwordless, usernameless login ceremony
// for the login page's biometric button — the browser prompts its own
// platform authenticator (Face ID/Touch ID/Windows Hello) to pick from
// whichever resident credentials it holds for this Relying Party, without
// Miranda ever being told who's attempting to log in until FinishDiscoverableLogin.
// Returns a ceremony id the client must echo back on finish (there's no
// session cookie yet to key the pending challenge by).
func (s *Service) BeginDiscoverableLogin(ctx context.Context) (*protocol.CredentialAssertion, string, error) {
	assertion, sessionData, err := s.rp.BeginDiscoverableLogin()
	if err != nil {
		return nil, "", fmt.Errorf("webauthn: begin login: %w", err)
	}
	ceremonyID, err := s.ceremony.PutNew(*sessionData)
	if err != nil {
		return nil, "", err
	}
	return assertion, ceremonyID, nil
}

// FinishDiscoverableLogin completes a passwordless login ceremony, resolving
// which user it belongs to purely from the credential id the authenticator
// returns (see Store.LookupByCredentialID), and returns that username on
// success. body is the raw request body, same convention as
// FinishRegistration. Always writes back the credential's mutated
// sign-count/flags on success — skipping that defeats clone detection.
func (s *Service) FinishDiscoverableLogin(ctx context.Context, ceremonyID string, body []byte) (string, error) {
	sessionData, ok := s.ceremony.Take(ceremonyID)
	if !ok {
		return "", fmt.Errorf("webauthn: login ceremony expired or not found")
	}

	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("webauthn: build verification request: %w", err)
	}

	handler := func(rawID, userHandle []byte) (webauthnlib.User, error) {
		username, found, err := s.store.LookupByCredentialID(ctx, s.rpid, rawID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("webauthn: unrecognized credential")
		}
		cu, err := s.loadUser(ctx, username)
		if err != nil {
			return nil, err
		}
		return cu, nil
	}

	user, cred, err := s.rp.FinishPasskeyLogin(handler, sessionData, req)
	if err != nil {
		return "", fmt.Errorf("webauthn: finish login: %w", err)
	}

	if err := s.store.UpdateSignCount(ctx, s.rpid, cred.ID, cred.Authenticator.SignCount, cred.Authenticator.CloneWarning, cred.Flags); err != nil {
		return "", err
	}

	return user.WebAuthnName(), nil
}

// ListCredentials returns display-safe info for every passkey username has
// registered, for the profile screen's "manage passkeys" list.
func (s *Service) ListCredentials(ctx context.Context, username string) ([]CredentialInfo, error) {
	return s.store.ListForUser(ctx, s.rpid, username)
}

// DeleteCredential removes one of username's passkeys by its base64url-less
// raw id.
func (s *Service) DeleteCredential(ctx context.Context, username string, credentialID []byte) error {
	return s.store.DeleteCredential(ctx, s.rpid, username, credentialID)
}

// loadUser resolves username against the users.Registry and its stored
// WebAuthn handle/credentials into the adapter the library's API needs.
func (s *Service) loadUser(ctx context.Context, username string) (credentialUser, error) {
	u, ok := s.users.Get(username)
	if !ok {
		return credentialUser{}, fmt.Errorf("webauthn: unknown user %q", username)
	}

	handle, err := s.store.EnsureUserHandle(ctx, s.rpid, username)
	if err != nil {
		return credentialUser{}, err
	}

	creds, err := s.store.CredentialsForUser(ctx, s.rpid, username)
	if err != nil {
		return credentialUser{}, err
	}

	return credentialUser{User: u, handle: handle, credentials: creds}, nil
}
