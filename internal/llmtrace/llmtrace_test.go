package llmtrace

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/llm"
)

func TestRecord_IncludesSystemPromptMessagesToolsAndResponse(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)

	req := llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "You are Miranda."},
			{Role: llm.RoleUser, Content: "включи свет"},
		},
		Tools: []llm.ToolDef{{Name: "ha_light_turn_on"}, {Name: "remember_this"}},
	}
	toolCalls := []llm.ToolCall{{ID: "call-1", Name: "ha_light_turn_on", Arguments: `{"entity_id":"light.living_room"}`}}

	l.Record(context.Background(), req, "claude", "", toolCalls, nil)

	out := buf.String()
	require.Contains(t, out, "provider=claude")
	require.Contains(t, out, "system: You are Miranda.")
	require.Contains(t, out, "user: включи свет")
	require.Contains(t, out, "tools: ha_light_turn_on, remember_this")
	require.Contains(t, out, `tool_call: ha_light_turn_on({"entity_id":"light.living_room"})`)
}

func TestRecord_IncludesConversationIDFromContext(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)

	ctx := WithConversationID(context.Background(), "conv-123")
	l.Record(ctx, llm.ChatRequest{}, "local", "hi", nil, nil)

	require.Contains(t, buf.String(), "conversation=conv-123")
}

func TestRecord_OmitsConversationIDWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)

	l.Record(context.Background(), llm.ChatRequest{}, "local", "hi", nil, nil)

	require.NotContains(t, buf.String(), "conversation=")
}

func TestRecord_RendersErrorInsteadOfResponse(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)

	l.Record(context.Background(), llm.ChatRequest{}, "local", "", nil, errors.New("boom"))

	require.Contains(t, buf.String(), "error: boom")
}

func TestRecord_NilLoggerIsANoOp(t *testing.T) {
	var l *Logger
	require.NotPanics(t, func() {
		l.Record(context.Background(), llm.ChatRequest{}, "local", "hi", nil, nil)
	})
}

func TestRecord_SerializesConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)

	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			l.Record(context.Background(), llm.ChatRequest{}, "local", "hi", nil, nil)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}

	// Every block must be fully written (no interleaved/corrupted output):
	// exactly 20 well-formed "=== ..." header lines.
	require.Equal(t, 20, strings.Count(buf.String(), "=== "))
}
