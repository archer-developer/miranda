package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad_OrchestratorDefaultsToMiranda(t *testing.T) {
	cfg, err := Load("../../config/config.yaml.dist")
	require.NoError(t, err, "the shipped example config must load")
	require.Equal(t, OrchestratorMiranda, cfg.Agent.Orchestrator)
	require.Equal(t, "claude", cfg.Agent.ClaudeCode.Binary)
	require.Equal(t, 15, cfg.Agent.ClaudeCode.MaxTurns)
}

func TestLoad_OrchestratorClaudeCodeKeepsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte("agent:\n  orchestrator: claude_code\n  claude_code:\n    model: sonnet\n"), 0o600))
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, OrchestratorClaudeCode, cfg.Agent.Orchestrator)
	require.Equal(t, "sonnet", cfg.Agent.ClaudeCode.Model)
	require.Equal(t, "claude", cfg.Agent.ClaudeCode.Binary, "unset fields keep their defaults")
	require.NotEmpty(t, cfg.Agent.SystemPrompt, "overriding agent.orchestrator must not wipe the default system prompt")
}

func TestLoad_UnknownOrchestratorFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte("agent:\n  orchestrator: claude-code\n"), 0o600))
	_, err := Load(path)
	require.ErrorContains(t, err, "agent.orchestrator")
}
