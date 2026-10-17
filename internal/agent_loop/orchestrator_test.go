package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda-llm/router"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/mcp/mcptest"
	"github.com/archer-developer/miranda/internal/memory"
	"github.com/archer-developer/miranda/internal/notify"
	"github.com/archer-developer/miranda/internal/replyformat"
	"github.com/archer-developer/miranda/internal/telegram"
	"github.com/archer-developer/miranda/internal/tools"
	"github.com/archer-developer/miranda/internal/users"
)

// fakeSpeakerResolver resolves the two speaker friendly names the tests use:
// a media_player and an assist_satellite.
type fakeSpeakerResolver struct{}

func (fakeSpeakerResolver) ResolveSpeaker(_ context.Context, friendlyName string) (string, error) {
	switch friendlyName {
	case "test-kitchen":
		return "media_player.kitchen", nil
	case "test-bedroom":
		return "assist_satellite.bedroom", nil
	}
	return "", fmt.Errorf("no speaker with friendly name %q", friendlyName)
}

// newFakeTTSServer is a stand-in for the miranda-tts MCP server, exposing
// its two real tools (speak, stop) with scripted successful results.
func newFakeTTSServer() *mcptest.FakeClient {
	return mcptest.New("miranda-tts", llm.ToolDef{Name: "speak"}, llm.ToolDef{Name: "stop"}).
		WithResult("speak", "played").
		WithResult("stop", "stopped")
}

// ttsCallsTo returns the arguments of every call the fake miranda-tts server
// received for tool.
func ttsCallsTo(tts *mcptest.FakeClient, tool string) []map[string]string {
	var out []map[string]string
	for _, c := range tts.CallsSnapshot() {
		if c.Tool != tool {
			continue
		}
		args := map[string]string{}
		_ = json.Unmarshal([]byte(c.ArgumentsJSON), &args)
		out = append(out, args)
	}
	return out
}

// requireEventuallySpoken waits for at least one speak call to have reached
// the fake miranda-tts server: speak_reply dispatches asynchronously (the
// real call blocks until playback ends), so Handle returns before it lands.
func requireEventuallySpoken(t *testing.T, tts *mcptest.FakeClient) {
	t.Helper()
	require.Eventually(t, func() bool { return len(ttsCallsTo(tts, "speak")) > 0 }, 2*time.Second, 5*time.Millisecond,
		"expected at least one speak call to reach miranda-tts")
}

// requireNeverSpoken gives any wrongly-triggered async speak call a brief
// window to show up, then asserts nothing did — there's no positive signal
// to wait on when asserting an absence.
func requireNeverSpoken(t *testing.T, tts *mcptest.FakeClient) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	require.Empty(t, ttsCallsTo(tts, "speak"))
}

// newTestOrchestratorWithTTS is like newTestOrchestrator but connects a fake
// miranda-tts MCP server, resolves speaker names via fakeSpeakerResolver and
// enables speak_reply/stop_speech with "test-kitchen" as the default device,
// so tests can assert on whether (and where) a turn asked miranda-tts to
// speak.
func newTestOrchestratorWithTTS(t *testing.T, provider *llmtest.FakeProvider) (*Orchestrator, *mcptest.FakeClient) {
	t.Helper()

	r, err := router.New([]llm.Provider{provider}, selfEscalation(provider.Name()), "")
	require.NoError(t, err)

	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	tts := newFakeTTSServer()
	ttsCfg := config.TTSConfig{
		DefaultDevice:  "test-kitchen",
		SpeakReplyTool: true,
		StopSpeechTool: true,
	}

	o := NewOrchestrator(
		r, mcp.NewManager(nil, tts), h, mem, hub.New(100, nil), nil,
		config.AgentConfig{},
		config.MemoryConfig{ExplicitTool: true},
		ttsCfg,
		"debug",
	)
	o.SetSpeakerHA(fakeSpeakerResolver{})
	return o, tts
}

// selfEscalation gives providerName an escalation config that targets
// itself — these tests don't exercise real escalation (see
// TestOrchestrator_EscalationIsTransparentToOrchestrator for that), but
// still need the escalate_to_claude tool to be visible to the FakeProvider
// the same way a real deployment's default provider would see its own
// configured escalation tool (see config.LLMProvider.Escalation).
func selfEscalation(providerName string) map[string]router.EscalationConfig {
	return map[string]router.EscalationConfig{
		providerName: {Enabled: true, ToolName: "escalate_to_claude", TargetProvider: providerName},
	}
}

func newTestOrchestrator(t *testing.T, provider *llmtest.FakeProvider, mcpClients ...mcp.Client) (*Orchestrator, *history.Store, *memory.Store) {
	t.Helper()

	r, err := router.New([]llm.Provider{provider}, selfEscalation(provider.Name()), "")
	require.NoError(t, err)

	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	toolManager := mcp.NewManager(nil, mcpClients...)

	o := NewOrchestrator(
		r, toolManager, h, mem, hub.New(100, nil), nil,
		config.AgentConfig{},
		config.MemoryConfig{
			ExplicitTool: true, AutoSummarize: true, SearchHistoryTool: true,
			EndConversationTool: true, ForgetConversationTool: true,
		},
		config.TTSConfig{},
		"debug",
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

// TestOrchestrator_EmptyFinalReplyFallsBackInsteadOfSendingBlankText is the
// regression test for a real production incident: a provider can stream
// Done with zero TextDelta and zero ToolCall chunks while reporting no
// error at all (observed from Gemini silently swallowing a prompt-level
// safety block — see gemini.Provider.attempt's PromptFeedback/finishReason
// handling in miranda-llm). runAgentLoop used to treat that as a valid
// "final answer" (len(toolCalls)==0) and forward the empty string straight
// through, so the end user got a silent blank reply. It must now go
// through the same turnFailureReply fallback every other agent-loop
// failure already uses.
func TestOrchestrator_EmptyFinalReplyFallsBackInsteadOfSendingBlankText(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: ""})
	o, _, _ := newTestOrchestrator(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "покажи анализ крови"})
	require.NoError(t, err, "a failed turn must still return a graceful reply to the caller, not a hard error")
	require.NotEmpty(t, resp.Reply, "the user must never see a blank final reply")
	require.Equal(t, o.turnFailureReply("alex"), resp.Reply)
}

