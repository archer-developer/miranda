package telegram

import (
	"context"
	"fmt"

	"github.com/archer-developer/miranda/internal/replyformat"
)

// Sender is what the orchestrator's send_telegram tool (internal/httpapi)
// calls to proactively push a message to a household member's Telegram —
// distinct from the webhook handler replying within its own request, since
// a proactive send can target a *different* user than whoever is currently
// talking to Miranda (e.g. "отправь Ане на телефон ...").
type Sender struct {
	client *Client
	chats  *ChatStore
}

// NewSender builds a Sender from an already-configured Client and ChatStore.
func NewSender(client *Client, chats *ChatStore) *Sender {
	return &Sender{client: client, chats: chats}
}

// SendToUser sends text to username's Telegram chat. Fails if username has
// never messaged the bot, since that's the only way their chat id is ever
// learned (see ChatStore's doc comment) — the caller (the send_telegram
// tool) relays this back to the model so it can tell the user why, instead
// of the message silently going nowhere.
func (s *Sender) SendToUser(ctx context.Context, username, text string) error {
	chatID, ok := s.chats.Get(username)
	if !ok {
		return fmt.Errorf("no known Telegram chat for %q — they need to message the bot at least once first", username)
	}
	return s.client.SendMessage(ctx, chatID, text)
}

// SendHTMLToUser is SendToUser, but rendering blocks as Telegram HTML (see
// Client.SendHTML) instead of sending text as-is — used wherever the
// source text may carry markdown-lite formatting the model produced (the
// send_telegram tool, the webhook's own reply), as opposed to Miranda's
// own static markdown-free strings, which keep using SendToUser.
func (s *Sender) SendHTMLToUser(ctx context.Context, username string, blocks []replyformat.Block) error {
	chatID, ok := s.chats.Get(username)
	if !ok {
		return fmt.Errorf("no known Telegram chat for %q — they need to message the bot at least once first", username)
	}
	return s.client.SendHTML(ctx, chatID, blocks)
}
