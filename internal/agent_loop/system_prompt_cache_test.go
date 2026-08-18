package agentloop

// Tests for docs/adr/system-prompt-caching.md: the system prompt is split
// into a stable block (persona, speaker identity, memory) sent first and a
// volatile block (current time) sent last, and shared/personal memory is
// read once per conversation rather than on every turn.

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda/internal/config"
)

// TestOrchestrator_SystemPromptSentAsTwoMessagesStableFirst guards the
// split itself: the stable block (index 0) must never contain the
// per-turn-volatile time, and the volatile block (index 1) must carry it —
// swapping the order (or merging them back into one message) would defeat
// anthropic.Provider's cache breakpoint, which is placed on the FIRST
// system message specifically (see anthropic.toAnthropicMessages).
func TestOrchestrator_SystemPromptSentAsTwoMessagesStableFirst(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "Привет!"})
	o, _, _ := newTestOrchestrator(t, provider)

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	require.Len(t, provider.Requests, 1)
	messages := provider.Requests[0].Messages
	require.GreaterOrEqual(t, len(messages), 3)
	require.Equal(t, llm.RoleSystem, messages[0].Role)
	require.Equal(t, llm.RoleSystem, messages[1].Role)
	require.Equal(t, llm.RoleUser, messages[2].Role)

	require.Contains(t, messages[1].Content, "Текущее время пользователя",
		"the volatile (second) block must carry the current time")
	require.NotContains(t, messages[0].Content, "Текущее время пользователя",
		"the stable (first) block must never contain the per-turn-volatile time")
	require.Contains(t, messages[0].Content, "alex",
		"the stable block still carries speaker identity")
}

// TestOrchestrator_SystemPromptListsHouseholdRoster guards the fix for the
// medical.ask "subjectId" hallucination: asked about a third household
// member by name, the model has no way to know that member's username
// unless the full roster (not just the current speaker) is spelled out in
// the stable system block. Sorted by username, not config order, so the
// block — and its cacheability — doesn't depend on config.yaml's list order.
func TestOrchestrator_SystemPromptListsHouseholdRoster(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "Привет!"})
	o := newTestOrchestratorWithUsers(t, provider, []config.UserConfig{
		{Username: "archer", PasswordHash: "x", FullName: "Саша"},
		{Username: "anna", PasswordHash: "x", FullName: "Аня"},
	})

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "archer", Text: "привет"})
	require.NoError(t, err)

	require.Len(t, provider.Requests, 1)
	stable := provider.Requests[0].Messages[0].Content
	require.Contains(t, stable, "anna — Аня")
	require.Contains(t, stable, "archer — Саша")
	require.Less(t, strings.Index(stable, "anna — Аня"), strings.Index(stable, "archer — Саша"),
		"roster must be sorted by username (anna before archer), not config order")
}

// TestOrchestrator_MemoryReadOncePerConversation guards conversationMemory:
// a fact written to disk mid-conversation (simulating a concurrently open,
// different conversation calling remember_this — see
// docs/adr/system-prompt-caching.md's accepted cross-conversation staleness
// tradeoff) must NOT appear in this conversation's stable system block on a
// later turn, since it was already cached on the first turn.
func TestOrchestrator_MemoryReadOncePerConversation(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{Text: "Окей"},
		llmtest.Response{Text: "Понял"},
	)
	o, _, mem := newTestOrchestrator(t, provider)
	ctx := context.Background()

	_, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	require.NoError(t, mem.Remember("alex", "написано другим разговором"))

	_, err = o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "продолжаем"})
	require.NoError(t, err)

	require.Len(t, provider.Requests, 2)
	stableSecondTurn := provider.Requests[1].Messages[0].Content
	require.NotContains(t, stableSecondTurn, "написано другим разговором",
		"the stable block must reuse the conversation's cached memory snapshot, not re-read disk mid-conversation")

	onDisk, err := mem.Read("alex")
	require.NoError(t, err)
	require.Contains(t, onDisk, "написано другим разговором", "sanity check: the fact really is on disk")
}

// TestOrchestrator_MemoryCacheClearedWhenConversationEnds guards
// clearConversationMemory's call from summarizeConversation (shared by the
// idle sweep and the explicit end_conversation tool) — without this, a
// later conversation for the same user could theoretically collide with a
// stale map entry (in practice conversation IDs are never reused, but the
// eviction is what keeps memoryCache from growing forever either way).
func TestOrchestrator_MemoryCacheClearedWhenConversationEnds(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{Text: "Окей"},
		llmtest.Response{Text: "## Summary\nsmalltalk\n## Preferences\n"}, // distillConversation's own Chat call
	)
	o, _, _ := newTestOrchestrator(t, provider)
	ctx := context.Background()

	resp, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	o.memoryMu.Lock()
	_, cached := o.memoryCache[resp.ConversationID]
	o.memoryMu.Unlock()
	require.True(t, cached, "sanity check: the conversation's memory should be cached after its first turn")

	require.NoError(t, o.SummarizeIdleSessions(ctx, 0))

	o.memoryMu.Lock()
	_, stillCached := o.memoryCache[resp.ConversationID]
	o.memoryMu.Unlock()
	require.False(t, stillCached, "ending the conversation must evict its cached memory snapshot")
}

// TestOrchestrator_MemoryCacheClearedWhenConversationForgotten mirrors
// TestOrchestrator_MemoryCacheClearedWhenConversationEnds for the
// forget_conversation path, which evicts directly in Handle rather than via
// summarizeConversation.
func TestOrchestrator_MemoryCacheClearedWhenConversationForgotten(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{Text: "Записал."},
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "forget_conversation", Arguments: `{}`}},
		llmtest.Response{Text: "Хорошо, забыл."},
	)
	o, _, _ := newTestOrchestrator(t, provider)
	ctx := context.Background()

	first, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "секретная информация"})
	require.NoError(t, err)

	resp, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "забудь этот диалог"})
	require.NoError(t, err)
	require.Equal(t, first.ConversationID, resp.ConversationID)

	o.memoryMu.Lock()
	_, stillCached := o.memoryCache[resp.ConversationID]
	o.memoryMu.Unlock()
	require.False(t, stillCached, "forgetting the conversation must evict its cached memory snapshot")
}
