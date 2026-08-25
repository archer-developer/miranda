package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefault_AuthTokenEnvIsNamed(t *testing.T) {
	cfg := Default().Server
	// Named by default so enabling auth needs no config.yaml edit — setting
	// the environment variable is enough.
	require.Equal(t, "MIRANDA_AUTH_TOKEN", cfg.AuthTokenEnv)
	require.Empty(t, cfg.AuthToken, "the retired inline field must default to empty")
}

func TestLoad_AuthTokenEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path,
		[]byte("server:\n  auth_token_env: \"CUSTOM_TOKEN_VAR\"\n"), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "CUSTOM_TOKEN_VAR", cfg.Server.AuthTokenEnv)
	require.Equal(t, ":8787", cfg.Server.HTTPAddr, "untouched siblings keep their defaults")
}

// TestLoad_RejectsRetiredInlineAuthToken is the whole reason the retired
// field is still declared. Deleting it would make YAML ignore the old key,
// and a deployment that had a token set would restart with the bearer check
// silently disabled — every request accepted — while looking configured.
// A credential migration that can fail open has to fail loudly instead.
func TestLoad_RejectsRetiredInlineAuthToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path,
		[]byte("server:\n  auth_token: \"s3cret-token\"\n"), 0o644))

	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "server.auth_token is no longer supported")
	require.Contains(t, err.Error(), "auth_token_env", "the error must name the replacement")
	require.NotContains(t, err.Error(), "s3cret-token", "the error must not echo the secret")
}

// TestLoad_EmptyInlineAuthTokenIsAccepted — an explicit empty value is not a
// configured credential, so it must not block startup. This matters because
// the shipped config.yaml.dist carried `auth_token: ""` for a long time.
func TestLoad_EmptyInlineAuthTokenIsAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path,
		[]byte("server:\n  auth_token: \"\"\n"), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "MIRANDA_AUTH_TOKEN", cfg.Server.AuthTokenEnv)
}
