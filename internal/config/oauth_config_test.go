package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, yamlContent string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))
	return path
}

func TestLoad_OAuthGatedServerWithTokenEnvReturnsError(t *testing.T) {
	path := writeConfig(t, `
oauth:
  providers:
    - name: google_calendar
      authorize_url: "https://accounts.google.com/o/oauth2/v2/auth"
      token_url: "https://oauth2.googleapis.com/token"
      client_id_env: "GOOGLE_OAUTH_CLIENT_ID"
      scopes: ["calendar"]
mcp:
  servers:
    - name: google_calendar
      url: "https://calendar.example.com/mcp"
      enabled: true
      lazy: true
      description: "Google Calendar"
      token_env: "SOME_TOKEN"
      oauth_provider: google_calendar
`)
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoad_OAuthGatedServerNotLazyReturnsError(t *testing.T) {
	path := writeConfig(t, `
oauth:
  providers:
    - name: google_calendar
      authorize_url: "https://accounts.google.com/o/oauth2/v2/auth"
      token_url: "https://oauth2.googleapis.com/token"
      client_id_env: "GOOGLE_OAUTH_CLIENT_ID"
      scopes: ["calendar"]
mcp:
  servers:
    - name: google_calendar
      url: "https://calendar.example.com/mcp"
      enabled: true
      lazy: false
      description: "Google Calendar"
      oauth_provider: google_calendar
`)
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoad_OAuthGatedServerUnknownProviderReturnsError(t *testing.T) {
	path := writeConfig(t, `
mcp:
  servers:
    - name: google_calendar
      url: "https://calendar.example.com/mcp"
      enabled: true
      lazy: true
      description: "Google Calendar"
      oauth_provider: does_not_exist
`)
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoad_DuplicateOAuthProviderNameReturnsError(t *testing.T) {
	path := writeConfig(t, `
oauth:
  providers:
    - name: google_calendar
      authorize_url: "https://accounts.google.com/o/oauth2/v2/auth"
      token_url: "https://oauth2.googleapis.com/token"
      client_id_env: "GOOGLE_OAUTH_CLIENT_ID"
      scopes: ["calendar"]
    - name: google_calendar
      authorize_url: "https://accounts.google.com/o/oauth2/v2/auth"
      token_url: "https://oauth2.googleapis.com/token"
      client_id_env: "GOOGLE_OAUTH_CLIENT_ID_2"
      scopes: ["calendar"]
`)
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoad_OAuthProviderMissingRequiredFieldReturnsError(t *testing.T) {
	path := writeConfig(t, `
oauth:
  providers:
    - name: google_calendar
      client_id_env: "GOOGLE_OAUTH_CLIENT_ID"
`)
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoad_OAuthEnabledWithoutPublicBaseURLReturnsError(t *testing.T) {
	path := writeConfig(t, `
oauth:
  enabled: true
  master_key_env: "OAUTH_MASTER_KEY"
`)
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoad_OAuthGatedServerWellFormedIsFine(t *testing.T) {
	path := writeConfig(t, `
oauth:
  enabled: true
  public_base_url: "https://miranda.example.com"
  callback_path: "/oauth/callback"
  master_key_env: "OAUTH_MASTER_KEY"
  providers:
    - name: google_calendar
      description: "Google Calendar"
      authorize_url: "https://accounts.google.com/o/oauth2/v2/auth"
      token_url: "https://oauth2.googleapis.com/token"
      client_id_env: "GOOGLE_OAUTH_CLIENT_ID"
      client_secret_env: "GOOGLE_OAUTH_CLIENT_SECRET"
      scopes: ["https://www.googleapis.com/auth/calendar"]
      pkce: true
      extra_authorize_params:
        access_type: offline
        prompt: consent
mcp:
  servers:
    - name: google_calendar
      url: "https://calendar.example.com/mcp"
      enabled: true
      lazy: true
      description: "Google Calendar — view and manage events."
      oauth_provider: google_calendar
`)
	cfg, err := Load(path)
	require.NoError(t, err)

	require.True(t, cfg.OAuth.Enabled)
	require.Len(t, cfg.OAuth.Providers, 1)
	require.Equal(t, "google_calendar", cfg.OAuth.Providers[0].Name)
	require.True(t, cfg.OAuth.Providers[0].PKCE)
	require.Equal(t, "offline", cfg.OAuth.Providers[0].ExtraAuthorizeParams["access_type"])
	require.Equal(t, "google_calendar", cfg.MCP.Servers[0].OAuthProvider)
}

func TestDefault_OAuthDisabledWithDefaultCallbackPath(t *testing.T) {
	cfg := Default()
	require.False(t, cfg.OAuth.Enabled)
	require.Equal(t, "/oauth/callback", cfg.OAuth.CallbackPath)
	require.Equal(t, "./data/oauth.db", cfg.Storage.OAuthSQLitePath)
}