// TestOrchestrator_InputResponseCarriesParsedBlocks guards the web UI's
// wiring: InputResponse.Blocks must be replyformat.Parse of the model's own
// reply text (finalText), independent of whatever transform (voice
// stripping, download footnotes) Reply itself went through for this
// source — the web UI renders from Blocks, never by parsing Reply.
func TestOrchestrator_InputResponseCarriesParsedBlocks(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "this is **bold** text"})
	o, _, _ := newTestOrchestrator(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: WebUISource, UserID: "alex", Text: "hi"})
	require.NoError(t, err)
	require.Equal(t, "this is **bold** text", resp.Reply)
	require.Equal(t, replyformat.Parse("this is **bold** text"), resp.Blocks)
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

// fakeWebTool is a minimal tools.Tool stub used only to exercise
// availableTools' MCP/built-in name-collision handling below, without
// needing a real tavily.Client.
type fakeWebTool struct {
	name string
}

func (f *fakeWebTool) Def() llm.ToolDef { return llm.ToolDef{Name: f.name, Description: "fake"} }
func (f *fakeWebTool) Call(ctx context.Context, argumentsJSON string) (string, error) {
	return "fake result", nil
}

func TestOrchestrator_MCPToolCollidingWithWebToolNameIsDroppedNotShadowed(t *testing.T) {
	// A server named "web" exposing a tool "search" gets prefixed
	// "web_search" by internal/mcp.Manager — the exact same name
	// internal/tools.WebSearchToolName uses (see agent_loop.go's
	// availableTools doc comment).
	collidingMCP := mcptest.New("web", llm.ToolDef{Name: "search"}).WithResult("search", "should never be called")
	provider := llmtest.New("local", llmtest.Response{Text: "ok"})
	o, _, _ := newTestOrchestrator(t, provider, collidingMCP)
	o.SetWebTools([]tools.Tool{&fakeWebTool{name: "web_search"}})

	defs := o.availableTools(context.Background(), "", &turnControl{})

	var names []string
	for _, d := range defs {
		names = append(names, d.Name)
	}
	require.Contains(t, names, "web_search")
	// Exactly one "web_search" ToolDef must reach the provider — sending
	// two with the same name is a hard error on Anthropic specifically —
	// and it must be the built-in one, not the MCP server's.
	count := 0
	for _, n := range names {
		if n == "web_search" {
			count++
		}
	}
	require.Equal(t, 1, count)
}

// toolDefNames extracts each ToolDef's Name, in order, for asserting on
// what a given provider request actually saw — used by the
// lazy-MCP-tool-loading tests below.
func toolDefNames(defs []llm.ToolDef) []string {
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return names
}

// TestOrchestrator_LazyMCPServerToolsHiddenBehindStub covers
// docs/adr/lazy-mcp-tool-loading.md §2.6: a server marked Lazy (here via
// SetLazyMCPServers, mirroring what config.Config.LazyMCPServers/cmd/miranda
// wires in) contributes only a one-line entry inside the shared
// load_tool_group stub, not its own real tool schemas, on a turn where the
// model hasn't asked for it yet.
func TestOrchestrator_LazyMCPServerToolsHiddenBehindStub(t *testing.T) {
	diary := mcptest.New("diary", llm.ToolDef{Name: "add_entry"})
	provider := llmtest.New("local", llmtest.Response{Text: "ok"})
	o, _, _ := newTestOrchestrator(t, provider, diary)
	o.SetLazyMCPServers(map[string]string{"diary": "Personal diary: notes, thoughts, events."})

	names := toolDefNames(o.availableTools(context.Background(), "", &turnControl{}))

	require.Contains(t, names, loadToolGroupToolName)
	require.NotContains(t, names, "diary_add_entry")
}

// TestOrchestrator_LoadedLazyGroupIsNotFetchedTwice guards a regression: an
// earlier version of availableTools only added a lazy server's name to the
// ToolsExcluding skip set while it was still *pending*, so once
// load_tool_group loaded it, ToolsForServer fetched its tools explicitly
// AND ToolsExcluding (no longer told to skip it) fetched them again —
// harmless to the final list (addMCP dedups by name) but the second,
// spurious copy tripped addMCP's same-name collision guard, which is meant
// to catch a genuine MCP/built-in name clash, and logged a false "collides
// with a built-in tool" hub error on every recompute after a group loaded.
func TestOrchestrator_LoadedLazyGroupIsNotFetchedTwice(t *testing.T) {
	diary := mcptest.New("diary", llm.ToolDef{Name: "add_entry"})
	provider := llmtest.New("local", llmtest.Response{Text: "ok"})
	o, _, _ := newTestOrchestrator(t, provider, diary)
	o.SetLazyMCPServers(map[string]string{"diary": "Personal diary: notes, thoughts, events."})

	h := hub.New(100, nil)
	o.hub = h
	_, replay, unsubscribe := h.Subscribe(nil)
	defer unsubscribe()

	defs := o.availableTools(context.Background(), "", &turnControl{loadedGroups: map[string]bool{"diary": true}})

	count := 0
	for _, d := range defs {
		if d.Name == "diary_add_entry" {
			count++
		}
	}
	require.Equal(t, 1, count, "a loaded lazy server's tool must appear exactly once")

	for _, ev := range replay {
		require.NotContains(t, ev.Message, "collides", "loading a lazy group must never trip the built-in/MCP name-collision guard")
	}
}

