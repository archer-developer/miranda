// Package llm defines the provider-agnostic chat interface that both the
// OpenAI-compatible client and the native Anthropic client implement, so the
// rest of the agent (router, orchestrator) never depends on a specific SDK.
package llm

import "context"

// Role identifies who authored a Message in a conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is a single function/tool invocation requested by the model.
type ToolCall struct {
	ID        string // provider-assigned id, echoed back in the tool result message
	Name      string
	Arguments string // raw JSON arguments, as emitted by the model
}

// Message is one turn in the conversation sent to/received from a Provider.
type Message struct {
	Role Role
	// Content is the text content. Empty for an assistant message that only
	// contains tool calls.
	Content string
	// ToolCalls is set on assistant messages that invoke one or more tools.
	ToolCalls []ToolCall
	// ToolCallID identifies which ToolCall this message answers, and is only
	// set on Role == RoleTool messages.
	ToolCallID string
}

// ToolDef describes one tool the model is allowed to call, in JSON Schema
// form (shared shape across OpenAI-compatible and Anthropic backends).
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema object
}

// ChatRequest is a full chat turn: history plus the tools available this turn.
type ChatRequest struct {
	Messages []Message
	Tools    []ToolDef
}

// StreamChunk is one increment of a streaming chat response. A stream ends
// either with Err set, or with the channel closing after a chunk carrying
// Done == true.
type StreamChunk struct {
	// TextDelta is a piece of assistant text to append, e.g. for TTS chunking
	// as it becomes available (see internal/tts).
	TextDelta string
	// ToolCall is set once when the provider finishes emitting a complete
	// tool call (providers stream tool-call arguments incrementally
	// internally, but Provider implementations buffer and emit it whole here
	// since partial JSON arguments aren't independently useful downstream).
	ToolCall *ToolCall
	Done     bool
	Err      error
}

// Provider is a chat backend: either an OpenAI-compatible endpoint (local or
// hosted, see internal/llm/openaicompat) or native Anthropic
// (internal/llm/anthropic). Implementations must be safe for concurrent use.
type Provider interface {
	// Name is the provider's configured name, used in logs and router fallback.
	Name() string
	// Chat starts a streaming chat completion. The returned channel is closed
	// once the stream ends (either after a Done chunk or an Err chunk). A
	// non-nil error return means the request could not even be started
	// (e.g. auth/connection failure).
	Chat(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error)
}
