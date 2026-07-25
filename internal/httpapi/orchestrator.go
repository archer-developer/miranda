package httpapi

import (
	"context"
	"fmt"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/llm/router"
	"github.com/archer-developer/miranda/internal/llmtrace"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/memory"
	"github.com/archer-developer/miranda/internal/tts"
)

// maxToolIterations bounds the agent loop (model calls a tool, gets a
// result, decides what to do next) so a misbehaving model can't loop forever
// racking up API calls.
const maxToolIterations = 5

const rememberToolName = "remember_this"

const searchHistoryToolName = "search_history"

const endConversationToolName = "end_conversation"

const forgetConversationToolName = "forget_conversation"

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
	memoryCfg        config.MemoryConfig
	escalationCfg    config.EscalationConfig
	chunkMaxChars    int
	defaultUserID    string
	baseSystemPrompt string
}

// NewOrchestrator wires the agent loop's dependencies together.
func NewOrchestrator(
	r *router.Router,
	toolManager *mcp.Manager,
	historyStore *history.Store,
	memoryStore *memory.Store,
	ttsDispatcher *tts.Dispatcher,
	h *hub.Hub,
	agentCfg config.AgentConfig,
	memoryCfg config.MemoryConfig,
	escalationCfg config.EscalationConfig,
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
		memoryCfg:        memoryCfg,
		escalationCfg:    escalationCfg,
		chunkMaxChars:    chunkMaxChars,
		defaultUserID:    defaultUserID,
		baseSystemPrompt: agentCfg.SystemPrompt,
	}
}

// Handle runs one full turn and returns the assistant's reply.
func (o *Orchestrator) Handle(ctx context.Context, req InputRequest) (InputResponse, error) {
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

	memContent, err := o.memory.Read(userID)
	if err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: read memory: %w", err)
	}

	if _, err := o.history.AppendMessage(ctx, convID, "user", req.Text); err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: record user message: %w", err)
	}

	systemPrompt := o.buildSystemPrompt(memContent)
	if err := o.history.SetSystemPrompt(ctx, convID, systemPrompt); err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: set system prompt: %w", err)
	}

	messages := append([]llm.Message{{Role: llm.RoleSystem, Content: systemPrompt}}, priorMessages...)
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: req.Text})

	tools, err := o.availableTools(ctx)
	if err != nil {
		return InputResponse{}, err
	}

	control := &turnControl{}
	finalText, providerUsed, err := o.runAgentLoop(ctx, userID, convID, req.Source, messages, tools, control)
	if err != nil {
		return InputResponse{}, err
	}

	if _, err := o.history.AppendMessage(ctx, convID, "assistant", finalText); err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: record assistant message: %w", err)
	}

	// Applied after the reply is recorded (and, via TTS, already spoken) so
	// an end/forget request never costs the user their answer to this turn.
	// Both are best-effort: a failure here shouldn't fail a turn the user
	// already got a correct reply to, just leave the conversation open for
	// the next sweep/turn to retry against.
	switch {
	case control.forgetRequested:
		if err := o.history.DeleteConversation(ctx, convID); err != nil {
			o.hub.Publish(hub.Event{Source: "error", Message: "forget conversation: " + err.Error()})
		}
	case control.endRequested:
		if err := o.summarizeConversation(ctx, convID, userID); err != nil {
			o.hub.Publish(hub.Event{Source: "error", Message: "end conversation: " + err.Error()})
		}
	}

	return InputResponse{ConversationID: convID, Reply: finalText, ProviderUsed: providerUsed}, nil
}