// TestOrchestrator_LoadToolGroupExpandsRealToolsForRestOfTurn is the
// end-to-end flow from docs/adr/lazy-mcp-tool-loading.md §5.2: the model
// calls load_tool_group("diary") first, sees diary's real tool on the very
// next iteration (and the stub itself disappears, since every lazy domain
// is now loaded), calls the real tool, and gets a normal final reply.
func TestOrchestrator_LoadToolGroupExpandsRealToolsForRestOfTurn(t *testing.T) {
	diary := mcptest.New("diary", llm.ToolDef{Name: "add_entry"}).WithResult("add_entry", "saved")
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: loadToolGroupToolName, Arguments: `{"group":"diary"}`}},
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-2", Name: "diary_add_entry", Arguments: `{"text":"hello"}`}},
		llmtest.Response{Text: "Записал в дневник."},
	)
	o, _, _ := newTestOrchestrator(t, provider, diary)
	o.SetLazyMCPServers(map[string]string{"diary": "Personal diary: notes, thoughts, events."})

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "запиши в дневник"})
	require.NoError(t, err)
	require.Equal(t, "Записал в дневник.", resp.Reply)
	require.Len(t, diary.Calls, 1)

	require.Len(t, provider.Requests, 3)
	firstNames := toolDefNames(provider.Requests[0].Tools)
	require.Contains(t, firstNames, loadToolGroupToolName)
	require.NotContains(t, firstNames, "diary_add_entry")

	secondNames := toolDefNames(provider.Requests[1].Tools)
	require.Contains(t, secondNames, "diary_add_entry")
	require.NotContains(t, secondNames, loadToolGroupToolName,
		"the stub must disappear once every lazy domain has been loaded this turn")
}

// TestOrchestrator_LoadToolGroupUnknownGroupReturnsError guards against the
// model hallucinating a domain name that isn't actually configured as lazy
// — executeTool must hand back an error result the model can react to
// (retry, apologize) instead of the turn silently misbehaving.
func TestOrchestrator_LoadToolGroupUnknownGroupReturnsError(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: loadToolGroupToolName, Arguments: `{"group":"nonexistent"}`}},
		llmtest.Response{Text: "ok"},
	)
	o, _, _ := newTestOrchestrator(t, provider)
	o.SetLazyMCPServers(map[string]string{"diary": "Personal diary: notes, thoughts, events."})

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "hi"})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Reply)

	require.Len(t, provider.Requests, 2)
	last := provider.Requests[1].Messages
	require.Equal(t, llm.RoleTool, last[len(last)-1].Role)
	require.Contains(t, last[len(last)-1].Content, "unknown tool group")
}

// TestOrchestrator_DirectCallToUnloadedLazyToolStillSucceeds guards
// docs/adr/lazy-mcp-tool-loading.md §3: a model that calls a real, existing
// tool by name without going through load_tool_group first (e.g. remembered
// from earlier in a long conversation) must still have that call served —
// mcp.Manager.Call routes by name, not by whether the name was in the list
// most recently shown to the model.
func TestOrchestrator_DirectCallToUnloadedLazyToolStillSucceeds(t *testing.T) {
	diary := mcptest.New("diary", llm.ToolDef{Name: "add_entry"}).WithResult("add_entry", "saved")
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "diary_add_entry", Arguments: `{"text":"hello"}`}},
		llmtest.Response{Text: "done"},
	)
	o, _, _ := newTestOrchestrator(t, provider, diary)
	o.SetLazyMCPServers(map[string]string{"diary": "Personal diary: notes, thoughts, events."})

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "hi"})
	require.NoError(t, err)
	require.Equal(t, "done", resp.Reply)
	require.Len(t, diary.Calls, 1)
}

func TestOrchestrator_EscalationIsTransparentToOrchestrator(t *testing.T) {
	local := llmtest.New("local-qwen", llmtest.Response{
		ToolCall: &llm.ToolCall{ID: "call-1", Name: "escalate_to_claude", Arguments: `{"reason":"hard"}`},
	})
	claude := llmtest.New("claude", llmtest.Response{Text: "Развёрнутый ответ от Клода."})

	escalations := map[string]router.EscalationConfig{
		"local-qwen": {Enabled: true, ToolName: "escalate_to_claude", TargetProvider: "claude"},
	}
	r, err := router.New([]llm.Provider{local, claude}, escalations, "")
	require.NoError(t, err)

	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })
	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	o := NewOrchestrator(r, mcp.NewManager(nil), h, mem, hub.New(100, nil), nil,
		config.AgentConfig{},
		config.MemoryConfig{ExplicitTool: true},
		config.TTSConfig{},
		"debug")

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "сложный вопрос"})
	require.NoError(t, err)
	require.Equal(t, "Развёрнутый ответ от Клода.", resp.Reply)
	require.Equal(t, "claude", resp.ProviderUsed)
}

