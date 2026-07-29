package httpapi

import (
	"context"
	"fmt"
	"time"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/llm/router"
	"github.com/archer-developer/miranda/internal/llmtrace"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/memory"
	"github.com/archer-developer/miranda/internal/telegram"
	"github.com/archer-developer/miranda/internal/tts"
	"github.com/archer-developer/miranda/internal/users"
)

// maxToolIterations bounds the agent loop (model calls a tool, gets a
// result, decides what to do next) so a misbehaving model can't loop forever
// racking up API calls.
const maxToolIterations = 5

const rememberToolName = "remember_this"

const searchHistoryToolName = "search_history"

const endConversationToolName = "end_conversation"

const forgetConversationToolName = "forget_conversation"

const speakReplyToolName = "speak_reply"

const stopSpeechToolName = "stop_speech"

const sendTelegramToolName = "send_telegram"

// InputRequest is the body of POST /api/v1/input — the single entry point
// for both Home Assistant's thin conversation agent and manual curl/web UI
// commands, distinguished by Source.
//
// ConversationID is accepted for backward-compatible request shape but is
// never read: session continuity is owned by the server, keyed only on
// UserID (see resolveConversation), so the same user talking through HA,
// the web UI, or any future channel always continues the same conversation
// regardless of what conversation_id (if any) the caller sends.
type InputRequest struct {
	Source         string         `json:"source"`
	UserID         string         `json:"user_id"`
	Text           string         `json:"text"`
	ConversationID string         `json:"conversation_id,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// InputResponse is the synchronous reply to an InputRequest.
type InputResponse struct {
	ConversationID string `json:"conversation_id"`
	Reply          string `json:"reply"`
	ProviderUsed   string `json:"provider_used,omitempty"`
	// UserMessageID/AssistantMessageID are the history.Message.ID values
	// this turn just recorded — the caller (the web UI's own chat screen)
	// uses them to reconcile its optimistic render against the same
	// per-user WS chat stream (GET /ws/chat/{username}, see ChatEvent)
	// every other channel/tab learns about this turn from, so a message
	// this tab already rendered from the HTTP response is never rendered a
	// second time when its ChatEvent arrives.
	UserMessageID      int64 `json:"user_message_id"`
	AssistantMessageID int64 `json:"assistant_message_id"`
}

// ChatEvent is published on internal/hub (Source: "chat", scoped by UserID)
// every time a conversation is mutated, so GET /ws/chat/{username} can keep
// every open tab/channel for that user in sync without polling — see
// CLAUDE.md's "Session ownership" for why this can happen on a channel a
// given tab never itself talked to (HA, Telegram, another tab).
type ChatEvent struct {
	// Type is "message" (a new history.Message was recorded — Message is
	// set), "conversation_deleted" (forget_conversation), or
	// "conversation_ended" (end_conversation / the idle sweep).
	Type           string           `json:"type"`
	ConversationID string           `json:"conversation_id"`
	Message        *history.Message `json:"message,omitempty"`
}

// Orchestrator drives one full agent turn: it loads the user's memory and
// prior conversation, calls the LLM router (looping on tool calls), executes
// tools (either locally — remember_this — or via the MCP tool manager),
// dispatches assistant text to TTS as it streams, and persists the turn to
// history.
type Orchestrator struct {
	router           *router.Router
	tools            *mcp.Manager
	history          *history.Store
	memory           *memory.Store
	tts              *tts.Dispatcher // may be nil: TTS dispatch is best-effort and optional
	hub              *hub.Hub
	users            *users.Registry // may be nil: falls back to the raw user id in the system prompt
	memoryCfg        config.MemoryConfig
	ttsCfg           config.TTSConfig
	chunkMaxChars    int
	defaultUserID    string
	baseSystemPrompt string
	telegram         *telegram.Sender // set via SetTelegram; nil means the send_telegram tool is never offered
	telegramCfg      config.TelegramConfig
}

// SetTelegram wires the optional send_telegram tool in, mirroring
// router.SetTracer's post-construction style for an optional dependency —
// call it once from cmd/miranda after building a telegram.Sender, only
// when config.TelegramConfig.Enabled. Leaving it uncalled (the default)
// means availableTools never offers send_telegram at all.
func (o *Orchestrator) SetTelegram(sender *telegram.Sender, cfg config.TelegramConfig) {
	o.telegram = sender
	o.telegramCfg = cfg
}

// NewOrchestrator wires the agent loop's dependencies together.
func NewOrchestrator(
	r *router.Router,
	toolManager *mcp.Manager,
	historyStore *history.Store,
	memoryStore *memory.Store,
	ttsDispatcher *tts.Dispatcher,
	h *hub.Hub,
	usersRegistry *users.Registry,
	agentCfg config.AgentConfig,
	memoryCfg config.MemoryConfig,
	ttsCfg config.TTSConfig,
	chunkMaxChars int,
	defaultUserID string,
) *Orchestrator {
	return &Orchestrator{
		router:           r,
		tools:            toolManager,
		history:          historyStore,
		memory:           memoryStore,
		tts:              ttsDispatcher,
		hub:              h,
		users:            usersRegistry,
		memoryCfg:        memoryCfg,
		ttsCfg:           ttsCfg,
		chunkMaxChars:    chunkMaxChars,
		defaultUserID:    defaultUserID,
		baseSystemPrompt: agentCfg.SystemPrompt,
	}
}

// Handle runs one full turn and returns the assistant's reply.
func (o *Orchestrator) Handle(ctx context.Context, req InputRequest) (InputResponse, error) {
	if req.Source == users.SourceHAAssist && o.tts != nil {
		// Barge-in: interrupt whatever the station is still finishing from a
		// previous turn *before* this turn's own speech has any chance to
		// enqueue anything, so a new voice turn always cuts in rather than
		// queuing after what's already playing.
		o.tts.Stop(ctx)
	}

	userID := req.UserID
	if userID == "" {
		userID = o.defaultUserID
	}

	convID, priorMessages, err := o.resolveConversation(ctx, userID, req.Source)
	if err != nil {
		return InputResponse{}, err
	}
	// Tags every LLM call this turn makes (via the router's trace log) with
	// which conversation it belongs to, so logs/llm.log can be correlated
	// with a specific dialog.
	ctx = llmtrace.WithConversationID(ctx, convID)

	sharedMem, err := o.memory.ReadShared()
	if err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: read shared memory: %w", err)
	}
	memContent, err := o.memory.Read(userID)
	if err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: read memory: %w", err)
	}

	userMsgID, err := o.history.AppendMessage(ctx, convID, "user", req.Text)
	if err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: record user message: %w", err)
	}
	o.publishChatMessage(userID, convID, history.Message{ID: userMsgID, ConversationID: convID, Role: "user", Content: req.Text})

	systemPrompt := o.buildSystemPrompt(userID, sharedMem, memContent)
	if err := o.history.SetSystemPrompt(ctx, convID, systemPrompt); err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: set system prompt: %w", err)
	}

	messages := append([]llm.Message{{Role: llm.RoleSystem, Content: systemPrompt}}, priorMessages...)
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: req.Text})

	tools := o.availableTools(ctx)

	control := &turnControl{}
	finalText, providerUsed, err := o.runAgentLoop(ctx, userID, convID, req.Source, messages, tools, control)
	if err != nil {
		return InputResponse{}, err
	}

	assistantMsgID, err := o.history.AppendMessage(ctx, convID, "assistant", finalText)
	if err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: record assistant message: %w", err)
	}
	o.publishChatMessage(userID, convID, history.Message{ID: assistantMsgID, ConversationID: convID, Role: "assistant", Content: finalText})

	// Applied after the reply is recorded (and, via TTS, already spoken) so
	// an end/forget request never costs the user their answer to this turn.
	// Both are best-effort: a failure here shouldn't fail a turn the user
	// already got a correct reply to, just leave the conversation open for
	// the next sweep/turn to retry against.
	switch {
	case control.forgetRequested:
		if err := o.history.DeleteConversation(ctx, convID); err != nil {
			o.hub.Publish(hub.Event{Source: "error", Message: "forget conversation: " + err.Error()})
		} else {
			o.hub.Publish(hub.Event{Source: "chat", UserID: userID, Data: ChatEvent{Type: "conversation_deleted", ConversationID: convID}})
		}
	case control.endRequested:
		if err := o.summarizeConversation(ctx, convID, userID); err != nil {
			o.hub.Publish(hub.Event{Source: "error", Message: "end conversation: " + err.Error()})
		}
	}

	return InputResponse{
		ConversationID:     convID,
		Reply:              finalText,
		ProviderUsed:       providerUsed,
		UserMessageID:      userMsgID,
		AssistantMessageID: assistantMsgID,
	}, nil
}

// publishChatMessage broadcasts one recorded history.Message over
// /ws/logs, tagged Source: "chat" and scoped to userID so
// handleWSChat can forward it only to that user's GET /ws/chat/{username}
// connections — see ChatEvent. msg.CreatedAt is set to now rather than
// re-reading the row back from history: this turn just wrote it, so "now"
// is accurate enough for a live UI update (the authoritative timestamp is
// still whatever AppendMessage persisted).
func (o *Orchestrator) publishChatMessage(userID, convID string, msg history.Message) {
	msg.CreatedAt = time.Now()
	o.hub.Publish(hub.Event{Source: "chat", UserID: userID, Data: ChatEvent{Type: "message", ConversationID: convID, Message: &msg}})
}
