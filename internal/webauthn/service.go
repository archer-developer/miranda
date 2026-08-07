package webauthn

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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

	// ID lets the caller (internal/webui) target the follow-up PRF probe
	// ceremony (BeginKeyProbe/FinishKeyProbe) at this specific credential —
	// see internal/keyring for why registration alone can't reliably
	// capture PRF output across every browser/authenticator.
	return CredentialInfo{ID: base64.RawURLEncoding.EncodeToString(cred.ID), Nickname: nickname}, nil
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
//
// Also returns the credential id used and, if the client requested and the
// authenticator/browser supports it, this assertion's PRF extension output
// (see webauthn.js's prepareRequestOptions — the client, not this method,
// is what requests prf.eval) — nil credentialID/prfOutput on any ordinary
// failure path, and a nil (not an error) prfOutput whenever PRF simply
// wasn't available for this login, since that's a normal, expected case
// (see internal/keyring.Service.UnlockWithPRF).
func (s *Service) FinishDiscoverableLogin(ctx context.Context, ceremonyID string, body []byte) (username string, credentialID, prfOutput []byte, err error) {
	sessionData, ok := s.ceremony.Take(ceremonyID)
	if !ok {
		return "", nil, nil, fmt.Errorf("webauthn: login ceremony expired or not found")
	}

	// Resync our stored flags to this assertion's actual flags before the
	// library's own validation runs — see Store.ReconcileFlags for the
	// Android BackupEligible quirk this works around (github.com/go-webauthn/
	// webauthn#335, #351). Parsing here is a peek, not the authoritative
	// parse — FinishPasskeyLogin below parses body again itself (from a
	// fresh reader) and does the real signature/challenge verification; a
	// parse failure here just means nothing to reconcile, and that same
	// malformed body fails again there with a proper error. Reused below to
	// also read the PRF extension output, since it's already parsed.
	parsed, perr := protocol.ParseCredentialRequestResponseBytes(body)
	if perr == nil {
		if err := s.store.ReconcileFlags(ctx, s.rpid, parsed.RawID, parsed.Response.AuthenticatorData.Flags); err != nil {
			return "", nil, nil, err
		}
	}

	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if err != nil {
		return "", nil, nil, fmt.Errorf("webauthn: build verification request: %w", err)
	}

	handler := func(rawID, userHandle []byte) (webauthnlib.User, error) {
		u, found, err := s.store.LookupByCredentialID(ctx, s.rpid, rawID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("webauthn: unrecognized credential")
		}
		cu, err := s.loadUser(ctx, u)
		if err != nil {
			return nil, err
		}
		return cu, nil
	}

	user, cred, err := s.rp.FinishPasskeyLogin(handler, sessionData, req)
	if err != nil {
		return "", nil, nil, fmt.Errorf("webauthn: finish login: %w", err)
	}

	if err := s.store.UpdateSignCount(ctx, s.rpid, cred.ID, cred.Authenticator.SignCount, cred.Authenticator.CloneWarning, cred.Flags); err != nil {
		return "", nil, nil, err
	}

	if perr == nil {
		prfOutput = extractPRFOutput(parsed.ClientExtensionResults)
	}
	return user.WebAuthnName(), cred.ID, prfOutput, nil
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

// BeginKeyProbe starts a follow-up assertion ceremony scoped to exactly one
// (typically just-registered) credential, so internal/keyring can capture
// its PRF extension output — registration ceremonies don't reliably return
// PRF eval results across every browser/authenticator, so the standard
// workaround is this immediate follow-up login-style ceremony instead.
// Unlike BeginDiscoverableLogin, the caller is already authenticated and
// knows both username and credentialID, so this is keyed by the caller's
// own session token exactly like BeginRegistration is, not a separate
// ceremony id.
func (s *Service) BeginKeyProbe(ctx context.Context, username, ceremonyKey string, credentialID []byte) (*protocol.CredentialAssertion, error) {
	cu, err := s.loadUser(ctx, username)
	if err != nil {
		return nil, err
	}

	cred, ok := credentialByID(cu.credentials, credentialID)
	if !ok {
		return nil, fmt.Errorf("webauthn: credential not found for probe")
	}

	// Restrict the assertion to just this one credential — WithAllowedCredentials
	// keeps the browser from prompting for a different passkey than the one
	// that was just registered.
	assertion, sessionData, err := s.rp.BeginLogin(cu, webauthnlib.WithAllowedCredentials([]protocol.CredentialDescriptor{cred.Descriptor()}))
	if err != nil {
		return nil, fmt.Errorf("webauthn: begin key probe: %w", err)
	}
	s.ceremony.Put(ceremonyKey, *sessionData)
	return assertion, nil
}

// FinishKeyProbe completes a BeginKeyProbe ceremony and returns the id of
// the credential the assertion was actually cryptographically validated
// against (cred.ID from FinishLogin — never the client's own claimed
// credentialId, which parseProbeRequest reads from a separate, unverified
// field of the same JSON body and which the caller must not use for
// anything security-sensitive), plus the PRF extension output the assertion
// carried, if any (nil, not an error, if the authenticator/browser doesn't
// support PRF — see webauthn.js's prepareRequestOptions for what requests
// it). Designed to fail soft at the caller: internal/webui must treat a
// probe failure as "this passkey works for login but not for encrypted-data
// unlock," never as undoing the already-successful passkey registration
// that preceded it.
func (s *Service) FinishKeyProbe(ctx context.Context, username, ceremonyKey string, body []byte) (credentialID, prfOutput []byte, err error) {
	sessionData, ok := s.ceremony.Take(ceremonyKey)
	if !ok {
		return nil, nil, fmt.Errorf("webauthn: key probe ceremony expired or not found")
	}

	cu, err := s.loadUser(ctx, username)
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("webauthn: build verification request: %w", err)
	}

	cred, err := s.rp.FinishLogin(cu, sessionData, req)
	if err != nil {
		return nil, nil, fmt.Errorf("webauthn: finish key probe: %w", err)
	}

	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		return nil, nil, fmt.Errorf("webauthn: parse key probe response: %w", err)
	}
	return cred.ID, extractPRFOutput(parsed.ClientExtensionResults), nil
}