func TestOrchestrator_EscalatedProviderStaysActiveAcrossToolCall(t *testing.T) {
	// Reproduces the reported bug: gemini-lite escalates to gemini-strong,
	// gemini-strong calls an ordinary (non-escalation) tool, and the
	// follow-up iteration that feeds the tool result back must still be
	// answered by gemini-strong — not silently downgraded back to
	// gemini-lite just because a tool call happened in between. lite only
	// has one scripted response, so FakeProvider panics if runAgentLoop ever
	// calls it a second time.
	ha := mcptest.New("ha", llm.ToolDef{Name: "get_state"}).WithResult("get_state", "light.living_room: on")

	lite := llmtest.New("gemini-lite", llmtest.Response{
		ToolCall: &llm.ToolCall{ID: "call-1", Name: "escalate_to_strong", Arguments: `{"reason":"hard"}`},
	})
	strong := llmtest.New("gemini-strong",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-2", Name: "ha_get_state", Arguments: `{"entity_id":"light.living_room"}`}},
		llmtest.Response{Text: "Свет в зале включён."},
	)

	escalations := map[string]router.EscalationConfig{
		"gemini-lite": {Enabled: true, ToolName: "escalate_to_strong", TargetProvider: "gemini-strong"},
	}
	r, err := router.New([]llm.Provider{lite, strong}, escalations, "")
	require.NoError(t, err)

	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })
	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	o := NewOrchestrator(r, mcp.NewManager(nil, ha), h, mem, hub.New(100, nil), nil,
		config.AgentConfig{},
		config.MemoryConfig{ExplicitTool: true},
		config.TTSConfig{},
		"debug")

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "свет в зале горит?"})
	require.NoError(t, err)
	require.Equal(t, "Свет в зале включён.", resp.Reply)
	require.Equal(t, "gemini-strong", resp.ProviderUsed)

	require.Len(t, lite.Requests, 1)
	require.Len(t, strong.Requests, 2)
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
	// conversation's summary, plus its conversation_id — restore_conversation
	// needs that id to bring the conversation back (see
	// TestOrchestrator_RestoreConversationToolReplaysTextOnlyHistoryIntoNewSession).
	lastReqMessages := provider.Requests[len(provider.Requests)-1].Messages
	require.Contains(t, lastReqMessages[len(lastReqMessages)-1].Content, "trip to Italy")
	require.Contains(t, lastReqMessages[len(lastReqMessages)-1].Content, "conversation_id=")
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

// newTestHistoryStore opens a standalone history.Store, independent of
// newTestOrchestrator, so a restore_conversation test can seed a source
// conversation — and learn its real, uuid-generated id — before scripting a
// FakeProvider whose canned tool call needs to reference that id.
func newTestHistoryStore(t *testing.T) *history.Store {
	t.Helper()
	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// newTestOrchestratorWithStore is like newTestOrchestrator but takes an
// already-open history.Store instead of creating its own — used by the
// restore_conversation tests below, which need to seed history (via
// newTestHistoryStore) before the FakeProvider's script can be written,
// since the script needs to reference a real conversation id.
func newTestOrchestratorWithStore(t *testing.T, provider *llmtest.FakeProvider, h *history.Store) *Orchestrator {
	t.Helper()

	r, err := router.New([]llm.Provider{provider}, selfEscalation(provider.Name()), "")
	require.NoError(t, err)

	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	o := NewOrchestrator(
		r, mcp.NewManager(nil), h, mem, hub.New(100, nil), nil,
		config.AgentConfig{},
		config.MemoryConfig{
			ExplicitTool: true, AutoSummarize: true, SearchHistoryTool: true,
			EndConversationTool: true, ForgetConversationTool: true,
		},
		config.TTSConfig{},
		"debug",
	)
	return o
}

// TestOrchestrator_RestoreConversationToolReplaysTextOnlyHistoryIntoNewSession
// is the main restore_conversation flow: given a conversation_id (as the
// model would have gotten it from a search_history result — see
// TestOrchestrator_SearchHistoryToolFindsPastConversation), the tool must
// close the currently open conversation exactly like end_conversation and
// start a fresh one seeded with the source conversation's plain text turns
// only — a tool-calling assistant turn (empty Content, only ToolCalls) and
// the "tool" result answering it must both be dropped, not replayed.
func TestOrchestrator_RestoreConversationToolReplaysTextOnlyHistoryIntoNewSession(t *testing.T) {
	ctx := context.Background()
	h := newTestHistoryStore(t)

	// Seed a past, already-ended conversation directly in history — this
	// stands in for a real conversation the idle sweep or end_conversation
	// already closed, with a tool-calling turn mixed into its text turns.
	sourceID, err := h.StartConversation(ctx, "alex", "cli")
	require.NoError(t, err)
	_, err = h.AppendMessage(ctx, sourceID, "user", "как сушить снеки дома?")
	require.NoError(t, err)
	_, err = h.AppendAssistantMessage(ctx, sourceID, "", []history.ToolCallRef{{ID: "call-1", Name: "some_tool", Arguments: "{}"}}, nil)
	require.NoError(t, err)
	_, err = h.AppendToolResultMessage(ctx, sourceID, "call-1", "tool result payload")
	require.NoError(t, err)
	_, err = h.AppendAssistantMessage(ctx, sourceID, "Суши при 60 градусах 6 часов.", nil, nil)
	require.NoError(t, err)
	require.NoError(t, h.EndConversationWithSummary(ctx, sourceID, "User asked about drying snacks at home."))

	restoreArgs := fmt.Sprintf(`{"conversation_id":%q}`, sourceID)
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-2", Name: "restore_conversation", Arguments: restoreArgs}},
		llmtest.Response{Text: "Возвращаемся к разговору о сушке снеков."},
		// restoreConversation closes the conversation this turn belongs to
		// exactly like end_conversation, via summarizeConversation — which,
		// with AutoSummarize on, makes its own background distillation call.
		llmtest.Response{Text: "## Summary\nUser asked to go back to the drying-snacks conversation.\n## Preferences\n"},
	)
	o := newTestOrchestratorWithStore(t, provider, h)

	resp, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "давай вернёмся к диалогу про сушку снеков"})
	require.NoError(t, err)
	require.Equal(t, "Возвращаемся к разговору о сушке снеков.", resp.Reply)

	// The conversation this turn's reply was recorded under (the one open
	// when Handle started) must now be closed, same as end_conversation.
	closed, err := h.GetConversation(ctx, resp.ConversationID)
	require.NoError(t, err)
	require.NotNil(t, closed.EndedAt)

	// A brand-new conversation must now be open for the user, seeded with
	// only the source conversation's plain text turns, in order.
	open, err := h.OpenConversation(ctx, "alex")
	require.NoError(t, err)
	require.NotNil(t, open)
	require.NotEqual(t, resp.ConversationID, open.ID)
	require.NotEqual(t, sourceID, open.ID)

	restored, err := h.ConversationMessages(ctx, open.ID)
	require.NoError(t, err)
	require.Len(t, restored, 2, "the tool-call and tool-result turns must be dropped")
	require.Equal(t, "user", restored[0].Role)
	require.Equal(t, "как сушить снеки дома?", restored[0].Content)
	require.Equal(t, "assistant", restored[1].Role)
	require.Equal(t, "Суши при 60 градусах 6 часов.", restored[1].Content)
}

