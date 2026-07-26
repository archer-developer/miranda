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
	"github.com/archer-developer/miranda/internal/tts"
	"github.com/archer-developer/miranda/internal/users"
)

// fakeHAClient is an in-memory tts.HAClient: play_media calls are recorded,
// and State always reports "idle" so the dispatcher's wait-for-idle poll
// never blocks the test.
type fakeHAClient struct {
	calls []string // recorded media_content_id values
}

func (f *fakeHAClient) CallService(ctx context.Context, domain, service string, data map[string]any) error {
	if domain == "media_player" && service == "play_media" {
		f.calls = append(f.calls, data["media_content_id"].(string))
	}
	return nil
}

func (f *fakeHAClient) State(ctx context.Context, entityID string) (string, error) {
	return "idle", nil
}

// newTestOrchestratorWithTTS is like newTestOrchestrator but wires up a real
// tts.Dispatcher backed by a fakeHAClient, so tests can assert on whether a
// turn actually spoke to the (simulated) Yandex Station or not.
func newTestOrchestratorWithTTS(t *testing.T, provider *llmtest.FakeProvider) (*Orchestrator, *fakeHAClient) {
	t.Helper()

	r, err := router.New([]llm.Provider{provider}, config.EscalationConfig{Enabled: true, ToolName: "escalate_to_claude", TargetProvider: provider.Name()})
	require.NoError(t, err)

	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	ha := &fakeHAClient{}
	dispatcher := tts.NewDispatcher(config.TTSConfig{
		Primary: "yandex_station",
		YandexStation: config.YandexStationConfig{
			Entities:           []string{"media_player.kitchen"},
			ChunkMaxChars:      100,
			IdlePollIntervalMS: 1,
		},
	}, ha)

	o := NewOrchestrator(
		r, mcp.NewManager(), h, mem, dispatcher, hub.New(100), nil,
		config.AgentConfig{},
		config.MemoryConfig{ExplicitTool: true},
		config.EscalationConfig{Enabled: true, ToolName: "escalate_to_claude", TargetProvider: provider.Name()},
		100, "debug",
	)
	return o, ha
}

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
		r, toolManager, h, mem, nil, hub.New(100), nil,
		config.AgentConfig{},
		config.MemoryConfig{
			ExplicitTool: true, AutoSummarize: false, SearchHistoryTool: true,
			EndConversationTool: true, ForgetConversationTool: true,
		},
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

	o := NewOrchestrator(r, mcp.NewManager(), h, mem, nil, hub.New(100), nil,
		config.AgentConfig{},
		config.MemoryConfig{ExplicitTool: true},
		config.EscalationConfig{Enabled: true, ToolName: "escalate_to_claude", TargetProvider: "claude"},
		100, "debug")

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "сложный вопрос"})
	require.NoError(t, err)
	require.Equal(t, "Развёрнутый ответ от Клода.", resp.Reply)
	require.Equal(t, "claude", resp.ProviderUsed)
}

func TestOrchestrator_SearchHistoryToolFindsPastConversation(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{Text: "Записал."},
		llmtest.Response{Text: "## Summary\nUser is planning a trip to Italy.\n## Preferences\n"},
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "search_history", Arguments: `{"query":"Италию"}`}},
		llmtest.Response{Text: "Да, ты говорил про отпуск в Италии."},
	)
	o, _, _ := newTestOrchestrator(t, provider)
	ctx := context.Background()

	_, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "я собираюсь в отпуск в Италию"})
	require.NoError(t, err)

	// Server-owned session continuity means this first conversation would
	// otherwise still be open (and thus already in context) when the second
	// turn arrives — end it now to simulate it having closed via the idle
	// timeout, so the second turn genuinely has to search a past, closed
	// conversation rather than seeing it already in its own context.
	require.NoError(t, o.SummarizeIdleSessions(ctx, 0))

	resp, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "помнишь, куда я собирался?"})
	require.NoError(t, err)
	require.Equal(t, "Да, ты говорил про отпуск в Италии.", resp.Reply)

	// The tool result fed back to the model must contain the earlier
	// conversation's summary.
	lastReqMessages := provider.Requests[len(provider.Requests)-1].Messages
	require.Contains(t, lastReqMessages[len(lastReqMessages)-1].Content, "trip to Italy")
}

