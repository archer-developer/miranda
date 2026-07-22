package history

import (
	"context"
	"path/filepath"
	"testing"

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
