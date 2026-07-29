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
  yandex_station:
    chunk_max_chars: 80
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)

	require.Equal(t, ":9999", cfg.Server.HTTPAddr)
	require.Equal(t, 80, cfg.TTS.YandexStation.ChunkMaxChars)

	// Fields untouched by the override must still carry their defaults.
	require.Equal(t, "./data/miranda.db", cfg.Storage.SQLitePath)
	require.Equal(t, 300, cfg.TTS.YandexStation.IdlePollIntervalMS)
	require.True(t, cfg.Memory.AutoSummarize)
	require.Equal(t, "", cfg.LLM.DefaultProvider)
}

func TestDefault_TTSDefaultsToYandexStationTextWithGeminiOptIn(t *testing.T) {
	cfg := Default()

	// The always-available, zero-external-dependency provider stays the
	// default — gemini_tts is a new opt-in channel, not a default swap.
	require.Equal(t, "yandex_station_text", cfg.TTS.Primary)
	require.Equal(t, "yandex_station_text", cfg.TTS.Fallback)
	require.False(t, cfg.TTS.GeminiTTS.Enabled)
	require.Equal(t, "wav", cfg.TTS.GeminiTTS.AudioFormat)
	require.Equal(t, 200, cfg.TTS.GeminiTTS.ChunkMaxChars)
	require.Equal(t, 5, cfg.TTS.GeminiTTS.QuotaCooldownSeconds)
	require.Equal(t, 3, cfg.TTS.GeminiTTS.MaxQuotaRetryCycles)
	require.True(t, cfg.TTS.StopSpeechTool)
	require.Equal(t, "./data/storage", cfg.Storage.TTSCacheDir)
}

func TestLoad_PartialOverrideOfGeminiTTSKeepsOtherTTSDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlContent := `
tts:
  primary: gemini_tts
  fallback: yandex_station_text
  gemini_tts:
    enabled: true
    api_key_envs: ["GEMINI_API_KEY_1", "GEMINI_API_KEY_2"]
    model: "gemini-2.5-flash-preview-tts"
    voice: "Kore"
    public_base_url: "http://192.168.1.50:8787"
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)

	require.Equal(t, "gemini_tts", cfg.TTS.Primary)
	require.Equal(t, "yandex_station_text", cfg.TTS.Fallback)
	require.True(t, cfg.TTS.GeminiTTS.Enabled)
	require.Equal(t, []string{"GEMINI_API_KEY_1", "GEMINI_API_KEY_2"}, cfg.TTS.GeminiTTS.APIKeyEnvs)
	require.Equal(t, "gemini-2.5-flash-preview-tts", cfg.TTS.GeminiTTS.Model)
	require.Equal(t, "Kore", cfg.TTS.GeminiTTS.Voice)
	require.Equal(t, "http://192.168.1.50:8787", cfg.TTS.GeminiTTS.PublicBaseURL)

	// Untouched GeminiTTS/other TTS fields must still carry their defaults.
	require.Equal(t, "wav", cfg.TTS.GeminiTTS.AudioFormat)
	require.Equal(t, 200, cfg.TTS.GeminiTTS.ChunkMaxChars)
	require.Equal(t, 100, cfg.TTS.YandexStation.ChunkMaxChars)
	require.True(t, cfg.TTS.StopSpeechTool)
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
