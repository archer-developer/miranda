package httpapi

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda-llm/router"
	agentloop "github.com/archer-developer/miranda/internal/agent_loop"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/memory"
)

// selfEscalation configures provider as its own escalation target — mirrors
// internal/agent_loop's own test helper of the same name (duplicated rather
// than exported: it's test-only fixture code, not part of either package's
// real API).
func selfEscalation(providerName string) map[string]router.EscalationConfig {
	return map[string]router.EscalationConfig{
		providerName: {Enabled: true, ToolName: "escalate_to_claude", TargetProvider: providerName},
	}
}

// newTestOrchestrator builds a minimal agentloop.Orchestrator — real
// router/history/memory, backed by a tempdir SQLite/markdown store — for
// tests that only care about the HTTP layer (Server, its webhook/upload
// routes) sitting in front of it. Mirrors internal/agent_loop's own
// newTestOrchestrator; duplicated here because that one is an unexported
// fixture in a different package.
func newTestOrchestrator(t *testing.T, provider *llmtest.FakeProvider, mcpClients ...mcp.Client) (*agentloop.Orchestrator, *history.Store, *memory.Store) {
	t.Helper()

	r, err := router.New([]llm.Provider{provider}, selfEscalation(provider.Name()), "")
	require.NoError(t, err)

	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	toolManager := mcp.NewManager(nil, mcpClients...)

	o := agentloop.NewOrchestrator(
		r, toolManager, h, mem, nil, hub.New(100, nil), nil,
		config.AgentConfig{},
		config.MemoryConfig{
			ExplicitTool: true, AutoSummarize: true, SearchHistoryTool: true,
			EndConversationTool: true, ForgetConversationTool: true,
		},
		config.TTSConfig{},
		100, "debug",
	)
	return o, h, mem
}
