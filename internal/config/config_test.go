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
	require.Equal(t, "escalate_to_claude", cfg.LLM.Escalation.ToolName)
}

func TestLoad_InvalidYAMLReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server: [this is not a map"), 0o644))

	_, err := Load(path)
	require.Error(t, err)
}
