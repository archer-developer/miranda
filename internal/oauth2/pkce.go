package oauth2

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// verifierBytes is 32 random bytes, base64url-no-padding-encoded into a
// 43-character code_verifier — within RFC 7636's required 43-128 character
// range, and matches the length most real-world OAuth2 clients use.
const verifierBytes = 32

// GenerateVerifier returns a fresh RFC 7636 code_verifier.
func GenerateVerifier() (string, error) {
	b := make([]byte, verifierBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauth2: generate pkce verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ChallengeS256 derives the S256 code_challenge from verifier:
// base64url-no-padding(sha256(verifier)), per RFC 7636 §4.2.
func ChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
