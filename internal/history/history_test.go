package history

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestConversationAndMessageRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	convID, err := s.StartConversation(ctx, "alex", "ha_assist")
	require.NoError(t, err)
	require.NotEmpty(t, convID)

	msgID, err := s.AppendMessage(ctx, convID, "user", "включи свет в зале")
	require.NoError(t, err)
	require.NoError(t, s.AppendToolCall(ctx, msgID, "ha_light_turn_on", "ha", `{"entity_id":"light.living_room"}`, `{"ok":true}`))

	_, err = s.AppendMessage(ctx, convID, "assistant", "Включил свет в зале.")
	require.NoError(t, err)
	require.NoError(t, s.EndConversation(ctx, convID))

	msgs, err := s.ConversationMessages(ctx, convID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, "user", msgs[0].Role)
	require.Equal(t, "assistant", msgs[1].Role)

	convos, err := s.RecentConversations(ctx, "alex", 10)
	require.NoError(t, err)
	require.Len(t, convos, 1)
	require.NotNil(t, convos[0].EndedAt)
}

func TestSearchMessages_FullTextSearchScopedToUser(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	convAlex, err := s.StartConversation(ctx, "alex", "cli")
	require.NoError(t, err)
	_, err = s.AppendMessage(ctx, convAlex, "user", "напомни купить молоко завтра")
	require.NoError(t, err)

	convAnna, err := s.StartConversation(ctx, "anna", "cli")
	require.NoError(t, err)
	_, err = s.AppendMessage(ctx, convAnna, "user", "напомни купить молоко послезавтра")
	require.NoError(t, err)

	results, err := s.SearchMessages(ctx, "alex", "молоко", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Contains(t, results[0].Content, "завтра")
}

func TestIdleConversations_FiltersByCutoffAndOpenStatus(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	openConv, err := s.StartConversation(ctx, "alex", "cli")
	require.NoError(t, err)
	_, err = s.AppendMessage(ctx, openConv, "user", "привет")
	require.NoError(t, err)

	closedConv, err := s.StartConversation(ctx, "alex", "cli")
	require.NoError(t, err)
	_, err = s.AppendMessage(ctx, closedConv, "user", "и это тоже")
	require.NoError(t, err)
	require.NoError(t, s.EndConversation(ctx, closedConv))

	// Under a generous cutoff, nothing just created counts as idle yet.
	idle, err := s.IdleConversations(ctx, time.Hour)
	require.NoError(t, err)
	require.Empty(t, idle)

	// Under a zero cutoff, the still-open conversation is immediately idle;
	// the already-ended one must never come back regardless of cutoff.
	idle, err = s.IdleConversations(ctx, 0)
	require.NoError(t, err)
	require.Len(t, idle, 1)
	require.Equal(t, openConv, idle[0].ID)
}

func TestSearchMessages_TreatsQueryAsFreeTextNotFTSSyntax(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	conv, err := s.StartConversation(ctx, "alex", "cli")
	require.NoError(t, err)
	_, err = s.AppendMessage(ctx, conv, "user", "мы говорили про отпуск в Италии")
	require.NoError(t, err)

	// Characters meaningful to FTS5 query syntax (unmatched quotes, boolean
	// operators, unbalanced parens) must not cause a query syntax error —
	// this is model-generated input via the search_history tool, not a
	// hand-written FTS5 query.
	_, err = s.SearchMessages(ctx, "alex", `отпуск" AND (Италии OR test`, 10)
	require.NoError(t, err)

	// A plain keyword still finds the match.
	results, err := s.SearchMessages(ctx, "alex", "отпуск", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
}

func TestRecentConversations_RespectsLimit(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	for i := 0; i < 5; i++ {
		_, err := s.StartConversation(ctx, "alex", "cli")
		require.NoError(t, err)
	}

	convos, err := s.RecentConversations(ctx, "alex", 3)
	require.NoError(t, err)
	require.Len(t, convos, 3)
}

func TestOpenConversation_ReturnsOpenOnlyOrNil(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	none, err := s.OpenConversation(ctx, "alex")
	require.NoError(t, err)
	require.Nil(t, none)

	convID, err := s.StartConversation(ctx, "alex", "cli")
	require.NoError(t, err)

	open, err := s.OpenConversation(ctx, "alex")
	require.NoError(t, err)
	require.NotNil(t, open)
	require.Equal(t, convID, open.ID)

	require.NoError(t, s.EndConversationWithSummary(ctx, convID, "discussed nothing much"))

	none, err = s.OpenConversation(ctx, "alex")
	require.NoError(t, err)
	require.Nil(t, none)
}

func TestGetConversation_ReturnsNilForMissing(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	missing, err := s.GetConversation(ctx, "does-not-exist")
	require.NoError(t, err)
	require.Nil(t, missing)

	convID, err := s.StartConversation(ctx, "alex", "cli")
	require.NoError(t, err)

	got, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "alex", got.UserID)
}

func TestEndConversationWithSummary_StoresSummary(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	convID, err := s.StartConversation(ctx, "alex", "cli")
	require.NoError(t, err)
	require.NoError(t, s.EndConversationWithSummary(ctx, convID, "talked about a trip to Italy"))

	got, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)
	require.NotNil(t, got.EndedAt)
	require.Equal(t, "talked about a trip to Italy", got.Summary)
}