func TestOrchestrator_SearchHistoryToolReportsNoMatches(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "search_history", Arguments: `{"query":"динозавры"}`}},
		llmtest.Response{Text: "Не припомню такого разговора."},
	)
	o, _, _ := newTestOrchestrator(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "мы говорили о динозаврах?"})
	require.NoError(t, err)
	require.Equal(t, "Не припомню такого разговора.", resp.Reply)

	lastReqMessages := provider.Requests[len(provider.Requests)-1].Messages
	require.Contains(t, lastReqMessages[len(lastReqMessages)-1].Content, "no matching")
}

func TestOrchestrator_ContinuesExistingConversationWithPriorContext(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{Text: "Первый ответ."},
		llmtest.Response{Text: "Второй ответ."},
	)
	o, _, _ := newTestOrchestrator(t, provider)

	first, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "первое сообщение"})
	require.NoError(t, err)

	// No conversation_id is passed here — session continuity is server-owned
	// and keyed only on userID, so this must still continue the same open
	// conversation automatically.
	second, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "второе сообщение"})
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

func TestOrchestrator_EndConversationToolEndsSessionAndSummarizesImmediately(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "end_conversation", Arguments: `{}`}},
		llmtest.Response{Text: "Хорошо, начинаем сначала."},
		llmtest.Response{Text: "## Summary\nUser asked to start a new conversation.\n## Preferences\n"},
		llmtest.Response{Text: "Привет!"},
	)
	o, h, _ := newTestOrchestrator(t, provider)
	ctx := context.Background()

	resp, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "давай начнём новую беседу"})
	require.NoError(t, err)
	require.Equal(t, "Хорошо, начинаем сначала.", resp.Reply)

	ended, err := h.GetConversation(ctx, resp.ConversationID)
	require.NoError(t, err)
	require.NotNil(t, ended.EndedAt)
	require.Contains(t, ended.Summary, "start a new conversation")

	// The next turn for the same user must open a brand-new conversation,
	// not resume the one just ended.
	next, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)
	require.NotEqual(t, resp.ConversationID, next.ConversationID)
}

func TestOrchestrator_ForgetConversationToolDeletesConversationEntirely(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{Text: "Записал."},
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "forget_conversation", Arguments: `{}`}},
		llmtest.Response{Text: "Хорошо, забыл."},
	)
	o, h, mem := newTestOrchestrator(t, provider)
	ctx := context.Background()

	first, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "секретная информация"})
	require.NoError(t, err)

	resp, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "забудь этот диалог"})
	require.NoError(t, err)
	require.Equal(t, "Хорошо, забыл.", resp.Reply)
	require.Equal(t, first.ConversationID, resp.ConversationID)

	gone, err := h.GetConversation(ctx, resp.ConversationID)
	require.NoError(t, err)
	require.Nil(t, gone)

	msgs, err := h.ConversationMessages(ctx, resp.ConversationID)
	require.NoError(t, err)
	require.Empty(t, msgs)

	// A forgotten conversation must never write anything to memory.
	content, err := mem.Read("alex")
	require.NoError(t, err)
	require.Empty(t, content)
}

func TestOrchestrator_SpeaksViaTTSForHAAssistSource(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "Включил свет в зале."})
	o, ha := newTestOrchestratorWithTTS(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: users.SourceHAAssist, UserID: "alex", Text: "включи свет"})
	require.NoError(t, err)
	require.Equal(t, "Включил свет в зале.", resp.Reply)

	// The reply must also have been spoken to the configured Yandex Station —
	// ha_assist is the one channel with a physical speaker to answer through.
	require.NotEmpty(t, ha.calls)
}

func TestOrchestrator_DoesNotSpeakViaTTSForNonHASources(t *testing.T) {
	for _, source := range []string{"web_ui", "cli", "telegram", "mobile_app"} {
		t.Run(source, func(t *testing.T) {
			provider := llmtest.New("local", llmtest.Response{Text: "Привет!"})
			o, ha := newTestOrchestratorWithTTS(t, provider)

			resp, err := o.Handle(context.Background(), InputRequest{Source: source, UserID: "alex", Text: "привет"})
			require.NoError(t, err)
			require.Equal(t, "Привет!", resp.Reply)

			// Every other channel already has its own output surface (the HTTP
			// response) — it must never also make the shared Yandex Station talk.
			require.Empty(t, ha.calls)
		})
	}
}
