package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/replyformat"
)

func TestSender_SendToUser_UnknownChatReturnsClearError(t *testing.T) {
	chats, err := OpenChatStore(filepath.Join(t.TempDir(), "chats.json"))
	require.NoError(t, err)
	sender := NewSender(New("token"), chats)

	err = sender.SendToUser(context.Background(), "nobody", "hi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "nobody")
	require.Contains(t, err.Error(), "message the bot")
}

func TestSender_SendToUser_KnownChatSendsMessage(t *testing.T) {
	var gotChatID float64
	var gotText string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		gotChatID = body["chat_id"].(float64)
		gotText = body["text"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	chats, err := OpenChatStore(filepath.Join(t.TempDir(), "chats.json"))
	require.NoError(t, err)
	require.NoError(t, chats.Save("alex", 777))

	sender := NewSender(c, chats)
	err = sender.SendToUser(context.Background(), "alex", "hello there")
	require.NoError(t, err)
	require.Equal(t, float64(777), gotChatID)
	require.Equal(t, "hello there", gotText)
}

func TestSender_SendHTMLToUser_UnknownChatReturnsClearError(t *testing.T) {
	chats, err := OpenChatStore(filepath.Join(t.TempDir(), "chats.json"))
	require.NoError(t, err)
	sender := NewSender(New("token"), chats)

	err = sender.SendHTMLToUser(context.Background(), "nobody", replyformat.Parse("hi"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "nobody")
	require.Contains(t, err.Error(), "message the bot")
}

func TestSender_SendHTMLToUser_KnownChatSendsHTMLWithParseMode(t *testing.T) {
	var gotChatID float64
	var gotText string
	var gotParseMode string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		gotChatID = body["chat_id"].(float64)
		gotText, _ = body["text"].(string)
		gotParseMode, _ = body["parse_mode"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	chats, err := OpenChatStore(filepath.Join(t.TempDir(), "chats.json"))
	require.NoError(t, err)
	require.NoError(t, chats.Save("alex", 777))

	sender := NewSender(c, chats)
	err = sender.SendHTMLToUser(context.Background(), "alex", replyformat.Parse("this is **bold** text"))
	require.NoError(t, err)
	require.Equal(t, float64(777), gotChatID)
	require.Equal(t, "HTML", gotParseMode)
	require.Equal(t, "this is <b>bold</b> text", gotText)
}
