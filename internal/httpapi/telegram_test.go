package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/telegram"
	"github.com/archer-developer/miranda/internal/users"
)

// newTestTelegramWebhook builds a TelegramWebhook whose Client talks to a
// fake Bot API server instead of the real Telegram, so tests can assert on
// what Miranda tried to send without any network access. The returned
// *[]sentMessage records every sendMessage call in order.
type sentMessage struct {
	ChatID float64
	Text   string
}

func newTestTelegramWebhook(t *testing.T) (*TelegramWebhook, *[]sentMessage) {
	t.Helper()
	var sent []sentMessage

	fakeAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if chatID, ok := body["chat_id"]; ok {
			sent = append(sent, sentMessage{ChatID: chatID.(float64), Text: body["text"].(string)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(fakeAPI.Close)

	chats, err := telegram.OpenChatStore(filepath.Join(t.TempDir(), "chats.json"))
	require.NoError(t, err)

	tw := &TelegramWebhook{
		Path:   "/telegram/webhook",
		Secret: "test-secret",
		Client: telegram.NewWithAPIBase("test-token", fakeAPI.URL),
		Chats:  chats,
	}
	return tw, &sent
}

func postTelegramUpdate(t *testing.T, ts *httptest.Server, secret string, update telegram.Update) *http.Response {
	t.Helper()
	body, err := json.Marshal(update)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/telegram/webhook", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestTelegramWebhook_RejectsRequestWithWrongOrMissingSecret(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "hello"})
	o, _, _ := newTestOrchestrator(t, provider)
	server := NewServer(o, o.Hub(), "", nil, nil, nil, nil)
	tw, sent := newTestTelegramWebhook(t)
	server.SetTelegramWebhook(tw)

	ts := httptest.NewServer(server)
	defer ts.Close()

	update := telegram.Update{Message: &telegram.Message{
		Text: "hi", Chat: telegram.Chat{ID: 1}, From: &telegram.From{Username: "alex"},
	}}

	resp := postTelegramUpdate(t, ts, "wrong-secret", update)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp2 := postTelegramUpdate(t, ts, "", update)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp2.StatusCode)

	require.Empty(t, *sent, "an unauthenticated update must never reach the bot API or the orchestrator")
}

func TestTelegramWebhook_UnrecognizedTelegramUserIsDroppedAndLogged(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "hello"})
	o, historyStore, _ := newTestOrchestrator(t, provider)

	registry, err := users.NewRegistry([]config.UserConfig{
		{Username: "alex", PasswordHash: "x", TelegramName: "alex_tg"},
	})
	require.NoError(t, err)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	server := NewServer(o, o.Hub(), "", nil, logger, registry, nil)
	tw, sent := newTestTelegramWebhook(t)
	server.SetTelegramWebhook(tw)

	ts := httptest.NewServer(server)
	defer ts.Close()

	update := telegram.Update{Message: &telegram.Message{
		Text: "привет", Chat: telegram.Chat{ID: 999}, From: &telegram.From{Username: "some_stranger"},
	}}
	resp := postTelegramUpdate(t, ts, "test-secret", update)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "the webhook itself must still be acked so Telegram doesn't retry")

	require.Contains(t, logBuf.String(), "unrecognized account")
	require.Contains(t, logBuf.String(), "some_stranger")
	require.Empty(t, *sent, "must never reply to, or otherwise act on, an unmapped Telegram account")

	convos, err := historyStore.RecentConversations(t.Context(), "some_stranger", 10)
	require.NoError(t, err)
	require.Empty(t, convos, "an unmatched update must never reach the orchestrator/create history")
}

func TestTelegramWebhook_KnownUserReachesOrchestratorAndGetsReplyViaBotAPI(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "И тебе привет!"})
	o, historyStore, _ := newTestOrchestrator(t, provider)

	registry, err := users.NewRegistry([]config.UserConfig{
		{Username: "alex", PasswordHash: "x", TelegramName: "@Alex_TG"},
	})
	require.NoError(t, err)

	server := NewServer(o, o.Hub(), "", nil, nil, registry, nil)
	tw, sent := newTestTelegramWebhook(t)
	server.SetTelegramWebhook(tw)

	ts := httptest.NewServer(server)
	defer ts.Close()

	// Telegram's own casing/leading-@ conventions vary; the mapping must be
	// case-insensitive and @-agnostic in both directions.
	update := telegram.Update{Message: &telegram.Message{
		Text: "привет", Chat: telegram.Chat{ID: 4242}, From: &telegram.From{Username: "alex_tg"},
	}}
	resp := postTelegramUpdate(t, ts, "test-secret", update)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Len(t, *sent, 1)
	require.Equal(t, float64(4242), (*sent)[0].ChatID)
	require.Equal(t, "И тебе привет!", (*sent)[0].Text)

	convos, err := historyStore.RecentConversations(t.Context(), "alex", 10)
	require.NoError(t, err)
	require.Len(t, convos, 1, "the turn must be recorded under the canonical username")

	chatID, ok := tw.Chats.Get("alex")
	require.True(t, ok)
	require.Equal(t, int64(4242), chatID)
}

func TestTelegramWebhook_IgnoresNonMessageUpdates(t *testing.T) {
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)
	server := NewServer(o, o.Hub(), "", nil, nil, nil, nil)
	tw, sent := newTestTelegramWebhook(t)
	server.SetTelegramWebhook(tw)

	ts := httptest.NewServer(server)
	defer ts.Close()

	resp := postTelegramUpdate(t, ts, "test-secret", telegram.Update{UpdateID: 1})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, *sent)
}