// TestOrchestrator_RestoreConversationToolDefaultsToLastEndedConversation
// covers "давай вернёмся к последнему диалогу" — no conversation_id at all,
// which must resolve to the user's most recently ended conversation via
// history.LastEndedConversation.
func TestOrchestrator_RestoreConversationToolDefaultsToLastEndedConversation(t *testing.T) {
	ctx := context.Background()
	h := newTestHistoryStore(t)

	sourceID, err := h.StartConversation(ctx, "alex", "cli")
	require.NoError(t, err)
	_, err = h.AppendMessage(ctx, sourceID, "user", "какой у нас пароль от wifi?")
	require.NoError(t, err)
	_, err = h.AppendAssistantMessage(ctx, sourceID, "Пароль: qwerty123.", nil, nil)
	require.NoError(t, err)
	require.NoError(t, h.EndConversationWithSummary(ctx, sourceID, "User asked for the wifi password."))

	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "restore_conversation", Arguments: `{}`}},
		llmtest.Response{Text: "Хорошо, вернулись к тому разговору."},
		llmtest.Response{Text: "## Summary\nUser asked to go back to the wifi-password conversation.\n## Preferences\n"},
	)
	o := newTestOrchestratorWithStore(t, provider, h)

	resp, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "давай вернёмся к последнему диалогу"})
	require.NoError(t, err)
	require.Equal(t, "Хорошо, вернулись к тому разговору.", resp.Reply)

	open, err := h.OpenConversation(ctx, "alex")
	require.NoError(t, err)
	require.NotNil(t, open)
	restored, err := h.ConversationMessages(ctx, open.ID)
	require.NoError(t, err)
	require.Len(t, restored, 2)
	require.Equal(t, "какой у нас пароль от wifi?", restored[0].Content)
	require.Equal(t, "Пароль: qwerty123.", restored[1].Content)
}

// TestOrchestrator_RestoreConversationToolRejectsAnotherUsersConversation
// guards ownership: conversation_id is model-supplied input (normally
// copied from a search_history result, but nothing stops a model from
// inventing one), so restoring across users — or restoring the currently
// open conversation into itself — must fail closed instead of leaking
// another household member's dialog.
func TestOrchestrator_RestoreConversationToolRejectsAnotherUsersConversation(t *testing.T) {
	ctx := context.Background()
	h := newTestHistoryStore(t)

	othersID, err := h.StartConversation(ctx, "someone-else", "cli")
	require.NoError(t, err)
	_, err = h.AppendMessage(ctx, othersID, "user", "чей-то секрет")
	require.NoError(t, err)
	require.NoError(t, h.EndConversationWithSummary(ctx, othersID, "Someone else's secret."))

	restoreArgs := fmt.Sprintf(`{"conversation_id":%q}`, othersID)
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "restore_conversation", Arguments: restoreArgs}},
		llmtest.Response{Text: "Не нашёл такой диалог."},
	)
	o := newTestOrchestratorWithStore(t, provider, h)

	resp, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "давай вернёмся к диалогу " + othersID})
	require.NoError(t, err)
	require.Equal(t, "Не нашёл такой диалог.", resp.Reply)

	// alex's own (still-open) conversation must be untouched — the rejection
	// must never have set control.restoreConversationID.
	stillOpen, err := h.GetConversation(ctx, resp.ConversationID)
	require.NoError(t, err)
	require.Nil(t, stillOpen.EndedAt)

	lastReqMessages := provider.Requests[len(provider.Requests)-1].Messages
	require.Contains(t, lastReqMessages[len(lastReqMessages)-1].Content, "no such past conversation")
}

// TestOrchestrator_HAAssistReplyIsNotSpokenByMiranda guards the
// double-voicing fix: an ha_assist reply is voiced exactly once, by Home
// Assistant's Assist pipeline from the text this turn returns — Miranda
// itself must never ask miranda-tts to speak it.
func TestOrchestrator_HAAssistReplyIsNotSpokenByMiranda(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "Включил свет в зале."})
	o, tts := newTestOrchestratorWithTTS(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: users.SourceHAAssist, UserID: "alex", Text: "включи свет"})
	require.NoError(t, err)
	require.Equal(t, "Включил свет в зале.", resp.Reply)

	requireNeverSpoken(t, tts)
}

// TestOrchestrator_HAAssistTurnFailureReplyIsNotSpokenByMiranda covers the
// other former speak site: the fallback text for a failed final call used to
// be voiced explicitly for ha_assist; it now only travels back as the reply.
func TestOrchestrator_HAAssistTurnFailureReplyIsNotSpokenByMiranda(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Err: errors.New("boom")})
	o, tts := newTestOrchestratorWithTTS(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: users.SourceHAAssist, UserID: "alex", Text: "привет"})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Reply)

	requireNeverSpoken(t, tts)
}

func TestOrchestrator_SpeakReplyAsksMirandaTTSToSpeakOnDefaultDevice(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "speak_reply", Arguments: `{"text":"Хорошо, слушай."}`}},
		llmtest.Response{Text: "Хорошо, слушай."},
	)
	o, tts := newTestOrchestratorWithTTS(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "озвучь мне это"})
	require.NoError(t, err)
	require.Equal(t, "Хорошо, слушай.", resp.Reply)

	requireEventuallySpoken(t, tts)
	require.Equal(t, []map[string]string{{"text": "Хорошо, слушай.", "entity_id": "media_player.kitchen"}}, ttsCallsTo(tts, "speak"))
}

