package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"
)

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
