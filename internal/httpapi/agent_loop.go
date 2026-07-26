package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/archer-developer/miranda/internal/history"
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

	// Prior turns are replayed in full, including tool calls and their
	// results (see history.Message.ToolCallID / ToolCalls, populated by
	// AppendAssistantMessage / AppendToolResultMessage) — not just the plain
	// user/assistant text, so the model resuming this conversation sees
	// exactly the same tool activity it saw when it originally ran.
	messages := make([]llm.Message, 0, len(stored))
	for _, m := range stored {
		switch m.Role {
		case "user":
			messages = append(messages, llm.Message{Role: llm.RoleUser, Content: m.Content})
		case "assistant":
			msg := llm.Message{Role: llm.RoleAssistant, Content: m.Content}
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
			}
			messages = append(messages, msg)
		case "tool":
			messages = append(messages, llm.Message{Role: llm.RoleTool, ToolCallID: m.ToolCallID, Content: m.Content})
		}
	}
	return open.ID, messages, nil
}

// buildSystemPrompt combines the base persona prompt with who is currently
// speaking and the user's distilled long-term memory, so both are available
// on every turn without re-deriving them from raw history. Without the
// speaker identity, the model has no way to tell which of the household it
// is talking to in a given conversation — it can only guess from what gets
// said, which is exactly the kind of thing that should never need guessing.
func (o *Orchestrator) buildSystemPrompt(userID, memory string) string {
	prompt := o.baseSystemPrompt
	if name := o.currentUserName(userID); name != "" {
		prompt += "\n\nСейчас с тобой разговаривает: " + name + "."
	}
	if memory != "" {
		prompt += "\n\nWhat you remember about this user:\n" + memory
	}
	return prompt
}

// currentUserName resolves userID to a human-readable display name via the
// users registry, if one is configured. Falls back to the bare userID when
// there's no registry, no match (e.g. the "debug" fallback id, or ad-hoc
// curl/testing), or an empty id — better to name the raw id than to say
// nothing about who's speaking.
func (o *Orchestrator) currentUserName(userID string) string {
	if o.users == nil || userID == "" {
		return userID
	}
	if u, ok := o.users.Get(userID); ok {
		return u.DisplayName()
	}
	return userID
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

		o.recordAssistantToolCallMessage(ctx, conversationID, text, toolCalls)
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

// recordAssistantToolCallMessage persists the assistant's turn that
// requested toolCalls (text may be empty when the model replies with only
// tool calls), so resolveConversation can replay it on a later turn instead
// of dropping it. Best-effort: a failure here is logged but must not abort
// the turn — the caller already has the tool calls in hand to execute.
func (o *Orchestrator) recordAssistantToolCallMessage(ctx context.Context, conversationID, text string, toolCalls []llm.ToolCall) {
	refs := make([]history.ToolCallRef, len(toolCalls))
	for i, tc := range toolCalls {
		refs[i] = history.ToolCallRef{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}
	}
	if _, err := o.history.AppendAssistantMessage(ctx, conversationID, text, refs); err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: "record assistant tool-call message: " + err.Error()})
	}
}

func (o *Orchestrator) recordToolCall(ctx context.Context, conversationID string, tc llm.ToolCall, result string) {
	msgID, err := o.history.AppendToolResultMessage(ctx, conversationID, tc.ID, result)
	if err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: "record tool call: " + err.Error()})
		return
	}
	if err := o.history.AppendToolCall(ctx, msgID, tc.Name, "", tc.Arguments, result); err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: "record tool call detail: " + err.Error()})
	}
}