func TestOrchestrator_SpeakReplyDeviceArgumentCanTargetASatellite(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "speak_reply", Arguments: `{"text":"Пора спать.","device":"test-bedroom"}`}},
		llmtest.Response{Text: "Сказала."},
	)
	o, tts := newTestOrchestratorWithTTS(t, provider)

	_, err := o.Handle(context.Background(), InputRequest{Source: "telegram", UserID: "alex", Text: "скажи в спальне"})
	require.NoError(t, err)

	requireEventuallySpoken(t, tts)
	require.Equal(t, []map[string]string{{"text": "Пора спать.", "entity_id": "assist_satellite.bedroom"}}, ttsCallsTo(tts, "speak"))
}

// TestOrchestrator_SpeaksExactlyTheTextPassedToSpeakReplyTool guards against
// a real production bug an earlier version of this code had: speak_reply
// used to be a bare flag with no arguments, and the orchestrator guessed
// which turn's streamed text was "the answer" to speak — guess wrong (e.g.
// the model answers and calls speak_reply in the very same completion) and
// a later turn's unrelated post-tool-call wrap-up got auto-spoken too,
// duplicating the reply. Now speak_reply takes the exact text to speak as
// an explicit argument, so there's nothing to guess: only that argument
// ever reaches miranda-tts, exactly once, regardless of what any turn's own
// reply text says or how many more turns follow.
func TestOrchestrator_SpeaksExactlyTheTextPassedToSpeakReplyTool(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{
			Text:     "Осень — время урожая и тёплого чая.",
			ToolCall: &llm.ToolCall{ID: "call-1", Name: "speak_reply", Arguments: `{"text":"Скажу словами: наступила осень."}`},
		},
		llmtest.Response{Text: "Готово, озвучила!"},
	)
	o, tts := newTestOrchestratorWithTTS(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "web_ui", UserID: "alex", Text: "сгенерируй текст и озвучь его"})
	require.NoError(t, err)
	require.Equal(t, "Готово, озвучила!", resp.Reply)

	requireEventuallySpoken(t, tts)
	// Exactly the tool argument's text, exactly once — not the first turn's
	// written reply, and not the second turn's wrap-up.
	calls := ttsCallsTo(tts, "speak")
	require.Len(t, calls, 1)
	require.Equal(t, "Скажу словами: наступила осень.", calls[0]["text"])
}

// TestOrchestrator_SpeakReplyStripsMarkdownBeforeSpeaking guards the
// replyformat wiring: speak_reply's argument may contain markdown-lite
// syntax the model wrote (matching the system prompt's "## Форматирование"
// guidance), but a TTS engine must never hear literal "**"/"*"/backticks —
// only the plain spoken text replyformat.ToVoiceText produces.
func TestOrchestrator_SpeakReplyStripsMarkdownBeforeSpeaking(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "speak_reply", Arguments: `{"text":"Погода сегодня **отличная**, бери *зонт* на всякий случай."}`}},
		llmtest.Response{Text: "Готово."},
	)
	o, tts := newTestOrchestratorWithTTS(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "web_ui", UserID: "alex", Text: "какая погода"})
	require.NoError(t, err)
	require.Equal(t, "Готово.", resp.Reply)

	requireEventuallySpoken(t, tts)
	calls := ttsCallsTo(tts, "speak")
	require.Len(t, calls, 1)
	require.Equal(t, "Погода сегодня отличная, бери зонт на всякий случай.", calls[0]["text"])
}

func TestOrchestrator_SpeakReplyReportsUnresolvableDeviceToTheModelWithoutCallingMirandaTTS(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "speak_reply", Arguments: `{"text":"привет","device":"no-such-device"}`}},
		llmtest.Response{Text: "Не получилось."},
	)
	o, tts := newTestOrchestratorWithTTS(t, provider)

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "скажи"})
	require.NoError(t, err)

	last := provider.Requests[len(provider.Requests)-1].Messages
	require.Contains(t, last[len(last)-1].Content, "error:")
	require.Contains(t, last[len(last)-1].Content, "no-such-device")
	requireNeverSpoken(t, tts)
}

func TestOrchestrator_DoesNotSpeakForNonHASourcesUnlessAsked(t *testing.T) {
	for _, source := range []string{"web_ui", "cli", "telegram", "mobile_app"} {
		t.Run(source, func(t *testing.T) {
			provider := llmtest.New("local", llmtest.Response{Text: "Привет!"})
			o, tts := newTestOrchestratorWithTTS(t, provider)

			resp, err := o.Handle(context.Background(), InputRequest{Source: source, UserID: "alex", Text: "привет"})
			require.NoError(t, err)
			require.Equal(t, "Привет!", resp.Reply)

			// Every other channel already has its own output surface (the HTTP
			// response) — it must never also make a shared speaker talk.
			requireNeverSpoken(t, tts)
		})
	}
}

// TestOrchestrator_StopSpeechAsksMirandaTTSToStopTheDefaultDevice: the model
// calling stop_speech must reach miranda-tts's stop tool, targeting the
// default device's resolved entity_id.
func TestOrchestrator_StopSpeechAsksMirandaTTSToStopTheDefaultDevice(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "stop_speech", Arguments: `{}`}},
		llmtest.Response{Text: "Молчу."},
	)
	o, tts := newTestOrchestratorWithTTS(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "хватит болтать"})
	require.NoError(t, err)
	require.Equal(t, "Молчу.", resp.Reply)

	require.Equal(t, []map[string]string{{"entity_id": "media_player.kitchen"}}, ttsCallsTo(tts, "stop"))
}

