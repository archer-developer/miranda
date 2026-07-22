package httpapi

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/llm/llmtest"
	"github.com/archer-developer/miranda/internal/llm/router"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/mcp/mcptest"
	"github.com/archer-developer/miranda/internal/memory"
)

func newTestOrchestrator(t *testing.T, provider *llmtest.FakeProvider, mcpClients ...mcp.Client) (*Orchestrator, *history.Store, *memory.Store) {
	t.Helper()

	r, err := router.New([]llm.Provider{provider}, config.EscalationConfig{Enabled: true, ToolName: "escalate_to_claude", TargetProvider: provider.Name()})
	require.NoError(t, err)

	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	toolManager := mcp.NewManager(mcpClients...)

	o := NewOrchestrator(
		r, toolManager, h, mem, nil, hub.New(100),
		config.MemoryConfig{ExplicitTool: true, AutoSummarize: false},
		config.EscalationConfig{Enabled: true, ToolName: "escalate_to_claude", TargetProvider: provider.Name()},
		100, "debug",
	)
	return o, h, mem
}

func TestOrchestrator_SimpleTextReply(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "Привет! Чем помочь?"})
	o, h, _ := newTestOrchestrator(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)
	require.Equal(t, "Привет! Чем помочь?", resp.Reply)
	require.Equal(t, "local", resp.ProviderUsed)
	require.NotEmpty(t, resp.ConversationID)

	msgs, err := h.ConversationMessages(context.Background(), resp.ConversationID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, "user", msgs[0].Role)
	require.Equal(t, "assistant", msgs[1].Role)
}

func TestOrchestrator_RememberThisToolUpdatesMemoryAndContinues(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "remember_this", Arguments: `{"fact":"allergic to cats"}`}},
		llmtest.Response{Text: "Запомнил."},
	)
	o, _, mem := newTestOrchestrator(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "у меня аллергия на кошек, запомни"})
	require.NoError(t, err)
	require.Equal(t, "Запомнил.", resp.Reply)

	content, err := mem.Read("alex")
	require.NoError(t, err)
	require.Contains(t, content, "allergic to cats")
}

func TestOrchestrator_CallsMCPToolAndReturnsResultToModel(t *testing.T) {
	ha := mcptest.New("ha", llm.ToolDef{Name: "get_state"}).WithResult("get_state", "light.living_room: on")
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "ha_get_state", Arguments: `{"entity_id":"light.living_room"}`}},
		llmtest.Response{Text: "Свет в зале включён."},
	)
	o, _, _ := newTestOrchestrator(t, provider, ha)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "свет в зале горит?"})
	require.NoError(t, err)
	require.Equal(t, "Свет в зале включён.", resp.Reply)

	require.Len(t, ha.Calls, 1)
	require.Equal(t, "get_state", ha.Calls[0].Tool)

	// Second call to the provider must have received the tool result in context.
	require.Len(t, provider.Requests, 2)
	last := provider.Requests[1].Messages
	require.Equal(t, llm.RoleTool, last[len(last)-1].Role)
	require.Contains(t, last[len(last)-1].Content, "on")
}

func TestOrchestrator_EscalationIsTransparentToOrchestrator(t *testing.T) {
	local := llmtest.New("local-qwen", llmtest.Response{
		ToolCall: &llm.ToolCall{ID: "call-1", Name: "escalate_to_claude", Arguments: `{"reason":"hard"}`},
	})
	claude := llmtest.New("claude", llmtest.Response{Text: "Развёрнутый ответ от Клода."})

	r, err := router.New([]llm.Provider{local, claude}, config.EscalationConfig{
		Enabled: true, ToolName: "escalate_to_claude", TargetProvider: "claude",
	})
	require.NoError(t, err)

	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })
	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	o := NewOrchestrator(r, mcp.NewManager(), h, mem, nil, hub.New(100),
		config.MemoryConfig{ExplicitTool: true},
		config.EscalationConfig{Enabled: true, ToolName: "escalate_to_claude", TargetProvider: "claude"},
		100, "debug")

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "сложный вопрос"})
	require.NoError(t, err)
	require.Equal(t, "Развёрнутый ответ от Клода.", resp.Reply)
	require.Equal(t, "claude", resp.ProviderUsed)
}

func TestOrchestrator_ContinuesExistingConversationWithPriorContext(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{Text: "Первый ответ."},
		llmtest.Response{Text: "Второй ответ."},
	)
	o, _, _ := newTestOrchestrator(t, provider)

	first, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "первое сообщение"})
	require.NoError(t, err)

	second, err := o.Handle(context.Background(), InputRequest{
		Source: "cli", UserID: "alex", Text: "второе сообщение", ConversationID: first.ConversationID,
	})
	require.NoError(t, err)
	require.Equal(t, first.ConversationID, second.ConversationID)

	// The second call to the model must have seen the first turn's history.
	secondReqMessages := provider.Requests[1].Messages
	var sawFirstUserMessage bool
	for _, m := range secondReqMessages {
		if m.Content == "первое сообщение" {
			sawFirstUserMessage = true
		}
	}
	require.True(t, sawFirstUserMessage)
}
