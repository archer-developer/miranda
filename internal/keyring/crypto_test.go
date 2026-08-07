package keyring

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWrapUnwrap_RoundTrips(t *testing.T) {
	key, err := GenerateMasterKey()
	require.NoError(t, err)
	plaintext := []byte("super secret master key material")
	aad := slotAAD("archer", SlotTypePassword, "")

	ciphertext, nonce, err := wrap(key, plaintext, aad)
	require.NoError(t, err)

	got, err := unwrap(key, ciphertext, nonce, aad)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
}

func TestUnwrap_FailsOnWrongKey(t *testing.T) {
	key, err := GenerateMasterKey()
	require.NoError(t, err)
	wrongKey, err := GenerateMasterKey()
	require.NoError(t, err)
	aad := slotAAD("archer", SlotTypePassword, "")

	ciphertext, nonce, err := wrap(key, []byte("plaintext"), aad)
	require.NoError(t, err)

	_, err = unwrap(wrongKey, ciphertext, nonce, aad)
	require.Error(t, err)
}

func TestUnwrap_FailsOnSplicedAAD(t *testing.T) {
	// Simulates an attacker with disk access copying one row's
	// wrapped_key+nonce into a different row's identity — even with the
	// correct key for the *target* row, this must fail loudly rather than
	// silently decrypt.
	key, err := GenerateMasterKey()
	require.NoError(t, err)
	aad := slotAAD("archer", SlotTypeWebAuthn, "credential-1")

	ciphertext, nonce, err := wrap(key, []byte("plaintext"), aad)
	require.NoError(t, err)

	splicedAAD := slotAAD("anna", SlotTypeWebAuthn, "credential-1")
	_, err = unwrap(key, ciphertext, nonce, splicedAAD)
	require.Error(t, err)
}

func TestDeriveFromPassword_DeterministicGivenSameSaltAndParams(t *testing.T) {
	salt, err := newSalt()
	require.NoError(t, err)
	params := defaultKDFParams()

	d1 := deriveFromPassword("hunter2", salt, params)
	d2 := deriveFromPassword("hunter2", salt, params)
	require.Equal(t, d1, d2)

	d3 := deriveFromPassword("different-password", salt, params)
	require.NotEqual(t, d1, d3)
}

func TestKDFParams_RoundTrip(t *testing.T) {
	s := currentKDFParams()
	got, err := parseKDFParams(s)
	require.NoError(t, err)
	require.Equal(t, defaultKDFParams(), got)
}
