package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	agentloop "github.com/archer-developer/miranda/internal/agent_loop"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/telegram"
	"github.com/archer-developer/miranda/internal/users"
)

// TelegramWebhook bundles everything Server needs to receive and answer
// Telegram Bot API webhook calls. Pass it to Server.SetTelegramWebhook once
// built (see cmd/miranda) — leaving it unset (nil, the default) means the
// route is never registered, the same nil-disables-the-feature convention
// webui.New uses for its WebAuthnService parameter.
type TelegramWebhook struct {
	// Path is where Telegram POSTs updates, e.g. "/telegram/webhook" (see
	// config.TelegramConfig.WebhookPath).
	Path string
	// Secret must match the X-Telegram-Bot-Api-Secret-Token header Telegram
	// echoes back on every call (see telegram.RandomSecret) — this is what
	// authenticates the webhook, since Telegram itself is the caller, not
	// anyone holding Server.authToken or a session cookie.
	Secret string
	Client *telegram.Client
	Chats  *telegram.ChatStore
}

// SetTelegramWebhook wires the optional Telegram bot route in, mirroring
// router.SetTracer's post-construction style for an optional dependency —
// no existing NewServer call site has to change to pass one more nil.
// Call at most once, before the server starts accepting connections; a nil
// tw is a no-op (Telegram channel disabled).
func (s *Server) SetTelegramWebhook(tw *TelegramWebhook) {
	if tw == nil {
		return
	}
	s.telegram = tw
	s.mux.HandleFunc("POST "+tw.Path, s.handleTelegramWebhook)
}

// handleTelegramWebhook receives one Telegram Bot API update, maps it to a
// configured Miranda user via UserConfig.TelegramName, and — only if that
// mapping succeeds — forwards it into the same agent loop every other
// channel uses. The reply is delivered back through the Bot API, not this
// handler's HTTP response: Telegram never reads that body, unlike the web
// UI/curl callers of POST /api/v1/input.
//
// An update from a Telegram account that doesn't match any configured
// user's telegram_name is logged as a warning and dropped before ever
// reaching the orchestrator — there's no Miranda identity (memory/history)
// for an unrecognized account to act as, and forwarding it anyway would
// silently create one keyed on a raw, unverified Telegram username.
func (s *Server) handleTelegramWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Telegram-Bot-Api-Secret-Token") != s.telegram.Secret {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var update telegram.Update
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		// Not worth a Telegram retry: whatever arrived wasn't a valid
		// update, and retrying decodes the same way.
		w.WriteHeader(http.StatusOK)
		return
	}

	msg := update.Message
	if msg == nil || msg.Text == "" || msg.From == nil {
		w.WriteHeader(http.StatusOK) // nothing actionable: edited message, non-text, channel post, ...
		return
	}

	var user users.User
	var matched bool
	if s.users != nil {
		user, matched = s.users.ResolveByTelegramName(msg.From.Username)
	}
	if !matched {
		s.logger.Warn("telegram: message from unrecognized account, dropping",
			"telegram_username", msg.From.Username, "chat_id", msg.Chat.ID)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Best-effort: learning/refreshing the chat id must never block
	// answering the message that carried it.
	if err := s.telegram.Chats.Save(user.Username, msg.Chat.ID); err != nil {
		s.logger.Error("telegram: save chat id", "error", err, "user", user.Username)
	}

	s.hub.Publish(hub.Event{Source: users.SourceTelegram, Message: fmt.Sprintf("%s: %s", user.Username, msg.Text)})

	// Detached from r.Context(): Telegram's own webhook connection isn't
	// held open for the reply (see the ack below), and speak_reply/ha_assist
	// TTS dispatch inside Handle can run well past however long the
	// connection Telegram used to deliver this update stays around — see
	// turnTimeout.
	turnCtx, cancel := agentloop.DetachedTurnContext(r.Context())
	defer cancel()

	resp, err := s.orchestrator.Handle(turnCtx, agentloop.InputRequest{
		Source: users.SourceTelegram,
		UserID: user.Username,
		Text:   msg.Text,
	})
	// Always ack the webhook itself, regardless of what happened next — a
	// non-2xx here just makes Telegram retry the same update, and any
	// failure below is reported to the user directly via the Bot API
	// instead.
	w.WriteHeader(http.StatusOK)
	if err != nil {
		s.logger.Error("telegram: orchestrator.Handle failed", "error", err, "user", user.Username)
		s.hub.Publish(hub.Event{Source: "error", Message: err.Error()})
		_ = s.telegram.Client.SendMessage(turnCtx, msg.Chat.ID, "Произошла ошибка, попробуйте ещё раз.")
		return
	}

	if err := s.telegram.Client.SendMessage(turnCtx, msg.Chat.ID, resp.Reply); err != nil {
		s.logger.Error("telegram: send reply failed", "error", err, "user", user.Username)
	}
}
