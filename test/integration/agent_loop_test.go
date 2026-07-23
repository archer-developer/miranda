// Package integration is a black-box regression test for the full agent
// loop: it drives the real HTTP server (internal/httpapi) backed by a real
// SQLite history.Store and a real memory.Store on temp directories, but with
// a scripted fake llm.Provider and a fake mcp.Client standing in for any
// real network/LLM/MCP dependency. This is the test that catches wiring
// breakage between modules that per-package unit tests can't see.
package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/httpapi"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/llm/llmtest"
	"github.com/archer-developer/miranda/internal/llm/router"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/mcp/mcptest"
	"github.com/archer-developer/miranda/internal/memory"
)

// harness bundles everything needed to drive one end-to-end HTTP request
// through the real agent loop.
type harness struct {
	server  *httptest.Server
	history *history.Store
	memory  *memory.Store
}

func newHarness(t *testing.T, provider *llmtest.FakeProvider, mcpClients ...mcp.Client) *harness {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "miranda.db")
	historyStore, err := history.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = historyStore.Close() })

	memoryStore, err := memory.New(t.TempDir())
	require.NoError(t, err)

	escalation := config.EscalationConfig{Enabled: true, ToolName: "escalate_to_claude", TargetProvider: provider.Name()}
	r, err := router.New([]llm.Provider{provider}, escalation)
	require.NoError(t, err)

	toolManager := mcp.NewManager(mcpClients...)
	eventHub := hub.New(200)

	orchestrator := httpapi.NewOrchestrator(
		r, toolManager, historyStore, memoryStore, nil, eventHub,
		config.MemoryConfig{ExplicitTool: true, AutoSummarize: false},
		escalation, 100, "debug",
	)

	server := httpapi.NewServer(orchestrator, eventHub, "", nil, nil, nil, nil)
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)

	return &harness{server: ts, history: historyStore, memory: memoryStore}
}

func (h *harness) post(t *testing.T, req httpapi.InputRequest) httpapi.InputResponse {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)

	resp, err := http.Post(h.server.URL+"/api/v1/input", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out httpapi.InputResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func TestAgentLoop_TextOnlyTurnPersistsToHistory(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "Свет включён."})
	h := newHarness(t, provider)

	resp := h.post(t, httpapi.InputRequest{Source: "cli", UserID: "alex", Text: "включи свет"})
	require.Equal(t, "Свет включён.", resp.Reply)
	require.NotEmpty(t, resp.ConversationID)

	msgs, err := h.history.ConversationMessages(t.Context(), resp.ConversationID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, "включи свет", msgs[0].Content)
	require.Equal(t, "Свет включён.", msgs[1].Content)
}

func TestAgentLoop_MCPToolCallRoundTrip(t *testing.T) {
	ha := mcptest.New("ha", llm.ToolDef{Name: "light_turn_on"}).WithResult("light_turn_on", "ok")
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "ha_light_turn_on", Arguments: `{"entity_id":"light.living_room"}`}},
		llmtest.Response{Text: "Готово, включил свет в зале."},
	)
	h := newHarness(t, provider, ha)

	resp := h.post(t, httpapi.InputRequest{Source: "ha_assist", UserID: "alex", Text: "включи свет в зале"})
	require.Equal(t, "Готово, включил свет в зале.", resp.Reply)

	require.Len(t, ha.Calls, 1)
	require.Equal(t, "light_turn_on", ha.Calls[0].Tool)

	msgs, err := h.history.ConversationMessages(t.Context(), resp.ConversationID)
	require.NoError(t, err)
	var sawToolResult bool
	for _, m := range msgs {
		if m.Role == "tool" && m.Content == "ok" {
			sawToolResult = true
		}
	}
	require.True(t, sawToolResult, "expected the tool's result to be recorded in history")
}

func TestAgentLoop_RememberThisUpdatesMemoryFile(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "remember_this", Arguments: `{"fact":"allergic to cats"}`}},
		llmtest.Response{Text: "Запомнил."},
	)
	h := newHarness(t, provider)

	resp := h.post(t, httpapi.InputRequest{Source: "cli", UserID: "alex", Text: "у меня аллергия на кошек"})
	require.Equal(t, "Запомнил.", resp.Reply)

	content, err := h.memory.Read("alex")
	require.NoError(t, err)
	require.Contains(t, content, "allergic to cats")
}

func TestAgentLoop_RejectsRequestMissingText(t *testing.T) {
	h := newHarness(t, llmtest.New("local"))

	resp, err := http.Post(h.server.URL+"/api/v1/input", "application/json", bytes.NewReader([]byte(`{"source":"cli","user_id":"alex"}`)))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