func TestSetSystemPrompt_PersistsPrompt(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	convID, err := s.StartConversation(ctx, "alex", "cli")
	require.NoError(t, err)
	require.NoError(t, s.SetSystemPrompt(ctx, convID, "You are Miranda."))

	got, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)
	require.Equal(t, "You are Miranda.", got.SystemPrompt)
}

func TestDeleteConversation_RemovesMessagesToolCallsAndFTSEntries(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	convID, err := s.StartConversation(ctx, "alex", "cli")
	require.NoError(t, err)
	msgID, err := s.AppendMessage(ctx, convID, "user", "мы говорили про отпуск в Италии")
	require.NoError(t, err)
	require.NoError(t, s.AppendToolCall(ctx, msgID, "search_history", "", "{}", "ok"))

	require.NoError(t, s.DeleteConversation(ctx, convID))

	got, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)
	require.Nil(t, got)

	msgs, err := s.ConversationMessages(ctx, convID)
	require.NoError(t, err)
	require.Empty(t, msgs)

	// The FTS index must not still reference the deleted message.
	results, err := s.SearchMessages(ctx, "alex", "Италии", 10)
	require.NoError(t, err)
	require.Empty(t, results)
}

func TestSearchConversations_OnlyMatchesEndedConversationsForThatUser(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	closedConv, err := s.StartConversation(ctx, "alex", "cli")
	require.NoError(t, err)
	_, err = s.AppendMessage(ctx, closedConv, "user", "я собираюсь в отпуск в Италию")
	require.NoError(t, err)
	require.NoError(t, s.EndConversationWithSummary(ctx, closedConv, "trip to Italy planned"))

	openConv, err := s.StartConversation(ctx, "alex", "cli")
	require.NoError(t, err)
	_, err = s.AppendMessage(ctx, openConv, "user", "ещё про Италию, но это открытый диалог")
	require.NoError(t, err)

	otherUserConv, err := s.StartConversation(ctx, "anna", "cli")
	require.NoError(t, err)
	_, err = s.AppendMessage(ctx, otherUserConv, "user", "я тоже еду в Италию")
	require.NoError(t, err)
	require.NoError(t, s.EndConversationWithSummary(ctx, otherUserConv, "anna's trip"))

	results, err := s.SearchConversations(ctx, "alex", "Италию", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, closedConv, results[0].ID)
	require.Equal(t, "trip to Italy planned", results[0].Summary)
}

// TestAppendAssistantMessage_DownloadsRoundTripSeparateFromContent guards
// the fix for a real double-chip incident: downloads used to be appended as
// an in-band "\n\n<download>{json}</download>" marker inside the message's
// own Content, which meant a stored assistant reply's Content — replayed
// back to the model as conversation history on every later turn — carried
// that exact tag shape, and a model was observed copying it into a new
// reply of its own with the wrong id. Downloads must round-trip via their
// own column, leaving Content exactly what was passed in, with no trace of
// the file reference in the text at all.
func TestAppendAssistantMessage_DownloadsRoundTripSeparateFromContent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	convID, err := s.StartConversation(ctx, "alex", "web_ui")
	require.NoError(t, err)

	const content = "Файл готов!"
	downloads := []DownloadRef{{FileID: "abc123", Filename: "autumn.docx", SizeBytes: 37293, MIMEType: "application/zip"}}
	_, err = s.AppendAssistantMessage(ctx, convID, content, nil, downloads)
	require.NoError(t, err)

	msgs, err := s.ConversationMessages(ctx, convID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, content, msgs[0].Content, "content must be exactly what was passed in — no marker text appended")
	require.NotContains(t, msgs[0].Content, "download")
	require.Equal(t, downloads, msgs[0].Downloads)
}

// TestAppendAssistantMessage_NilDownloadsRoundTripAsEmpty covers the common
// case (a reply with no files) alongside the above: Downloads must decode
// as nil/empty, not error, when downloads_json was never set for the row.
func TestAppendAssistantMessage_NilDownloadsRoundTripAsEmpty(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	convID, err := s.StartConversation(ctx, "alex", "web_ui")
	require.NoError(t, err)

	_, err = s.AppendAssistantMessage(ctx, convID, "просто текст", nil, nil)
	require.NoError(t, err)

	msgs, err := s.ConversationMessages(ctx, convID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Empty(t, msgs[0].Downloads)
}

func TestMigrate_IsIdempotentAcrossReopens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "miranda.db")

	s1, err := Open(path)
	require.NoError(t, err)
	convID, err := s1.StartConversation(ctx, "alex", "cli")
	require.NoError(t, err)
	require.NoError(t, s1.EndConversationWithSummary(ctx, convID, "hello"))
	require.NoError(t, s1.Close())

	// Reopening an already-migrated database must not fail even though the
	// summary/system_prompt columns already exist (ensureColumn's job).
	s2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	got, err := s2.GetConversation(ctx, convID)
	require.NoError(t, err)
	require.Equal(t, "hello", got.Summary)
}
