package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.NoError(t, err)
	require.Equal(t, Default(), cfg)
}

func TestLoad_PartialOverrideKeepsOtherDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlContent := `
server:
  http_addr: ":9999"
tts:
  default_device: "Станция Мини 3 Про"
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)

	require.Equal(t, ":9999", cfg.Server.HTTPAddr)
	require.Equal(t, "Станция Мини 3 Про", cfg.TTS.DefaultDevice)

	// Fields untouched by the override must still carry their defaults.
	require.Equal(t, "./data/miranda.db", cfg.Storage.SQLitePath)
	require.True(t, cfg.TTS.SpeakReplyTool)
	require.True(t, cfg.TTS.StopSpeechTool)
	require.True(t, cfg.Memory.AutoSummarize)
	require.Equal(t, "", cfg.LLM.DefaultProvider)
}

func TestDefault_TTSToolsOnAndNoDefaultDevice(t *testing.T) {
	cfg := Default()

	require.Equal(t, "", cfg.TTS.DefaultDevice)
	require.True(t, cfg.TTS.SpeakReplyTool)
	require.True(t, cfg.TTS.StopSpeechTool)
}

func TestLoad_LegacyTTSKeysAreIgnored(t *testing.T) {
	// A tts.yaml written before synthesis moved to miranda-tts still carries
	// primary/fallback/gemini_tts/yandex_station; those must not break
	// startup (the loader is not strict), and must not leak into defaults.
	path := filepath.Join(t.TempDir(), "tts.yaml")
	yamlContent := `
tts:
  default_device: "Станция"
  primary: gemini_tts
  gemini_tts:
    enabled: true
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "Станция", cfg.TTS.DefaultDevice)
}

func TestLoad_InvalidYAMLReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server: [this is not a map"), 0o644))

	_, err := Load(path)
	require.Error(t, err)
}

func TestLoad_DuplicateEnabledMCPServerNameReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlContent := `
mcp:
  servers:
    - name: ha
      url: "http://localhost:8123/api/mcp"
      enabled: true
    - name: ha
      url: "http://localhost:9999/api/mcp"
      enabled: true
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	_, err := Load(path)
	require.Error(t, err)
}

func TestLoad_DuplicateNameAcrossOneDisabledServerIsFine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlContent := `
mcp:
  servers:
    - name: ha
      url: "http://localhost:8123/api/mcp"
      enabled: true
    - name: ha
      url: "http://localhost:9999/api/mcp"
      enabled: false
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	_, err := Load(path)
	require.NoError(t, err)
}

func TestLoad_AnthropicNativeToolsAlongsideTavilyIsAllowed(t *testing.T) {
	// Tavily tools are named "tavily_web_search"/"tavily_web_fetch", so they
	// don't collide with Anthropic's native "web_search"/"web_fetch" — both
	// can be enabled at once. Claude sees all four and prefers its own
	// server-side native ones; Gemini and other providers only see Tavily's.
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlContent := `
llm:
  providers:
    - name: claude
      type: anthropic
      model: "claude-sonnet-5"
      anthropic_tools:
        web_search: true
        web_fetch: true
        code_execution: true
tavily:
  web_search:
    enabled: true
  web_fetch:
    enabled: true
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	_, err := Load(path)
	require.NoError(t, err)
}

func TestLoad_EncryptionKeyAllowedOnNonHTTPSServerReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlContent := `
mcp:
  servers:
    - name: diary
      url: "http://127.0.0.1:8789/mcp"
      enabled: true
      encryption_key_allowed: true
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	_, err := Load(path)
	require.Error(t, err)
}

func TestLoad_EncryptionKeyAllowedOnHTTPSServerIsFine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlContent := `
mcp:
  servers:
    - name: diary
      url: "https://diary.example.com/mcp"
      enabled: true
      encryption_key_allowed: true
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.True(t, cfg.MCP.Servers[0].EncryptionKeyAllowed)
}

func TestMCPServer_EncryptionKeyPermitted(t *testing.T) {
	require.True(t, MCPServer{EncryptionKeyAllowed: true, URL: "https://diary.example.com/mcp"}.EncryptionKeyPermitted())
	require.False(t, MCPServer{EncryptionKeyAllowed: true, URL: "http://diary.example.com/mcp"}.EncryptionKeyPermitted(), "not https")
	require.False(t, MCPServer{EncryptionKeyAllowed: false, URL: "https://diary.example.com/mcp"}.EncryptionKeyPermitted(), "not opted in")
}

func TestMCPServer_EncryptionKeyArg(t *testing.T) {
	require.Equal(t, "encryption_key", MCPServer{}.EncryptionKeyArg(), "defaults when unset")
	require.Equal(t, "record_encryption_key", MCPServer{EncryptionKeyArgName: "record_encryption_key"}.EncryptionKeyArg(), "override wins")
}

func TestMCPServer_SessionIDArg(t *testing.T) {
	require.Equal(t, "sessionId", MCPServer{}.SessionIDArg(), "defaults when unset")
	require.Equal(t, "conversation_session", MCPServer{SessionIDArgName: "conversation_session"}.SessionIDArg(), "override wins")
}

func TestLoad_SessionIDArgWithoutSessionIDToolsReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlContent := `
mcp:
  servers:
    - name: medical_card
      url: "https://127.0.0.1:8791/mcp"
      enabled: true
      session_id_arg: "conversation_session"
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	_, err := Load(path)
	require.Error(t, err)
}

