package httpapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/tts"
)

// resolveConversation either continues an existing conversation (loading its
// prior turns so the model has context) or starts a new one.
//
// Prior turns are reconstructed as plain user/assistant text messages —
// tool-call structure isn't replayed from history, which is an accepted v1
// simplification: it only affects continuing a conversation across separate
// HTTP requests, not the in-flight agent loop within a single request.
func (o *Orchestrator) resolveConversation(ctx context.Context, userID, source, conversationID string) (string, []llm.Message, error) {
	if conversationID == "" {
		convID, err := o.history.StartConversation(ctx, userID, source)
		if err != nil {
			return "", nil, fmt.Errorf("orchestrator: start conversation: %w", err)
		}
		return convID, nil, nil
	}

	stored, err := o.history.ConversationMessages(ctx, conversationID)
	if err != nil {
		return "", nil, fmt.Errorf("orchestrator: load conversation %s: %w", conversationID, err)
	}

	messages := make([]llm.Message, 0, len(stored))
	for _, m := range stored {
		role := llm.RoleUser
		if m.Role == "assistant" {
			role = llm.RoleAssistant
		}
		messages = append(messages, llm.Message{Role: role, Content: m.Content})
	}
	return conversationID, messages, nil
}

// buildSystemPrompt combines the base persona prompt with the user's
// distilled long-term memory, so it's available on every turn without
// re-deriving it from raw history.
func (o *Orchestrator) buildSystemPrompt(memory string) string {
	if memory == "" {
		return o.baseSystemPrompt
	}
	return o.baseSystemPrompt + "\n\nWhat you remember about this user:\n" + memory
}

// availableTools combines every connected MCP server's tools with the
// agent's built-in tools (remember_this, and the escalation tool if enabled
// — the router intercepts calls to it transparently, so it never reaches
// executeTool).
func (o *Orchestrator) availableTools(ctx context.Context) ([]llm.ToolDef, error) {
	mcpTools, err := o.tools.Tools(ctx)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: list MCP tools: %w", err)
	}

	tools := append([]llm.ToolDef{}, mcpTools...)

	if o.memoryCfg.ExplicitTool {
		tools = append(tools, llm.ToolDef{
			Name:        rememberToolName,
			Description: "Remember a durable fact about the current user for future conversations (preferences, recurring context).",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"fact": map[string]any{"type": "string"}},
				"required":   []string{"fact"},
			},
		})
	}

	if o.escalationCfg.Enabled {
		tools = append(tools, llm.ToolDef{
			Name:        o.escalationCfg.ToolName,
			Description: "Hand this turn off to a more capable model when the request is too complex, ambiguous, or high-stakes for you to handle well.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"reason": map[string]any{"type": "string"}},
			},
		})
	}

	return tools, nil
}

// runAgentLoop drives the model until it produces a final text-only reply:
// each iteration streams a response, executes any requested tool calls, and
// feeds their results back in for the next iteration.
func (o *Orchestrator) runAgentLoop(ctx context.Context, userID, conversationID string, messages []llm.Message, tools []llm.ToolDef) (string, string, error) {
	var providerUsed string

	for i := 0; i < maxToolIterations; i++ {
		text, toolCalls, err := o.streamOneTurn(ctx, messages, tools, &providerUsed)
		if err != nil {
			return "", "", err
		}

		if len(toolCalls) == 0 {
			return text, providerUsed, nil
		}

		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Content: text, ToolCalls: toolCalls})
		for _, tc := range toolCalls {
			result := o.executeTool(ctx, userID, tc)
			o.recordToolCall(ctx, conversationID, tc, result)
			messages = append(messages, llm.Message{Role: llm.RoleTool, ToolCallID: tc.ID, Content: result})
		}
	}

	return "", "", fmt.Errorf("orchestrator: exceeded %d tool-call iterations without a final reply", maxToolIterations)
}

// streamOneTurn consumes one router.Chat stream: text deltas are pushed
// through a tts.Accumulator so complete sentences get spoken as soon as
// they're available, and tool calls are collected for the caller to execute.
func (o *Orchestrator) streamOneTurn(ctx context.Context, messages []llm.Message, tools []llm.ToolDef, providerUsed *string) (string, []llm.ToolCall, error) {
	stream, err := o.router.Chat(ctx, llm.ChatRequest{Messages: messages, Tools: tools}, func(name string) { *providerUsed = name })
	if err != nil {
		return "", nil, fmt.Errorf("orchestrator: chat: %w", err)
	}

	var fullText string
	var toolCalls []llm.ToolCall
	acc := tts.NewAccumulator(o.chunkMaxChars)

	for chunk := range stream {
		if chunk.Err != nil {
			return "", nil, fmt.Errorf("orchestrator: stream: %w", chunk.Err)
		}
		if chunk.TextDelta != "" {
			fullText += chunk.TextDelta
			o.speakChunks(ctx, acc.Push(chunk.TextDelta))
		}
		if chunk.ToolCall != nil {
			toolCalls = append(toolCalls, *chunk.ToolCall)
		}
	}
	o.speakChunks(ctx, acc.Flush())

	return fullText, toolCalls, nil
}

// speakChunks dispatches each ready chunk to TTS. TTS is best-effort: a
// dispatch failure is logged to the hub but never fails the turn — a broken
// speaker shouldn't stop the assistant from answering.
func (o *Orchestrator) speakChunks(ctx context.Context, chunks []string) {
	for _, chunk := range chunks {
		o.hub.Publish(hub.Event{Source: "assistant", Message: chunk})
		if o.tts == nil {
			continue
		}
		if err := o.tts.Speak(ctx, chunk); err != nil {
			o.hub.Publish(hub.Event{Source: "tts", Message: "speak failed: " + err.Error()})
		}
	}
}

// executeTool runs one tool call, either locally (remember_this) or via the
// MCP tool manager. Errors are turned into a result string rather than
// aborting the turn, so the model can see what went wrong and react
// (apologize, retry differently) instead of the whole request failing.
func (o *Orchestrator) executeTool(ctx context.Context, userID string, tc llm.ToolCall) string {
	if tc.Name == rememberToolName {
		var args struct {
			Fact string `json:"fact"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		if err := o.memory.Remember(userID, args.Fact); err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return "remembered"
	}

	result, err := o.tools.Call(ctx, tc.Name, tc.Arguments)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return result
}

func (o *Orchestrator) recordToolCall(ctx context.Context, conversationID string, tc llm.ToolCall, result string) {
	msgID, err := o.history.AppendMessage(ctx, conversationID, "tool", result)
	if err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: "record tool call: " + err.Error()})
		return
	}
	if err := o.history.AppendToolCall(ctx, msgID, tc.Name, "", tc.Arguments, result); err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: "record tool call detail: " + err.Error()})
	}
}