func toolNames(req llm.ChatRequest) []string {
	var names []string
	for _, tool := range req.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// TestOrchestrator_SpeechToolsOfferedOnNonHASourcesAndMirandaTTSToolsHidden
// pins the model-visible tool surface: speak_reply/stop_speech are offered on
// a non-ha_assist turn, while miranda-tts's own prefixed tools never appear.
func TestOrchestrator_SpeechToolsOfferedOnNonHASourcesAndMirandaTTSToolsHidden(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "ok"})
	o, _ := newTestOrchestratorWithTTS(t, provider)

	_, err := o.Handle(context.Background(), InputRequest{Source: "telegram", UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	names := toolNames(provider.Requests[0])
	require.Contains(t, names, "speak_reply")
	require.Contains(t, names, "stop_speech")
	require.NotContains(t, names, "miranda-tts_speak")
	require.NotContains(t, names, "miranda-tts_stop")
}

// TestOrchestrator_SpeechToolsAbsentOnHAAssistTurns: the Assist pipeline
// already voices these turns, so offering speak_reply there would reopen the
// double-voicing this change closes, one level down.
func TestOrchestrator_SpeechToolsAbsentOnHAAssistTurns(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "ok"})
	o, _ := newTestOrchestratorWithTTS(t, provider)

	_, err := o.Handle(context.Background(), InputRequest{Source: users.SourceHAAssist, UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	names := toolNames(provider.Requests[0])
	require.NotContains(t, names, "speak_reply")
	require.NotContains(t, names, "stop_speech")
	require.NotContains(t, names, "miranda-tts_speak")
}

// TestOrchestrator_SpeechToolsAbsentWithoutMirandaTTS: speak_reply is a thin
// wrapper over the miranda-tts MCP server, so it is not offered when that
// server isn't connected.
func TestOrchestrator_SpeechToolsAbsentWithoutMirandaTTS(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "ok"})
	o, _, _ := newTestOrchestrator(t, provider)
	o.ttsCfg = config.TTSConfig{SpeakReplyTool: true, StopSpeechTool: true}

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	names := toolNames(provider.Requests[0])
	require.NotContains(t, names, "speak_reply")
	require.NotContains(t, names, "stop_speech")
}

// newTestOrchestratorWithUsers is like newTestOrchestrator but also wires up
// a users.Registry from configs, for tests that need speaker-identity or
// household-roster resolution (see buildSystemPrompt) without the rest of
// newTestOrchestratorWithTelegram's Telegram-specific setup.
func newTestOrchestratorWithUsers(t *testing.T, provider *llmtest.FakeProvider, configs []config.UserConfig) *Orchestrator {
	t.Helper()

	registry, err := users.NewRegistry(configs)
	require.NoError(t, err)

	r, err := router.New([]llm.Provider{provider}, selfEscalation(provider.Name()), "")
	require.NoError(t, err)

	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	return NewOrchestrator(
		r, mcp.NewManager(nil), h, mem, hub.New(100, nil), registry,
		config.AgentConfig{},
		config.MemoryConfig{ExplicitTool: true},
		config.TTSConfig{},
		"debug",
	)
}

// newTestOrchestratorWithTelegram is like newTestOrchestrator but also wires
// up a users.Registry and a telegram.Sender backed by a fake Bot API server,
// so tests can exercise the send_telegram tool end to end. sentTo records
// every SendMessage call's (chat id, text) as they happen.
func newTestOrchestratorWithTelegram(t *testing.T, provider *llmtest.FakeProvider, configs []config.UserConfig) (*Orchestrator, *[]sentTelegramMessage) {
	t.Helper()

	var sent []sentTelegramMessage
	fakeAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = append(sent, sentTelegramMessage{ChatID: body["chat_id"].(float64), Text: body["text"].(string)})
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(fakeAPI.Close)

	registry, err := users.NewRegistry(configs)
	require.NoError(t, err)

	chats, err := telegram.OpenChatStore(filepath.Join(t.TempDir(), "chats.json"))
	require.NoError(t, err)
	sender := telegram.NewSender(telegram.NewWithAPIBase("test-token", fakeAPI.URL), chats)

	r, err := router.New([]llm.Provider{provider}, selfEscalation(provider.Name()), "")
	require.NoError(t, err)

	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	o := NewOrchestrator(
		r, mcp.NewManager(nil), h, mem, hub.New(100, nil), registry,
		config.AgentConfig{},
		config.MemoryConfig{},
		config.TTSConfig{},
		"debug",
	)
	o.SetTelegram(sender, config.TelegramConfig{SendMessageTool: true})

	// Pre-populate the chat store as if every configured user had already
	// messaged the bot once, chat id == their index+1 — tests that need a
	// user with *no* known chat should use a username not in configs.
	for i, c := range configs {
		require.NoError(t, chats.Save(c.Username, int64(i+1)))
	}

	return o, &sent
}

type sentTelegramMessage struct {
	ChatID float64
	Text   string
}

func TestOrchestrator_SendTelegramTool_DefaultsToCurrentUser(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "send_telegram", Arguments: `{"text":"напоминание"}`}},
		llmtest.Response{Text: "Отправила."},
	)
	o, sent := newTestOrchestratorWithTelegram(t, provider, []config.UserConfig{
		{Username: "alex", PasswordHash: "x", FullName: "Alex"},
	})

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "отправь мне на телефон напоминание"})
	require.NoError(t, err)
	require.Equal(t, "Отправила.", resp.Reply)

	require.Len(t, *sent, 1)
	require.Equal(t, float64(1), (*sent)[0].ChatID) // alex's seeded chat id
	require.Equal(t, "напоминание", (*sent)[0].Text)
}

func TestOrchestrator_SendTelegramTool_ResolvesNamedRecipient(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "send_telegram", Arguments: `{"text":"купи молока","recipient":"Аня"}`}},
		llmtest.Response{Text: "Отправила Ане."},
	)
	o, sent := newTestOrchestratorWithTelegram(t, provider, []config.UserConfig{
		{Username: "alex", PasswordHash: "x", FullName: "Alex"},
		{Username: "anna", PasswordHash: "x", FullName: "Аня"},
	})

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "отправь Ане на телефон купи молока"})
	require.NoError(t, err)
	require.Equal(t, "Отправила Ане.", resp.Reply)

	require.Len(t, *sent, 1)
	require.Equal(t, float64(2), (*sent)[0].ChatID) // anna's seeded chat id
	require.Equal(t, "купи молока", (*sent)[0].Text)
}

