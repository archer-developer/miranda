package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/history"
)

type fakeHistory struct {
	conversations []history.Conversation
	messages      []history.Message
}

func (f *fakeHistory) RecentConversations(ctx context.Context, userID string, limit int) ([]history.Conversation, error) {
	return f.conversations, nil
}

func (f *fakeHistory) ConversationMessages(ctx context.Context, conversationID string) ([]history.Message, error) {
	return f.messages, nil
}

func TestHandleIndex_ServesDashboardPage(t *testing.T) {
	h, err := New(&fakeHistory{})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "Miranda")
}

func TestHandleStatic_ServesCompiledStylesheet(t *testing.T) {
	h, err := New(&fakeHistory{})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/static/css/styles.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "tailwindcss")
}

func TestHandleDialogs_RequiresUserID(t *testing.T) {
	h, err := New(&fakeHistory{})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/dialogs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleDialogs_ReturnsConversationsJSON(t *testing.T) {
	fake := &fakeHistory{conversations: []history.Conversation{
		{ID: "conv-1", UserID: "alex", Source: "cli", StartedAt: time.Now()},
	}}
	h, err := New(fake)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/dialogs?user_id=alex", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var out []history.Conversation
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Len(t, out, 1)
	require.Equal(t, "conv-1", out[0].ID)
}

func TestHandleDialogMessages_ReturnsMessagesJSON(t *testing.T) {
	fake := &fakeHistory{messages: []history.Message{
		{ID: 1, Role: "user", Content: "привет"},
	}}
	h, err := New(fake)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/dialogs/conv-1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var out []history.Message
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Len(t, out, 1)
	require.Equal(t, "привет", out[0].Content)
}
