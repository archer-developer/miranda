package webauthn

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	virtualwebauthn "github.com/descope/virtualwebauthn"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/users"
)

const testOrigin = "https://" + testRPID

// newTestService builds a real Service plus a registry with one "archer"
// user — the shared fixture for every ceremony test below.
func newTestService(t *testing.T) (*Service, *Store) {
	t.Helper()

	store := openTestStore(t)
	registry, err := users.NewRegistry([]config.UserConfig{
		{Username: "archer", PasswordHash: "x", FullName: "Archer"},
	})
	require.NoError(t, err)

	svc, err := NewService(testRPID, "Miranda Test", []string{testOrigin}, store, NewCeremonyStore(time.Minute), registry)
	require.NoError(t, err)
	return svc, store
}

// registerVirtualPasskey drives a full registration ceremony against svc
// using a simulated software authenticator (github.com/descope/virtualwebauthn),
// so tests exercise the library's real attestation verification rather than
// hand-crafted fixtures. Returns the authenticator/credential so the caller
// can go on to simulate a login with the same passkey.
func registerVirtualPasskey(t *testing.T, ctx context.Context, svc *Service, store *Store, username, ceremonyKey string) (virtualwebauthn.Authenticator, virtualwebauthn.Credential) {
	t.Helper()

	creation, err := svc.BeginRegistration(ctx, username, ceremonyKey)
	require.NoError(t, err)

	creationJSON, err := json.Marshal(creation)
	require.NoError(t, err)
	attestationOptions, err := virtualwebauthn.ParseAttestationOptions(string(creationJSON))
	require.NoError(t, err)

	handle, ok, err := store.UserHandle(ctx, testRPID, username)
	require.NoError(t, err)
	require.True(t, ok)

	rp := virtualwebauthn.RelyingParty{ID: testRPID, Name: "Miranda Test", Origin: testOrigin}
	authenticator := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserHandle: handle})
	credential := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	attestationResponse := virtualwebauthn.CreateAttestationResponse(rp, authenticator, credential, *attestationOptions)
	_, err = svc.FinishRegistration(ctx, username, ceremonyKey, "Test passkey", []byte(attestationResponse))
	require.NoError(t, err)

	authenticator.AddCredential(credential)
	return authenticator, credential
}

func TestService_RegistrationAndDiscoverableLoginRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestService(t)

	authenticator, credential := registerVirtualPasskey(t, ctx, svc, store, "archer", "session-token-1")

	creds, err := store.CredentialsForUser(ctx, testRPID, "archer")
	require.NoError(t, err)
	require.Len(t, creds, 1)

	assertion, ceremonyID, err := svc.BeginDiscoverableLogin(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, ceremonyID)

	assertionJSON, err := json.Marshal(assertion)
	require.NoError(t, err)
	assertionOptions, err := virtualwebauthn.ParseAssertionOptions(string(assertionJSON))
	require.NoError(t, err)

	credential.Counter++ // simulate a real authenticator incrementing on use
	rp := virtualwebauthn.RelyingParty{ID: testRPID, Name: "Miranda Test", Origin: testOrigin}
	assertionResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, credential, *assertionOptions)

	username, err := svc.FinishDiscoverableLogin(ctx, ceremonyID, []byte(assertionResponse))
	require.NoError(t, err)
	require.Equal(t, "archer", username)

	// The sign count must have been written back so clone detection works
	// on the next login.
	updated, err := store.CredentialsForUser(ctx, testRPID, "archer")
	require.NoError(t, err)
	require.Len(t, updated, 1)
	require.EqualValues(t, 1, updated[0].Authenticator.SignCount)

	list, err := store.ListForUser(ctx, testRPID, "archer")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NotEmpty(t, list[0].LastUsedAt, "last_used_at must be stamped after a successful login")
}

