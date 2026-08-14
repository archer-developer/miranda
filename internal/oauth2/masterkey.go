package oauth2

import (
	"encoding/base64"
	"fmt"
	"os"
)

// masterKeySize is 32 bytes (AES-256), matching internal/keyring's own
// master key size.
const masterKeySize = 32

// LoadMasterKey reads a base64-standard-encoded 32-byte AES-256 key from the
// environment variable named envVar (config.OAuthConfig.MasterKeyEnv) — the
// same *_env indirection convention as every other secret in this codebase,
// except this env var holds raw key material directly rather than naming a
// credential to fetch from some external issuer. Generate one with
// `openssl rand -base64 32`. Regenerating it orphans every stored refresh
// token: every household member must re-authorize every OAuth-gated MCP
// server, since the previously encrypted rows become permanently
// undecryptable.
func LoadMasterKey(envVar string) ([]byte, error) {
	encoded := os.Getenv(envVar)
	if encoded == "" {
		return nil, fmt.Errorf("oauth2: env var %s is not set", envVar)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("oauth2: decode %s: %w", envVar, err)
	}
	if len(key) != masterKeySize {
		return nil, fmt.Errorf("oauth2: %s must decode to %d bytes, got %d", envVar, masterKeySize, len(key))
	}
	return key, nil
}
