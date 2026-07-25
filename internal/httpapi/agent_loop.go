package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/tts"
	"github.com/archer-developer/miranda/internal/users"
)

// searchHistoryLimit bounds how many past conversations the search_history
// tool feeds back to the model in one call, so a broad query doesn't blow up
// the prompt.
const searchHistoryLimit = 8

// turnControl lets a tool call executed mid-agent-loop (end_conversation,
// forget_conversation) signal Handle to close or delete the conversation
// once the turn finishes, instead of doing it immediately: the destructive
// action must wait until after the final assistant reply is recorded (and,
// via TTS, spoken), or that write would be orphaned/lost.
type turnControl struct {
	endRequested    bool
	forgetRequested bool
}

// resolveConversation either continues the user's currently open
// conversation (loading its prior turns so the model has context) or starts
// a new one. Continuity is server-owned and keyed only on userID — not on
// any conversation_id a caller might send — so the idle timeout and the
// explicit end_conversation/forget_conversation tools are what actually
// govern session boundaries, regardless of which channel (HA, web UI,
// future Telegram/mobile) a turn arrives on.
func (o *Orchestrator) resolveConversation(ctx context.Context, userID, source string) (string, []llm.Message, error) {
	open, err := o.history.OpenConversation(ctx, userID)
	if err != nil {
		return "", nil, fmt.Errorf("orchestrator: query open conversation: %w", err)
	}
	if open == nil {
		convID, err := o.history.StartConversation(ctx, userID, source)
		if err != nil {
			return "", nil, fmt.Errorf("orchestrator: start conversation: %w", err)
		}
		return convID, nil, nil
	}

	stored, err := o.history.ConversationMessages(ctx, open.ID)
	if err != nil {
		return "", nil, fmt.Errorf("orchestrator: load conversation %s: %w", open.ID, err)
	}

	// Prior turns are reconstructed as plain user/assistant text messages;
	// tool-call/tool-result rows are dropped rather than replayed — their
	// structure doesn't survive a fresh HTTP request anyway (an accepted v1
	// simplification), and mislabeling a tool result as if the user said it
	// would actively confuse the model on every later turn of a long-lived
	// session.
	messages := make([]llm.Message, 0, len(stored))
	for _, m := range stored {
		var role llm.Role
		switch m.Role {
		case "user":
			role = llm.RoleUser
		case "assistant":
			role = llm.RoleAssistant
		default:
			continue
		}
		messages = append(messages, llm.Message{Role: role, Content: m.Content})
	}
	return open.ID, messages, nil
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

	if o.memoryCfg.SearchHistoryTool {
		tools = append(tools, llm.ToolDef{
			Name: searchHistoryToolName,
			Description: "Search this user's past conversations for something they said earlier — use it when " +
				"they reference an earlier conversation (e.g. \"помнишь мы говорили о...\", \"remember when we talked about...\").",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "keywords to search for, in the same language the user used",
					},
				},
				"required": []string{"query"},
			},
		})
	}

	if o.memoryCfg.EndConversationTool {
		tools = append(tools, llm.ToolDef{
			Name: endConversationToolName,
			Description: "End the current conversation right now — use when the user explicitly asks to start a " +
				"new conversation (e.g. \"давай начнём новую беседу\", \"let's start a new conversation\"), " +
				"instead of waiting for the idle timeout to close it.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}

	if o.memoryCfg.ForgetConversationTool {
		tools = append(tools, llm.ToolDef{
			Name: forgetConversationToolName,
			Description: "Delete this entire conversation with no memory of it — use when the user explicitly asks " +
				"to forget this conversation or start completely from scratch (e.g. \"забудь этот диалог\", " +
				"\"давай с начала\").",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
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
func (o *Orchestrator) runAgentLoop(ctx context.Context, userID, conversationID, source string, messages []llm.Message, tools []llm.ToolDef, control *turnControl) (string, string, error) {
	var providerUsed string

	for i := 0; i < maxToolIterations; i++ {
		text, toolCalls, err := o.streamOneTurn(ctx, source, messages, tools, &providerUsed)
		if err != nil {
			return "", "", err
		}

		if len(toolCalls) == 0 {
			return text, providerUsed, nil
		}

		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Content: text, ToolCalls: toolCalls})
		for _, tc := range toolCalls {
			result := o.executeTool(ctx, userID, tc, control)
			o.recordToolCall(ctx, conversationID, tc, result)
			messages = append(messages, llm.Message{Role: llm.RoleTool, ToolCallID: tc.ID, Content: result})
		}
	}

	return "", "", fmt.Errorf("orchestrator: exceeded %d tool-call iterations without a final reply", maxToolIterations)
}

// streamOneTurn consumes one router.Chat stream: text deltas are pushed
// through a tts.Accumulator so complete sentences get spoken as soon as
// they're available, and tool calls are collected for the caller to execute.
func (o *Orchestrator) streamOneTurn(ctx context.Context, source string, messages []llm.Message, tools []llm.ToolDef, providerUsed *string) (string, []llm.ToolCall, error) {
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
			o.speakChunks(ctx, source, acc.Push(chunk.TextDelta))
		}
		if chunk.ToolCall != nil {
			toolCalls = append(toolCalls, *chunk.ToolCall)
		}
	}
	o.speakChunks(ctx, source, acc.Flush())

	return fullText, toolCalls, nil
}

// speakChunks dispatches each ready chunk to TTS, but only for turns that
// arrived via Home Assistant's voice pipeline (source == ha_assist): that's
// the one channel where the reply also needs to come out of a physical
// speaker, separate from (and in addition to) the text reply HA's pipeline
// itself may speak (see README). Every other channel — web UI, a future
// Telegram bot or mobile app — already has its own output surface (the HTTP
// response), so dispatching to the shared Yandex Station there would just
// make it talk at you unprompted. TTS itself is best-effort: a dispatch
// failure is logged to the hub but never fails the turn — a broken speaker
// shouldn't stop the assistant from answering.
func (o *Orchestrator) speakChunks(ctx context.Context, source string, chunks []string) {
	for _, chunk := range chunks {
		o.hub.Publish(hub.Event{Source: "assistant", Message: chunk})
		if o.tts == nil || source != users.SourceHAAssist {
			continue
		}
		if err := o.tts.Speak(ctx, chunk); err != nil {
			o.hub.Publish(hub.Event{Source: "tts", Message: "speak failed: " + err.Error()})
		}
	}
}

// executeTool runs one tool call, either locally (remember_this,
// search_history, end_conversation, forget_conversation) or via the MCP tool
// manager. Errors are turned into a result string rather than aborting the
// turn, so the model can see what went wrong and react (apologize, retry
// differently) instead of the whole request failing.
func (o *Orchestrator) executeTool(ctx context.Context, userID string, tc llm.ToolCall, control *turnControl) string {
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

	if tc.Name == searchHistoryToolName {
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		results, err := o.history.SearchConversations(ctx, userID, args.Query, searchHistoryLimit)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		if len(results) == 0 {
			return "no matching past conversations found"
		}
		var b strings.Builder
		for _, c := range results {
			fmt.Fprintf(&b, "[%s] %s\n", c.StartedAt.Format("2006-01-02"), c.Summary)
		}
		return b.String()
	}

	if tc.Name == endConversationToolName {
		control.endRequested = true
		return "conversation ended"
	}

	if tc.Name == forgetConversationToolName {
		control.forgetRequested = true
		return "conversation forgotten"
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
