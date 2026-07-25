package router

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/llm/llmtest"
	"github.com/archer-developer/miranda/internal/llmtrace"
)

func drainText(t *testing.T, ch <-chan llm.StreamChunk) string {
	t.Helper()
	var text string
	for chunk := range ch {
		require.NoError(t, chunk.Err)
		text += chunk.TextDelta
	}
	return text
}

func noEscalation() config.EscalationConfig {
	return config.EscalationConfig{Enabled: false}
}

func TestRouter_UsesFirstHealthyProvider(t *testing.T) {
	primary := llmtest.New("local", llmtest.Response{Text: "hi from local"})
	r, err := New([]llm.Provider{primary}, noEscalation())
	require.NoError(t, err)

	var used string
	ch, err := r.Chat(context.Background(), llm.ChatRequest{}, func(name string) { used = name })
	require.NoError(t, err)
	require.Equal(t, "hi from local", drainText(t, ch))
	require.Equal(t, "local", used)
}

func TestRouter_FallsBackOnConnectionError(t *testing.T) {
	broken := llmtest.New("broken", llmtest.Response{Err: errors.New("connection refused")})
	backup := llmtest.New("backup", llmtest.Response{Text: "hi from backup"})
	r, err := New([]llm.Provider{broken, backup}, noEscalation())
	require.NoError(t, err)

	var used string
	ch, err := r.Chat(context.Background(), llm.ChatRequest{}, func(name string) { used = name })
	require.NoError(t, err)
	require.Equal(t, "hi from backup", drainText(t, ch))
	require.Equal(t, "backup", used)
}

func TestRouter_AllProvidersFailReturnsError(t *testing.T) {
	broken1 := llmtest.New("broken1", llmtest.Response{Err: errors.New("down")})
	broken2 := llmtest.New("broken2", llmtest.Response{Err: errors.New("down too")})
	r, err := New([]llm.Provider{broken1, broken2}, noEscalation())
	require.NoError(t, err)

	_, err = r.Chat(context.Background(), llm.ChatRequest{}, nil)
	require.Error(t, err)
}

func TestRouter_EscalatesToTargetProviderOnToolCall(t *testing.T) {
	escalation := config.EscalationConfig{
		Enabled:        true,
		ToolName:       "escalate_to_claude",
		TargetProvider: "claude",
	}

	local := llmtest.New("local-qwen", llmtest.Response{
		ToolCall: &llm.ToolCall{ID: "call-1", Name: "escalate_to_claude", Arguments: `{"reason":"too hard"}`},
	})
	claude := llmtest.New("claude", llmtest.Response{Text: "the sophisticated answer"})

	r, err := New([]llm.Provider{local, claude}, escalation)
	require.NoError(t, err)

	var used string
	ch, err := r.Chat(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "a hard question"}},
	}, func(name string) { used = name })
	require.NoError(t, err)
	require.Equal(t, "the sophisticated answer", drainText(t, ch))
	require.Equal(t, "claude", used)

	// The target provider must have received the original user turn plus the
	// escalation tool call/result so it has full context.
	require.Len(t, claude.Requests, 1)
	msgs := claude.Requests[0].Messages
	require.Len(t, msgs, 3)
	require.Equal(t, llm.RoleUser, msgs[0].Role)
	require.Equal(t, llm.RoleAssistant, msgs[1].Role)
	require.Equal(t, "escalate_to_claude", msgs[1].ToolCalls[0].Name)
	require.Equal(t, llm.RoleTool, msgs[2].Role)
	require.Equal(t, "call-1", msgs[2].ToolCallID)
}

func TestRouter_EscalationTargetNotConfiguredReturnsErrChunk(t *testing.T) {
	escalation := config.EscalationConfig{Enabled: true, ToolName: "escalate_to_claude", TargetProvider: "claude"}
	local := llmtest.New("local-qwen", llmtest.Response{
		ToolCall: &llm.ToolCall{ID: "call-1", Name: "escalate_to_claude"},
	})
	r, err := New([]llm.Provider{local}, escalation)
	require.NoError(t, err)

	ch, err := r.Chat(context.Background(), llm.ChatRequest{}, nil)
	require.NoError(t, err)

	var gotErr error
	for chunk := range ch {
		if chunk.Err != nil {
			gotErr = chunk.Err
		}
	}
	require.Error(t, gotErr)
}

func TestRouter_TracesRequestAndResponseWhenTracerSet(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "hi"})
	r, err := New([]llm.Provider{provider}, noEscalation())
	require.NoError(t, err)

	var buf bytes.Buffer
	r.SetTracer(llmtrace.New(&buf))

	ch, err := r.Chat(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "hi", drainText(t, ch))

	out := buf.String()
	require.Contains(t, out, "provider=local")
	require.Contains(t, out, "user: hello")
	require.Contains(t, out, "text: hi")
}

func TestRouter_TracesEscalationAsTwoBlocks(t *testing.T) {
	escalation := config.EscalationConfig{Enabled: true, ToolName: "escalate_to_claude", TargetProvider: "claude"}
	local := llmtest.New("local-qwen", llmtest.Response{
		ToolCall: &llm.ToolCall{ID: "call-1", Name: "escalate_to_claude", Arguments: `{"reason":"hard"}`},
	})
	claude := llmtest.New("claude", llmtest.Response{Text: "the answer"})
	r, err := New([]llm.Provider{local, claude}, escalation)
	require.NoError(t, err)

	var buf bytes.Buffer
	r.SetTracer(llmtrace.New(&buf))

	ch, err := r.Chat(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "a hard question"}},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "the answer", drainText(t, ch))

	out := buf.String()
	require.Contains(t, out, "provider=local-qwen")
	require.Contains(t, out, "escalate_to_claude")
	require.Contains(t, out, "provider=claude")
	require.Contains(t, out, "text: the answer")
}

func TestRouter_NoTracerSetIsFine(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "hi"})
	r, err := New([]llm.Provider{provider}, noEscalation())
	require.NoError(t, err)

	ch, err := r.Chat(context.Background(), llm.ChatRequest{}, nil)
	require.NoError(t, err)
	require.Equal(t, "hi", drainText(t, ch))
}
