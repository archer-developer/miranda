package oauth2

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerateVerifier(t *testing.T) {
	v, err := GenerateVerifier()
	require.NoError(t, err)
	require.Len(t, v, 43) // 32 random bytes, base64url-no-padding
	require.Regexp(t, `^[A-Za-z0-9_-]+$`, v)

	v2, err := GenerateVerifier()
	require.NoError(t, err)
	require.NotEqual(t, v, v2)
}

func TestChallengeS256_RFC7636AppendixB(t *testing.T) {
	// Known test vector from RFC 7636 Appendix B.
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const wantChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	require.Equal(t, wantChallenge, ChallengeS256(verifier))
}
