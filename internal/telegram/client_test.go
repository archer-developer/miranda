package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewWithAPIBase("test-token", server.URL)
}

func TestSendMessage_PostsToCorrectMethodAndChat(t *testing.T) {
	var gotPath string
	var gotBody map[string]any

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{}})
	})

	err := c.SendMessage(context.Background(), 12345, "hello")
	require.NoError(t, err)
	require.Equal(t, "/bottest-token/sendMessage", gotPath)
	require.Equal(t, float64(12345), gotBody["chat_id"])
	require.Equal(t, "hello", gotBody["text"])
}

func TestSendMessage_SplitsLongTextAcrossMultipleCalls(t *testing.T) {
	var calls []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		calls = append(calls, body["text"].(string))
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	long := strings.Repeat("a", maxMessageChars) + "\n" + strings.Repeat("b", maxMessageChars)
	err := c.SendMessage(context.Background(), 1, long)
	require.NoError(t, err)
	require.Greater(t, len(calls), 1, "text longer than the per-message limit must be split across multiple calls")
	require.Equal(t, long, strings.Join(calls, ""), "chunks must reconstruct the original text exactly")
	for _, chunk := range calls {
		require.LessOrEqual(t, len([]rune(chunk)), maxMessageChars)
	}
}

func TestSendMessage_APIErrorEnvelopeReturnsError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "chat not found"})
	})

	err := c.SendMessage(context.Background(), 1, "hi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "chat not found")
}

func TestSetWebhook_SendsURLAndSecret(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	err := c.SetWebhook(context.Background(), "https://example.com/telegram/webhook", "shh")
	require.NoError(t, err)
	require.Equal(t, "/bottest-token/setWebhook", gotPath)
	require.Equal(t, "https://example.com/telegram/webhook", gotBody["url"])
	require.Equal(t, "shh", gotBody["secret_token"])
}

func TestRandomSecret_ProducesDistinctValues(t *testing.T) {
	a, err := RandomSecret()
	require.NoError(t, err)
	b, err := RandomSecret()
	require.NoError(t, err)
	require.NotEmpty(t, a)
	require.NotEqual(t, a, b)
}

func TestSplitMessage_PrefersBreakingAtNewline(t *testing.T) {
	text := strings.Repeat("x", 10) + "\n" + strings.Repeat("y", 5)
	chunks := splitMessage(text, 12)
	require.Equal(t, []string{strings.Repeat("x", 10) + "\n", strings.Repeat("y", 5)}, chunks)
}

func TestSplitMessage_ShortTextIsUnsplit(t *testing.T) {
	require.Equal(t, []string{"short"}, splitMessage("short", 100))
}