// TestService_DiscoverableLogin_SurvivesBackupEligibleFlip reproduces a
// real Android quirk (see Store.ReconcileFlags's doc comment): some Android
// platform authenticators report BackupEligible=false at the exact moment a
// passkey is registered, then BackupEligible=true on every login afterward
// once the credential finishes syncing to Google Password Manager. Without
// FinishDiscoverableLogin's reconcile step, go-webauthn's validateLogin
// hard-fails that second login with "Backup Eligible flag inconsistency
// detected during login validation" — the exact bug report this test
// guards against regressing.
func TestService_DiscoverableLogin_SurvivesBackupEligibleFlip(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestService(t)
	username := "archer"
	ceremonyKey := "session-token-1"

	creation, err := svc.BeginRegistration(ctx, username, ceremonyKey)
	require.NoError(t, err)
	creationJSON, err := json.Marshal(creation)
	require.NoError(t, err)
	attestationOptions, err := virtualwebauthn.ParseAttestationOptions(string(creationJSON))
	require.NoError(t, err)

	handle, ok, err := store.UserHandle(ctx, testRPID, username)
	require.NoError(t, err)
	require.True(t, ok)

	rp := virtualwebauthn.RelyingParty{ID: testRPID, Name: "Miranda Test", Origin: testOrigin}
	// BackupEligible: false at registration — simulating Android reporting
	// this before the credential has synced to the cloud.
	authenticator := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserHandle: handle, BackupEligible: false})
	credential := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	attestationResponse := virtualwebauthn.CreateAttestationResponse(rp, authenticator, credential, *attestationOptions)
	_, err = svc.FinishRegistration(ctx, username, ceremonyKey, "Android passkey", []byte(attestationResponse))
	require.NoError(t, err)
	authenticator.AddCredential(credential)

	stored, err := store.CredentialsForUser(ctx, testRPID, username)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.False(t, stored[0].Flags.BackupEligible, "precondition: registration stored BackupEligible=false")

	// Same authenticator, same credential — but now reporting
	// BackupEligible=true, as Android does once sync completes. This is
	// the "log out and log back in" step from the bug report.
	authenticator.Options.BackupEligible = true

	assertion, ceremonyID, err := svc.BeginDiscoverableLogin(ctx)
	require.NoError(t, err)
	assertionJSON, err := json.Marshal(assertion)
	require.NoError(t, err)
	assertionOptions, err := virtualwebauthn.ParseAssertionOptions(string(assertionJSON))
	require.NoError(t, err)

	credential.Counter++
	assertionResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, credential, *assertionOptions)

	loggedInUsername, err := svc.FinishDiscoverableLogin(ctx, ceremonyID, []byte(assertionResponse))
	require.NoError(t, err, "second login must not fail on a BackupEligible flag that legitimately changed after registration")
	require.Equal(t, username, loggedInUsername)

	updated, err := store.CredentialsForUser(ctx, testRPID, username)
	require.NoError(t, err)
	require.Len(t, updated, 1)
	require.True(t, updated[0].Flags.BackupEligible, "stored flag must catch up to the authenticator's current report")
}

func TestService_FinishRegistration_RejectsUnknownCeremonyKey(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)

	_, err := svc.FinishRegistration(ctx, "archer", "never-began", "nickname", []byte(`{}`))
	require.Error(t, err)
}

func TestService_FinishRegistration_CeremonyKeyIsConsumedOnUse(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestService(t)

	registerVirtualPasskey(t, ctx, svc, store, "archer", "session-token-1")

	// A second finish with the same (already-consumed) ceremony key must fail.
	_, err := svc.FinishRegistration(ctx, "archer", "session-token-1", "again", []byte(`{}`))
	require.Error(t, err)
}

func TestService_FinishDiscoverableLogin_RejectsUnknownCredential(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)

	assertion, ceremonyID, err := svc.BeginDiscoverableLogin(ctx)
	require.NoError(t, err)

	assertionJSON, err := json.Marshal(assertion)
	require.NoError(t, err)
	assertionOptions, err := virtualwebauthn.ParseAssertionOptions(string(assertionJSON))
	require.NoError(t, err)

	// Nobody ever registered this credential/authenticator.
	rp := virtualwebauthn.RelyingParty{ID: testRPID, Name: "Miranda Test", Origin: testOrigin}
	rogueAuth := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserHandle: []byte("someone-else")})
	rogueCred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	rogueAuth.AddCredential(rogueCred)

	assertionResponse := virtualwebauthn.CreateAssertionResponse(rp, rogueAuth, rogueCred, *assertionOptions)

	_, err = svc.FinishDiscoverableLogin(ctx, ceremonyID, []byte(assertionResponse))
	require.Error(t, err)
}

func TestService_ListAndDeleteCredential(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestService(t)

	_, credential := registerVirtualPasskey(t, ctx, svc, store, "archer", "session-token-1")

	list, err := svc.ListCredentials(ctx, "archer")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "Test passkey", list[0].Nickname)

	require.NoError(t, svc.DeleteCredential(ctx, "archer", credential.ID))

	list, err = svc.ListCredentials(ctx, "archer")
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestNewService_FailsWithoutOrigins(t *testing.T) {
	store := openTestStore(t)
	registry, err := users.NewRegistry(nil)
	require.NoError(t, err)

	_, err = NewService(testRPID, "Miranda Test", nil, store, NewCeremonyStore(time.Minute), registry)
	require.Error(t, err)
}
