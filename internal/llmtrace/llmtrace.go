// Package llmtrace records a human-readable trace of every request sent to
// an LLM provider and what it produced in response — the exact system
// prompt, message history, and available tools that went out, and the final
// text/tool calls (or error) that came back. This is the "why didn't my
// prompt work" debugging aid: internal/llm/router writes one block per
// provider call here, independent of the general application log.
package llmtrace

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/archer-developer/miranda/internal/llm"
)

type ctxKey int

const conversationIDKey ctxKey = iota

// WithConversationID attaches a conversation id to ctx so any trace recorded
// further down this call chain can be correlated with the rest of that
// conversation's logs. Absent (e.g. a background distillation call not tied
// to one specific conversation), the trace block just omits it.
func WithConversationID(ctx context.Context, conversationID string) context.Context {
	return context.WithValue(ctx, conversationIDKey, conversationID)
}

func conversationIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(conversationIDKey).(string)
	return id
}

// Logger writes one formatted block per traced call to an underlying
// io.Writer (typically a rotating log file — see cmd/miranda). A nil
// *Logger is valid and Record on it is a no-op, so tracing can be wired up
// as an optional dependency (see router.Router.SetTracer).
type Logger struct {
	mu sync.Mutex
	w  io.Writer
}

// New builds a Logger writing to w.
func New(w io.Writer) *Logger {
	return &Logger{w: w}
}

// Record writes one trace block for a single provider call: the request
// that went out (system prompt, message history, tool names) and what came
// back (final text, tool calls, or err if the call failed). Safe for
// concurrent use — writes are serialized so concurrent turns can't
// interleave mid-block.
func (l *Logger) Record(ctx context.Context, req llm.ChatRequest, provider, responseText string, toolCalls []llm.ToolCall, err error) {
	if l == nil {
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "=== %s provider=%s", time.Now().Format(time.RFC3339), provider)
	if convID := conversationIDFrom(ctx); convID != "" {
		fmt.Fprintf(&b, " conversation=%s", convID)
	}
	b.WriteString(" ===\n")

	for _, m := range req.Messages {
		if m.Role == llm.RoleSystem {
			fmt.Fprintf(&b, "system: %s\n", m.Content)
		}
	}

	b.WriteString("messages:\n")
	for _, m := range req.Messages {
		if m.Role == llm.RoleSystem {
			continue
		}
		fmt.Fprintf(&b, "  %s: %s\n", m.Role, formatMessageBody(m))
	}

	if len(req.Tools) > 0 {
		names := make([]string, len(req.Tools))
		for i, t := range req.Tools {
			names[i] = t.Name
		}
		fmt.Fprintf(&b, "tools: %s\n", strings.Join(names, ", "))
	}

	b.WriteString("--- response ---\n")
	switch {
	case err != nil:
		fmt.Fprintf(&b, "error: %v\n", err)
	default:
		if responseText != "" {
			fmt.Fprintf(&b, "text: %s\n", responseText)
		}
		for _, tc := range toolCalls {
			fmt.Fprintf(&b, "tool_call: %s(%s)\n", tc.Name, tc.Arguments)
		}
		if responseText == "" && len(toolCalls) == 0 {
			b.WriteString("(empty)\n")
		}
	}
	b.WriteString("\n")

	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.w, b.String())
}

// formatMessageBody renders a message's content, or its tool calls if it's
// an assistant message that only invoked tools (Content is empty in that
// case — see llm.Message).
func formatMessageBody(m llm.Message) string {
	if len(m.ToolCalls) > 0 {
		parts := make([]string, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			parts[i] = fmt.Sprintf("%s(%s)", tc.Name, tc.Arguments)
		}
		return strings.Join(parts, ", ")
	}
	return m.Content
}
