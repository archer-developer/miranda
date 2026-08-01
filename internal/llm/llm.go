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
	// ProviderMetadata is an opaque, provider-specific blob (base64-encoded
	// if the provider's own data is binary) that must be echoed back
	// verbatim on a later turn for providers that require it — e.g.
	// Gemini's "thought signature" on a function-call part (see
	// internal/llm/gemini's toLLMToolCall/toGeminiContents): omitting it on
	// a later turn's replayed tool call is a hard 400 from the API, not
	// just degraded quality. Empty and unused by providers that don't need
	// this (Anthropic, OpenAI-compat).
	ProviderMetadata string
}

// ContentPart is one non-text content block within a user message —
// currently only image data for vision-capable models. Plain text turns
// always use Message.Content and leave Parts nil; Parts is only non-empty
// when the user attached an image file that can be inlined for vision.
type ContentPart struct {
	// ImageBase64 is the base64-encoded image bytes. Set MIMEType to the
	// image's actual MIME type (e.g. "image/jpeg", "image/png",
	// "image/gif", "image/webp") so providers know how to decode it.
	ImageBase64 string
	// MIMEType is the MIME type of ImageBase64's data.
	MIMEType string
}

// Message is one turn in the conversation sent to/received from a Provider.
type Message struct {
	Role Role
	// Content is the text content. Empty for an assistant message that only
	// contains tool calls.
	Content string
	// Parts carries multi-modal content blocks (image data for vision models)
	// alongside Content. Non-nil only for user messages that include uploaded
	// images; plain text turns always use Content and leave Parts nil.
	// Providers that don't support vision ignore Parts silently.
	Parts []ContentPart
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

// Tracer receives one record per provider call: the exact request and
// response payloads, already serialized (typically to JSON) by the
// Provider that built them — only the Provider knows its own SDK's wire
// format precisely enough for the trace to be trustworthy. This is what
// lets a trace capture provider-specific specifics the shared
// ChatRequest/StreamChunk types can't represent — e.g. Anthropic's own
// server-side web_search/web_fetch/code_execution tools, which a Provider
// adds to the real request itself and which never appear in the
// ChatRequest.Tools the Orchestrator built. response is empty when err is
// non-nil. Implemented by internal/llmtrace.Logger; a Provider that
// supports tracing accepts one via an optional SetTracer(Tracer) method,
// which internal/llm/router.Router forwards to on Router.SetTracer.
type Tracer interface {
	Trace(ctx context.Context, provider, request, response string, err error)
}
