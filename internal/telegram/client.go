// Package telegram implements Miranda's optional Telegram bot channel: a
// minimal Bot API client for sending messages and registering the webhook,
// the webhook payload types, and a small persisted mapping from Miranda
// username to Telegram chat id (see ChatStore's doc comment for why that
// has to be learned rather than derived). The webhook itself is received
// and routed to the agent loop by internal/httpapi, which is what actually
// wires a Client/ChatStore/Sender together — this package has no
// dependency on the orchestrator or the users registry.
package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/archer-developer/miranda/internal/httpx"
	"github.com/archer-developer/miranda/internal/replyformat"
)

// defaultAPIBase is the real Telegram Bot API origin. Overridable per
// Client (see New) only so tests can point at an httptest.Server instead.
const defaultAPIBase = "https://api.telegram.org"

// maxMessageChars stays comfortably under Telegram's 4096 UTF-16-code-unit
// limit per sendMessage call — SendMessage splits longer text into several
// calls rather than truncating or failing.
const maxMessageChars = 4000

// Client is a small HTTP client for the subset of the Telegram Bot API
// Miranda needs: sending messages and registering the webhook.
type Client struct {
	token   string
	apiBase string
	http    *http.Client
}

// New builds a Client authenticating as the bot identified by token (from
// @BotFather, read out of the TELEGRAM_BOT_TOKEN environment variable by
// the caller — see config.TelegramConfig's doc comment).
func New(token string) *Client {
	return NewWithAPIBase(token, defaultAPIBase)
}

// NewWithAPIBase is New, but against apiBase instead of the real Telegram
// API — for tests, or for pointing at a self-hosted Bot API server instance
// instead of api.telegram.org.
func NewWithAPIBase(token, apiBase string) *Client {
	return &Client{
		token:   token,
		apiBase: apiBase,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// SendMessage sends text to chatID, splitting it across multiple API calls
// if it exceeds Telegram's per-message length limit.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) error {
	for _, chunk := range splitMessage(text, maxMessageChars) {
		payload := map[string]any{"chat_id": chatID, "text": chunk}
		if err := c.call(ctx, "sendMessage", payload, nil); err != nil {
			return err
		}
	}
	return nil
}

// SendHTML sends blocks to chatID as Telegram Bot API HTML
// (parse_mode="HTML"), splitting across multiple calls via
// replyformat.ToTelegramHTMLChunks when the rendered HTML exceeds
// maxMessageChars. Unlike SendMessage, malformed HTML in a chunk would
// make Telegram reject that whole call — ToTelegramHTMLChunks' chunking is
// block/tag-aware specifically to avoid that.
func (c *Client) SendHTML(ctx context.Context, chatID int64, blocks []replyformat.Block) error {
	for _, chunk := range replyformat.ToTelegramHTMLChunks(blocks, maxMessageChars) {
		payload := map[string]any{"chat_id": chatID, "text": chunk, "parse_mode": "HTML"}
		if err := c.call(ctx, "sendMessage", payload, nil); err != nil {
			return err
		}
	}
	return nil
}

// SetWebhook registers url as where Telegram should POST updates, with
// secretToken as the value Telegram will echo back on every call via the
// X-Telegram-Bot-Api-Secret-Token header — the webhook handler
// (internal/httpapi) checks that header matches before trusting a request.
func (c *Client) SetWebhook(ctx context.Context, url, secretToken string) error {
	payload := map[string]any{"url": url, "secret_token": secretToken}
	return c.call(ctx, "setWebhook", payload, nil)
}

// call POSTs a JSON payload to a Bot API method and decodes its "result"
// into out (if non-nil), treating both a non-2xx HTTP status and the API's
// own {"ok": false, ...} envelope as errors. The request/response transport
// plumbing (including a response size cap) is shared with internal/tavily's
// client via internal/httpx.PostJSON — only the response envelope
// ({"ok","description","result"}) is specific to this client.
func (c *Client) call(ctx context.Context, method string, payload, out any) error {
	url := fmt.Sprintf("%s/bot%s/%s", c.apiBase, c.token, method)
	status, respBody, err := httpx.PostJSON(ctx, c.http, url, nil, payload, httpx.DefaultMaxResponseBytes)
	if err != nil {
		return fmt.Errorf("telegram: %s: %w", method, err)
	}

	var apiResp struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("telegram: decode %s response (status %d): %w", method, status, err)
	}
	if !apiResp.OK {
		return fmt.Errorf("telegram: %s failed: %s", method, apiResp.Description)
	}
	if out != nil && len(apiResp.Result) > 0 {
		if err := json.Unmarshal(apiResp.Result, out); err != nil {
			return fmt.Errorf("telegram: decode %s result: %w", method, err)
		}
	}
	return nil
}

// splitMessage breaks text into chunks of at most max runes, preferring to
// cut at the last newline within a chunk so a message isn't split
// mid-sentence when it doesn't have to be. Mirrors internal/tts.Chunk's
// intent for a different transport (Telegram's length limit instead of the
// Yandex Station's).
func splitMessage(text string, max int) []string {
	runes := []rune(text)
	if len(runes) <= max {
		return []string{text}
	}

	var chunks []string
	for len(runes) > 0 {
		n := max
		if n > len(runes) {
			n = len(runes)
		}
		cut := n
		if n < len(runes) {
			if idx := lastNewline(runes[:n]); idx > 0 {
				cut = idx + 1
			}
		}
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}
	return chunks
}

func lastNewline(rs []rune) int {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i] == '\n' {
			return i
		}
	}
	return -1
}

// RandomSecret generates a fresh webhook authentication secret — see
// config.TelegramConfig's doc comment for why this is regenerated on every
// startup instead of being a configured value. 48 hex chars, well within
// Telegram's allowed secret_token charset ([A-Za-z0-9_-], 1-256 chars).
func RandomSecret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("telegram: generate webhook secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}
