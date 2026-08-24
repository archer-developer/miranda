package agentloop

import (
	"context"
	"errors"
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
	"github.com/archer-developer/miranda/internal/memory"
)

// newTestOrchestratorWithMemoryConfig is like newTestOrchestrator but takes
// a caller-chosen config.MemoryConfig, so a test can exercise AutoSummarize
// independently of newTestOrchestrator's own default.
func newTestOrchestratorWithMemoryConfig(t *testing.T, provider *llmtest.FakeProvider, memCfg config.MemoryConfig) (*Orchestrator, *history.Store, *memory.Store) {
	t.Helper()

	r, err := router.New([]llm.Provider{provider}, selfEscalation(provider.Name()), "")
	require.NoError(t, err)

	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	o := NewOrchestrator(
		r, mcp.NewManager(nil), h, mem, nil, hub.New(100, nil), nil,
		config.AgentConfig{}, memCfg, config.TTSConfig{}, 100, "debug",
	)
	return o, h, mem
}

func TestOrchestrator_SummarizeIdleSessionsDistillsMemoryAndEndsConversation(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{Text: "Хорошо, буду знать."},
		llmtest.Response{Text: "## Summary\nDiscussed that they only drink tea, never coffee.\n## Preferences\n- prefers tea over coffee"},
	)
	o, h, mem := newTestOrchestrator(t, provider)
	ctx := context.Background()

	resp, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "я пью только чай, никогда кофе"})
	require.NoError(t, err)

	// idleFor: 0 makes the just-created conversation immediately eligible,
	// without needing to backdate its messages.
	require.NoError(t, o.SummarizeIdleSessions(ctx, 0))

	content, err := mem.Read("alex")
	require.NoError(t, err)
	require.Contains(t, content, "prefers tea over coffee")

	convos, err := h.RecentConversations(ctx, "alex", 10)
	require.NoError(t, err)
	require.Len(t, convos, 1)
	require.Equal(t, resp.ConversationID, convos[0].ID)
	require.NotNil(t, convos[0].EndedAt)
	require.Contains(t, convos[0].Summary, "only drink tea")
}

func TestOrchestrator_SummarizeIdleSessionsSkipsConversationsStillWithinTimeout(t *testing.T) {
	// Only one scripted response: if the sweep summarized this conversation
	// anyway, the FakeProvider would panic on an unscripted second call.
	provider := llmtest.New("local", llmtest.Response{Text: "Ответ."})
	o, h, mem := newTestOrchestrator(t, provider)
	ctx := context.Background()

	_, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	require.NoError(t, o.SummarizeIdleSessions(ctx, time.Hour))

	content, err := mem.Read("alex")
	require.NoError(t, err)
	require.Empty(t, content)

	convos, err := h.RecentConversations(ctx, "alex", 10)
	require.NoError(t, err)
	require.Len(t, convos, 1)
	require.Nil(t, convos[0].EndedAt)
}

// TestOrchestrator_SummarizeIdleSessionsPromotesHouseholdFactToSharedMemory
// guards the fix for a real gap: before this, summarizeConversation only
// ever read/wrote a user's own Preferences section — a household fact
// mentioned in conversation but never explicitly flagged via
// remember_this(scope="shared") had no path into shared memory at all, only
// into that one user's personal notes (or nowhere).
func TestOrchestrator_SummarizeIdleSessionsPromotesHouseholdFactToSharedMemory(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{Text: "Хорошо, буду знать."},
		llmtest.Response{Text: "## Summary\nMentioned the household cat's name.\n" +
			"## Preferences\n\n" +
			"## Shared\n- у нас живёт кот Барсик"},
	)
	o, _, mem := newTestOrchestrator(t, provider)
	ctx := context.Background()

	_, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "у нас живёт кот Барсик"})
	require.NoError(t, err)

	require.NoError(t, o.SummarizeIdleSessions(ctx, 0))

	shared, err := mem.ReadShared()
	require.NoError(t, err)
	require.Contains(t, shared, "у нас живёт кот Барсик")
}

func TestOrchestrator_SummarizeIdleSessionsSkipsMemoryWriteOnEmptyDistillation(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{Text: "Понял."},
		llmtest.Response{Text: ""}, // nothing durable worth remembering
	)
	o, h, mem := newTestOrchestrator(t, provider)
	ctx := context.Background()

	_, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "какая сейчас погода?"})
	require.NoError(t, err)

	require.NoError(t, o.SummarizeIdleSessions(ctx, 0))

	content, err := mem.Read("alex")
	require.NoError(t, err)
	require.Empty(t, content)

	convos, err := h.RecentConversations(ctx, "alex", 10)
	require.NoError(t, err)
	require.Len(t, convos, 1)
	require.NotNil(t, convos[0].EndedAt)
}

