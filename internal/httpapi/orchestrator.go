package httpapi

import (
	"context"
	"fmt"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/llm/router"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/memory"
	"github.com/archer-developer/miranda/internal/tts"
)

// maxToolIterations bounds the agent loop (model calls a tool, gets a
// result, decides what to do next) so a misbehaving model can't loop forever
// racking up API calls.
const maxToolIterations = 5

const rememberToolName = "remember_this"

// InputRequest is the body of POST /api/v1/input — the single entry point
// for both Home Assistant's thin conversation agent and manual curl/web UI
// commands, distinguished by Source.
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
		baseSystemPrompt: "You are Miranda, a home voice assistant. Be concise — your replies are often spoken aloud.",
	}
}

// Handle runs one full turn and returns the assistant's reply.
func (o *Orchestrator) Handle(ctx context.Context, req InputRequest) (InputResponse, error) {
	userID := req.UserID
	if userID == "" {
		userID = o.defaultUserID
	}

	convID, priorMessages, err := o.resolveConversation(ctx, userID, req.Source, req.ConversationID)
	if err != nil {
		return InputResponse{}, err
	}

	memContent, err := o.memory.Read(userID)
	if err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: read memory: %w", err)
	}

	if _, err := o.history.AppendMessage(ctx, convID, "user", req.Text); err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: record user message: %w", err)
	}

	messages := append([]llm.Message{{Role: llm.RoleSystem, Content: o.buildSystemPrompt(memContent)}}, priorMessages...)
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: req.Text})

	tools, err := o.availableTools(ctx)
	if err != nil {
		return InputResponse{}, err
	}

	finalText, providerUsed, err := o.runAgentLoop(ctx, userID, convID, messages, tools)
	if err != nil {
		return InputResponse{}, err
	}

	if _, err := o.history.AppendMessage(ctx, convID, "assistant", finalText); err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: record assistant message: %w", err)
	}

	return InputResponse{ConversationID: convID, Reply: finalText, ProviderUsed: providerUsed}, nil
}