func credentialByID(creds []webauthnlib.Credential, id []byte) (webauthnlib.Credential, bool) {
	for _, c := range creds {
		if bytes.Equal(c.ID, id) {
			return c, true
		}
	}
	return webauthnlib.Credential{}, false
}

// prfExtensionOutput is the shape of the "prf" entry inside a parsed
// response's ClientExtensionResults (an untyped map[string]any — the
// go-webauthn library has no typed support for the PRF extension). Results.First
// is base64url text, not a raw JSON binary type — see webauthn.js's
// credentialToJSON for why (a browser ArrayBuffer would otherwise silently
// serialize as "{}" over the wire).
type prfExtensionOutput struct {
	Enabled bool `json:"enabled,omitempty"`
	Results *struct {
		First string `json:"first"`
	} `json:"results,omitempty"`
}

// extractPRFOutput reads and decodes the PRF extension's "first" eval
// result from a parsed response's ClientExtensionResults, if present.
// Returns nil (not an error) whenever PRF wasn't requested, isn't
// supported, or the response is malformed — an absent master-key-unlock
// capability is always a normal, expected case here, never a login failure.
func extractPRFOutput(results protocol.AuthenticationExtensionsClientOutputs) []byte {
	if results == nil {
		return nil
	}
	raw, ok := results["prf"]
	if !ok {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out prfExtensionOutput
	if err := json.Unmarshal(data, &out); err != nil || out.Results == nil || out.Results.First == "" {
		return nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(out.Results.First)
	if err != nil {
		return nil
	}
	return decoded
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