// TestOrchestrator_SummarizeIdleSessionsSkipsDistillationWhenAutoSummarizeDisabled
// guards the fix where AutoSummarize used to only gate whether cmd/miranda's
// background ticker ran at all — meaning a deployment that disabled it also,
// silently, stopped every conversation from ever closing on the idle
// timeout. AutoSummarize must control only the LLM-based recap/memory step;
// idle-timeout closure must always happen.
// TestOrchestrator_SummarizeIdleSessionsSkipsPlaceholderNoneReply guards a
// real bug: despite summarizeSystemPrompt instructing "leave this section
// empty", the model sometimes replies with placeholder filler like
// "*(none)*" or a bare "-" bullet instead of an actually-empty body. Before
// isNoneSentinel/hasContent existed, that filler passed the
// TrimSpace(...) != "" check and got written verbatim — e.g. shared.md
// ending up with "## Remembered\n- (2026-08-17) *(none)*".
func TestOrchestrator_SummarizeIdleSessionsSkipsPlaceholderNoneReply(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{Text: "Хорошо, буду знать."},
		llmtest.Response{Text: "## Summary\nJust chit-chat, nothing durable.\n" +
			"## Preferences\n*(none)*\n" +
			"## Shared\n-"},
	)
	o, _, mem := newTestOrchestrator(t, provider)
	ctx := context.Background()

	_, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "какая сейчас погода?"})
	require.NoError(t, err)

	require.NoError(t, o.SummarizeIdleSessions(ctx, 0))

	content, err := mem.Read("alex")
	require.NoError(t, err)
	require.NotContains(t, content, "none")

	shared, err := mem.ReadShared()
	require.NoError(t, err)
	require.Empty(t, shared)
}

func TestOrchestrator_SummarizeIdleSessionsSkipsDistillationWhenAutoSummarizeDisabled(t *testing.T) {
	// Only one scripted response: if summarizeConversation attempted
	// distillation anyway, the FakeProvider would panic on the unscripted
	// second Chat call.
	provider := llmtest.New("local", llmtest.Response{Text: "Ответ."})
	o, h, mem := newTestOrchestratorWithMemoryConfig(t, provider, config.MemoryConfig{AutoSummarize: false})
	ctx := context.Background()

	resp, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	require.NoError(t, o.SummarizeIdleSessions(ctx, 0))

	content, err := mem.Read("alex")
	require.NoError(t, err)
	require.Empty(t, content, "no distillation must run when AutoSummarize is disabled")

	convos, err := h.RecentConversations(ctx, "alex", 10)
	require.NoError(t, err)
	require.Len(t, convos, 1)
	require.Equal(t, resp.ConversationID, convos[0].ID)
	require.NotNil(t, convos[0].EndedAt, "idle-timeout closure must happen regardless of AutoSummarize")
	require.Empty(t, convos[0].Summary)
}

// TestOrchestrator_SummarizeIdleSessionsEndsConversationEvenWhenDistillationFails
// guards the fix where a failed distillation call (LLM error, rate limit,
// exhausted escalation chain) used to leave the conversation open
// indefinitely for the next sweep to retry — meaning a persistently broken
// summarization provider could keep a session (and its sessionId, e.g. the
// one injected into medical.ask — see docs/adr/medical-card-session-injection.md)
// pinned open forever, well past its idle timeout. A distillation failure
// must be logged and skipped, not retried: the conversation still closes on
// this same pass, just without a recap.
func TestOrchestrator_SummarizeIdleSessionsEndsConversationEvenWhenDistillationFails(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{Text: "Ответ."},
		llmtest.Response{Err: errors.New("provider unavailable")},
	)
	o, h, mem := newTestOrchestrator(t, provider)
	ctx := context.Background()

	resp, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	// Subscribed only after the turn itself finishes, so the "chat" events
	// Handle publishes for the user/assistant messages don't show up here —
	// this test only cares about what SummarizeIdleSessions publishes.
	hubEvents, _, unsubscribe := o.hub.Subscribe(nil)
	defer unsubscribe()

	require.NoError(t, o.SummarizeIdleSessions(ctx, 0))

	convos, err := h.RecentConversations(ctx, "alex", 10)
	require.NoError(t, err)
	require.Len(t, convos, 1)
	require.Equal(t, resp.ConversationID, convos[0].ID)
	require.NotNil(t, convos[0].EndedAt, "a failed distillation must not block idle-timeout closure")
	require.Empty(t, convos[0].Summary)

	content, err := mem.Read("alex")
	require.NoError(t, err)
	require.Empty(t, content)

	select {
	case ev := <-hubEvents:
		require.Equal(t, "error", ev.Source)
		require.Contains(t, ev.Message, "distill")
	case <-time.After(time.Second):
		t.Fatal("expected a hub error event logging the failed distillation")
	}
}