func TestOrchestrator_SendTelegramTool_UnknownRecipientReturnsErrorToModel(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "send_telegram", Arguments: `{"text":"привет","recipient":"Незнакомец"}`}},
		llmtest.Response{Text: "Не нашла такого человека."},
	)
	o, sent := newTestOrchestratorWithTelegram(t, provider, []config.UserConfig{
		{Username: "alex", PasswordHash: "x", FullName: "Alex"},
	})

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "отправь Незнакомцу на телефон привет"})
	require.NoError(t, err)
	require.Equal(t, "Не нашла такого человека.", resp.Reply)
	require.Empty(t, *sent)
}

func TestOrchestrator_SendTelegramTool_UserWithNoKnownChatReturnsErrorToModel(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "send_telegram", Arguments: `{"text":"привет"}`}},
		llmtest.Response{Text: "У меня нет твоего телеграма."},
	)
	o, sent := newTestOrchestratorWithTelegram(t, provider, nil) // no users seeded => no known chat for "alex"

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "отправь мне на телефон привет"})
	require.NoError(t, err)
	require.Equal(t, "У меня нет твоего телеграма.", resp.Reply)
	require.Empty(t, *sent)
}

func TestOrchestrator_SendTelegramTool_NotOfferedWhenTelegramNotConfigured(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "Привет!"})
	o, _, _ := newTestOrchestrator(t, provider) // SetTelegram never called

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	require.Len(t, provider.Requests, 1)
	for _, tool := range provider.Requests[0].Tools {
		require.NotEqual(t, "send_telegram", tool.Name)
	}
}

// newTestOrchestratorWithNotify is like newTestOrchestratorWithTelegram but
// for the send_notification tool — wires a real (feed-only, no webpush
// config) notify.Service in via SetNotify.
func newTestOrchestratorWithNotify(t *testing.T, provider *llmtest.FakeProvider, configs []config.UserConfig) (*Orchestrator, *notify.Service) {
	t.Helper()

	registry, err := users.NewRegistry(configs)
	require.NoError(t, err)

	r, err := router.New([]llm.Provider{provider}, selfEscalation(provider.Name()), "")
	require.NoError(t, err)

	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	o := NewOrchestrator(
		r, mcp.NewManager(nil), h, mem, hub.New(100, nil), registry,
		config.AgentConfig{},
		config.MemoryConfig{},
		config.TTSConfig{},
		"debug",
	)

	store, err := notify.Open(filepath.Join(t.TempDir(), "notify.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	svc := notify.NewService(store, nil, nil)
	o.SetNotify(svc, config.NotifyConfig{SendNotificationTool: true})

	return o, svc
}

func TestOrchestrator_SendNotificationTool_DefaultsToCurrentUser(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "send_notification", Arguments: `{"text":"ужин готов"}`}},
		llmtest.Response{Text: "Отправила."},
	)
	o, svc := newTestOrchestratorWithNotify(t, provider, []config.UserConfig{
		{Username: "alex", PasswordHash: "x", FullName: "Alex"},
	})

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "уведоми меня, что ужин готов"})
	require.NoError(t, err)
	require.Equal(t, "Отправила.", resp.Reply)

	got, err := svc.ListForUser(context.Background(), "alex", 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "ужин готов", got[0].Body)
	require.Equal(t, "tool", got[0].Source)
}

func TestOrchestrator_SendNotificationTool_ResolvesNamedRecipient(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "send_notification", Arguments: `{"text":"ужин готов","recipient":"Аня"}`}},
		llmtest.Response{Text: "Отправила Ане."},
	)
	o, svc := newTestOrchestratorWithNotify(t, provider, []config.UserConfig{
		{Username: "alex", PasswordHash: "x", FullName: "Alex"},
		{Username: "anna", PasswordHash: "x", FullName: "Аня"},
	})

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "отправь Ане нотификацию, что ужин готов"})
	require.NoError(t, err)
	require.Equal(t, "Отправила Ане.", resp.Reply)

	alexNotifications, err := svc.ListForUser(context.Background(), "alex", 0)
	require.NoError(t, err)
	require.Empty(t, alexNotifications, "the notification must go to anna, not the calling user")

	annaNotifications, err := svc.ListForUser(context.Background(), "anna", 0)
	require.NoError(t, err)
	require.Len(t, annaNotifications, 1)
	require.Equal(t, "ужин готов", annaNotifications[0].Body)
}

func TestOrchestrator_SendNotificationTool_UnknownRecipientReturnsErrorToModel(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "send_notification", Arguments: `{"text":"привет","recipient":"Незнакомец"}`}},
		llmtest.Response{Text: "Не нашла такого человека."},
	)
	o, svc := newTestOrchestratorWithNotify(t, provider, []config.UserConfig{
		{Username: "alex", PasswordHash: "x", FullName: "Alex"},
	})

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "отправь Незнакомцу нотификацию привет"})
	require.NoError(t, err)
	require.Equal(t, "Не нашла такого человека.", resp.Reply)

	got, err := svc.ListForUser(context.Background(), "alex", 0)
	require.NoError(t, err)
	require.Empty(t, got, "an unresolved recipient must not fall back to notifying the caller")
}

func TestOrchestrator_SendNotificationTool_NotOfferedWhenNotifyNotConfigured(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "Привет!"})
	o, _, _ := newTestOrchestrator(t, provider) // SetNotify never called

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	require.Len(t, provider.Requests, 1)
	for _, tool := range provider.Requests[0].Tools {
		require.NotEqual(t, "send_notification", tool.Name)
	}
}