func TestLoad_SessionIDToolsWithoutSessionIDArgIsFine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlContent := `
mcp:
  servers:
    - name: medical_card
      url: "https://127.0.0.1:8791/mcp"
      enabled: true
      session_id_tools: ["medical.ask"]
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, []string{"medical.ask"}, cfg.MCP.Servers[0].SessionIDTools)
	require.Equal(t, "sessionId", cfg.MCP.Servers[0].SessionIDArg(), "falls back to the default arg name")
}

func TestLoad_SessionIDArgWithSessionIDToolsIsFine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlContent := `
mcp:
  servers:
    - name: medical_card
      url: "https://127.0.0.1:8791/mcp"
      enabled: true
      session_id_arg: "conversation_session"
      session_id_tools: ["medical.ask"]
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "conversation_session", cfg.MCP.Servers[0].SessionIDArg())
}

func TestMCPServer_FilesEndpoint(t *testing.T) {
	url, token, err := MCPServer{Name: "medical_card", URL: "https://127.0.0.1:8791/mcp", TokenEnv: "NONEXISTENT_TEST_TOKEN_ENV"}.FilesEndpoint()
	require.NoError(t, err)
	require.Equal(t, "https://127.0.0.1:8791/files", url)
	require.Equal(t, "", token)

	_, _, err = MCPServer{Name: "bad", URL: "https://127.0.0.1:8791/not-mcp"}.FilesEndpoint()
	require.Error(t, err)
}

func TestConfig_FileExposingServers_OnlyIncludesOptedInEnabledServers(t *testing.T) {
	cfg := Config{MCP: MCPConfig{Servers: []MCPServer{
		{Name: "medical_card", URL: "https://127.0.0.1:8791/mcp", Enabled: true, ExposeFiles: true},
		{Name: "sandbox", URL: "http://127.0.0.1:8788/mcp", Enabled: true, ExposeFiles: true},
		{Name: "ha", URL: "http://192.168.1.50:8123/api/mcp", Enabled: true, ExposeFiles: false},
		{Name: "disabled_but_opted_in", URL: "http://127.0.0.1:9000/mcp", Enabled: false, ExposeFiles: true},
	}}}

	servers, err := cfg.FileExposingServers()
	require.NoError(t, err)
	require.Len(t, servers, 2)
	require.Equal(t, "https://127.0.0.1:8791/files", servers["medical_card"].FilesURL)
	require.Equal(t, "http://127.0.0.1:8788/files", servers["sandbox"].FilesURL)
	require.NotContains(t, servers, "ha")
	require.NotContains(t, servers, "disabled_but_opted_in")
}

func TestConfig_FileExposingServers_FailsFastOnMalformedURL(t *testing.T) {
	cfg := Config{MCP: MCPConfig{Servers: []MCPServer{
		{Name: "bad", URL: "https://127.0.0.1:8791/not-mcp", Enabled: true, ExposeFiles: true},
	}}}

	_, err := cfg.FileExposingServers()
	require.Error(t, err)
}

func TestLoad_LazyWithoutDescriptionReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlContent := `
mcp:
  servers:
    - name: diary
      url: "http://127.0.0.1:8789/mcp"
      enabled: true
      lazy: true
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	_, err := Load(path)
	require.Error(t, err)
}

func TestLoad_LazyWithDescriptionIsFine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlContent := `
mcp:
  servers:
    - name: diary
      url: "http://127.0.0.1:8789/mcp"
      enabled: true
      lazy: true
      description: "Personal diary: notes, thoughts, events."
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.True(t, cfg.MCP.Servers[0].Lazy)
	require.Equal(t, "Personal diary: notes, thoughts, events.", cfg.MCP.Servers[0].Description)
}

func TestConfig_LazyMCPServers_OnlyIncludesOptedInEnabledServers(t *testing.T) {
	cfg := Config{MCP: MCPConfig{Servers: []MCPServer{
		{Name: "diary", URL: "http://127.0.0.1:8789/mcp", Enabled: true, Lazy: true, Description: "Personal diary."},
		{Name: "ha", URL: "http://192.168.1.50:8123/api/mcp", Enabled: true, Lazy: false},
		{Name: "disabled_but_lazy", URL: "http://127.0.0.1:9000/mcp", Enabled: false, Lazy: true, Description: "n/a"},
	}}}

	lazy := cfg.LazyMCPServers()
	require.Len(t, lazy, 1)
	require.Equal(t, "Personal diary.", lazy["diary"])
	require.NotContains(t, lazy, "ha")
	require.NotContains(t, lazy, "disabled_but_lazy")
}
