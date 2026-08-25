package history

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeRedactor stands in for internal/redact. The engine's own behavior is
// tested there; what matters here is only that every write path actually
// routes its text through the store's redactor.
type fakeRedactor struct{ calls int }

func (f *fakeRedactor) Redact(s string) string {
	f.calls++
	return strings.ReplaceAll(s, "665533", "******")
}

func openRedactingStore(t *testing.T) (*Store, *fakeRedactor) {
	t.Helper()
	s := openTestStore(t)
	r := &fakeRedactor{}
	s.SetRedactor(r)
	return s, r
}

func TestAppendMessage_RedactsContent(t *testing.T) {
	ctx := context.Background()
	s, _ := openRedactingStore(t)

	convID, err := s.StartConversation(ctx, "alex", "web_ui")
	require.NoError(t, err)
	_, err = s.AppendMessage(ctx, convID, "user", "пин-код от телефона Ани 665533")
	require.NoError(t, err)

	messages, err := s.ConversationMessages(ctx, convID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "пин-код от телефона Ани ******", messages[0].Content)
}

// TestSearchMessages_CannotFindRedactedValue is the point of masking at the
// store rather than at the caller: the FTS shadow tables are populated by
// triggers on `messages`, so they only ever see text that was already masked.
func TestSearchMessages_CannotFindRedactedValue(t *testing.T) {
	ctx := context.Background()
	s, _ := openRedactingStore(t)

	convID, err := s.StartConversation(ctx, "alex", "web_ui")
	require.NoError(t, err)
	_, err = s.AppendMessage(ctx, convID, "user", "пин-код от телефона Ани 665533")
	require.NoError(t, err)

	found, err := s.SearchMessages(ctx, "alex", "665533", 10)
	require.NoError(t, err)
	require.Empty(t, found, "the raw value must not survive anywhere in the FTS index")

	// The surrounding text is still searchable, so redaction has not cost the
	// user the ability to find the conversation again.
	found, err = s.SearchMessages(ctx, "alex", "телефона", 10)
	require.NoError(t, err)
	require.Len(t, found, 1)
}

func TestAppendAssistantMessage_RedactsContentAndToolArguments(t *testing.T) {
	ctx := context.Background()
	s, _ := openRedactingStore(t)

	convID, err := s.StartConversation(ctx, "alex", "web_ui")
	require.NoError(t, err)

	toolCalls := []ToolCallRef{{ID: "call_1", Name: "remember_this", Arguments: `{"fact":"пин 665533"}`}}
	_, err = s.AppendAssistantMessage(ctx, convID, "записала пин 665533", toolCalls, nil)
	require.NoError(t, err)

	messages, err := s.ConversationMessages(ctx, convID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "записала пин ******", messages[0].Content)
	require.Len(t, messages[0].ToolCalls, 1)
	require.Equal(t, `{"fact":"пин ******"}`, messages[0].ToolCalls[0].Arguments)

	// The caller's own slice must be untouched — the rest of the turn still
	// needs the original arguments.
	require.Equal(t, `{"fact":"пин 665533"}`, toolCalls[0].Arguments)
}

func TestAppendToolResultMessage_RedactsContent(t *testing.T) {
	ctx := context.Background()
	s, _ := openRedactingStore(t)

	convID, err := s.StartConversation(ctx, "alex", "web_ui")
	require.NoError(t, err)
	_, err = s.AppendToolResultMessage(ctx, convID, "call_1", "сохранено: 665533")
	require.NoError(t, err)

	messages, err := s.ConversationMessages(ctx, convID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "сохранено: ******", messages[0].Content)
}

func TestAppendToolCall_RedactsRequestAndResponse(t *testing.T) {
	ctx := context.Background()
	s, r := openRedactingStore(t)

	convID, err := s.StartConversation(ctx, "alex", "web_ui")
	require.NoError(t, err)
	msgID, err := s.AppendMessage(ctx, convID, "assistant", "ок")
	require.NoError(t, err)

	before := r.calls
	require.NoError(t, s.AppendToolCall(ctx, msgID, "ha_light", "ha", `{"pin":"665533"}`, `{"ok":"665533"}`))
	require.Equal(t, before+2, r.calls, "both request and response JSON must be redacted")

	var request, response string
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT request_json, response_json FROM tool_calls WHERE message_id = ?`, msgID,
	).Scan(&request, &response))
	require.Equal(t, `{"pin":"******"}`, request)
	require.Equal(t, `{"ok":"******"}`, response)
}

func TestSetSystemPrompt_Redacts(t *testing.T) {
	ctx := context.Background()
	s, _ := openRedactingStore(t)

	convID, err := s.StartConversation(ctx, "alex", "web_ui")
	require.NoError(t, err)
	// The system prompt carries the user's memory file folded into it, so it
	// is a first-class sink, not an incidental one.
	require.NoError(t, s.SetSystemPrompt(ctx, convID, "Ты Miranda.\n\n## Remembered\n- пин 665533"))

	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)
	require.Equal(t, "Ты Miranda.\n\n## Remembered\n- пин ******", conv.SystemPrompt)
}

func TestEndConversationWithSummary_RedactsSummary(t *testing.T) {
	ctx := context.Background()
	s, _ := openRedactingStore(t)

	convID, err := s.StartConversation(ctx, "alex", "web_ui")
	require.NoError(t, err)
	require.NoError(t, s.EndConversationWithSummary(ctx, convID, "обсудили пин 665533"))

	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)
	require.Equal(t, "обсудили пин ******", conv.Summary)
}

// TestStore_WithoutRedactorStoresVerbatim — redaction is optional, and a
// store with none must not alter anything.
func TestStore_WithoutRedactorStoresVerbatim(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	convID, err := s.StartConversation(ctx, "alex", "web_ui")
	require.NoError(t, err)
	_, err = s.AppendMessage(ctx, convID, "user", "пин-код от телефона Ани 665533")
	require.NoError(t, err)

	messages, err := s.ConversationMessages(ctx, convID)
	require.NoError(t, err)
	require.Equal(t, "пин-код от телефона Ани 665533", messages[0].Content)
}
