package webauthn

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	webauthnlib "github.com/go-webauthn/webauthn/webauthn"
	"github.com/stretchr/testify/require"
)

const testRPID = "miranda.example.com"

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "webauthn.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func testCredential() *webauthnlib.Credential {
	cred := &webauthnlib.Credential{
		ID:                []byte{1, 2, 3, 4},
		PublicKey:         []byte{5, 6, 7, 8},
		AttestationType:   "none",
		AttestationFormat: "packed",
		Transport:         []protocol.AuthenticatorTransport{protocol.USB, protocol.Internal},
		Flags:             webauthnlib.NewCredentialFlags(protocol.FlagUserPresent | protocol.FlagUserVerified),
	}
	cred.Authenticator.AAGUID = []byte{9, 9, 9}
	cred.Authenticator.SignCount = 1
	cred.Authenticator.CloneWarning = false
	cred.Authenticator.Attachment = protocol.Platform
	cred.Attestation.ClientDataJSON = []byte(`{"type":"webauthn.create"}`)
	cred.Attestation.ClientDataHash = []byte{1, 1, 1}
	cred.Attestation.AuthenticatorData = []byte{2, 2, 2}
	cred.Attestation.PublicKeyAlgorithm = -7
	cred.Attestation.Object = []byte{3, 3, 3}
	return cred
}

func TestEnsureUserHandle_GeneratesOnceAndIsStable(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	handle1, err := s.EnsureUserHandle(ctx, testRPID, "archer")
	require.NoError(t, err)
	require.Len(t, handle1, userHandleBytes)

	handle2, err := s.EnsureUserHandle(ctx, testRPID, "archer")
	require.NoError(t, err)
	require.Equal(t, handle1, handle2, "must return the same handle on repeat calls, not regenerate")
}

func TestUserHandle_ReturnsFalseWhenNotYetCreated(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	_, ok, err := s.UserHandle(ctx, testRPID, "archer")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestSaveAndCredentialsForUser_RoundTripsEveryField(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	original := testCredential()
	require.NoError(t, s.SaveCredential(ctx, testRPID, "archer", "iPhone", original))

	got, err := s.CredentialsForUser(ctx, testRPID, "archer")
	require.NoError(t, err)
	require.Len(t, got, 1)

	require.Equal(t, original.ID, got[0].ID)
	require.Equal(t, original.PublicKey, got[0].PublicKey)
	require.Equal(t, original.AttestationType, got[0].AttestationType)
	require.Equal(t, original.AttestationFormat, got[0].AttestationFormat)
	require.ElementsMatch(t, original.Transport, got[0].Transport)
	require.Equal(t, original.Flags, got[0].Flags)
	require.Equal(t, original.Authenticator, got[0].Authenticator)
	require.Equal(t, original.Attestation, got[0].Attestation)
}

func TestCredentialsForUser_ScopedByUsername(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	require.NoError(t, s.SaveCredential(ctx, testRPID, "archer", "iPhone", testCredential()))

	got, err := s.CredentialsForUser(ctx, testRPID, "anna")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestListForUser_ReturnsDisplaySafeInfoNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	first := testCredential()
	first.ID = []byte{1}
	require.NoError(t, s.SaveCredential(ctx, testRPID, "archer", "old phone", first))

	second := testCredential()
	second.ID = []byte{2}
	require.NoError(t, s.SaveCredential(ctx, testRPID, "archer", "new phone", second))

	list, err := s.ListForUser(ctx, testRPID, "archer")
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "new phone", list[0].Nickname)
	require.Equal(t, "old phone", list[1].Nickname)
	require.Contains(t, list[0].Transports, "usb")

	// ID must be the base64url encoding of the credential's raw id, so the
	// frontend can round-trip it straight into a delete request.
	require.Equal(t, base64.RawURLEncoding.EncodeToString(second.ID), list[0].ID)
}

func TestDeleteCredential_ScopedByUsername(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	cred := testCredential()
	require.NoError(t, s.SaveCredential(ctx, testRPID, "archer", "iPhone", cred))

	// A different user can't delete archer's credential.
	require.NoError(t, s.DeleteCredential(ctx, testRPID, "anna", cred.ID))
	remaining, err := s.CredentialsForUser(ctx, testRPID, "archer")
	require.NoError(t, err)
	require.Len(t, remaining, 1)

	require.NoError(t, s.DeleteCredential(ctx, testRPID, "archer", cred.ID))
	remaining, err = s.CredentialsForUser(ctx, testRPID, "archer")
	require.NoError(t, err)
	require.Empty(t, remaining)
}

func TestLookupByCredentialID_FindsOwner(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	cred := testCredential()
	require.NoError(t, s.SaveCredential(ctx, testRPID, "archer", "iPhone", cred))

	username, found, err := s.LookupByCredentialID(ctx, testRPID, cred.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "archer", username)

	_, found, err = s.LookupByCredentialID(ctx, testRPID, []byte{99, 99, 99})
	require.NoError(t, err)
	require.False(t, found)
}

func TestUpdateSignCount_PersistsMutatedFields(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	cred := testCredential()
	require.NoError(t, s.SaveCredential(ctx, testRPID, "archer", "iPhone", cred))

	newFlags := webauthnlib.NewCredentialFlags(protocol.FlagUserPresent | protocol.FlagUserVerified | protocol.FlagBackupEligible | protocol.FlagBackupState)
	require.NoError(t, s.UpdateSignCount(ctx, testRPID, cred.ID, 42, true, newFlags))

	got, err := s.CredentialsForUser(ctx, testRPID, "archer")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.EqualValues(t, 42, got[0].Authenticator.SignCount)
	require.True(t, got[0].Authenticator.CloneWarning)
	require.Equal(t, newFlags, got[0].Flags)
}
