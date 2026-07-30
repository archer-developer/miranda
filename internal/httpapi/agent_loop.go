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
				msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments, ProviderMetadata: tc.ProviderMetadata})
			}
			messages = append(messages, msg)
		case "tool":
			messages = append(messages, llm.Message{Role: llm.RoleTool, ToolCallID: m.ToolCallID, Content: m.Content})
		}
	}
	return open.ID, messages, nil
}

// buildSystemPrompt combines the base persona prompt with who is currently
// speaking and long-term memory, so both are available on every turn without
// re-deriving them from raw history. sharedMemory is household-wide facts
// injected first (from shared.md); userMemory is the per-user file that
// follows. Without the speaker identity, the model has no way to tell which
// of the household it is talking to — it can only guess from what gets said,
// which is exactly the kind of thing that should never need guessing.
func (o *Orchestrator) buildSystemPrompt(userID, sharedMemory, userMemory string) string {
	prompt := o.baseSystemPrompt
	if name := o.currentUserName(userID); name != "" {
		prompt += "\n\nСейчас с тобой разговаривает: " + name + "."
	}
	if sharedMemory != "" {
		prompt += "\n\nShared household memory:\n" + sharedMemory
	}
	if userMemory != "" {
		prompt += "\n\nWhat you remember about this user:\n" + userMemory
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
// agent's built-in tools (remember_this, etc.). The escalation tool is NOT
// added here: since each provider in the router's fallback/escalation chain
// can configure its own escalation target and tool name (see
// config.LLMProvider.Escalation), only the Router knows which one applies
// to whichever provider is active at a given hop — it appends that
// provider's own escalation ToolDef to this base list right before each
// Chat() call (see internal/llm/router.requestFor), and intercepts calls to
// it transparently, so it never reaches executeTool.
//
// Built-ins are collected first (into the closure-captured `tools`/`names`
// pair below) and MCP tools are filtered against that set afterward, rather
// than the reverse, because MCP tool names come from a live server
// (internal/mcp.Manager.Tools, prefixed "<serverName>_<toolName>") and
// aren't known until connect time — an MCP server whose prefixed name
// happens to collide with one of Miranda's own fixed tool names (e.g. a
// server named "web" exposing a tool "search") is dropped, with a warning,
// rather than silently shadowing (or being shadowed by) a built-in of the
// same name. Sending two ToolDefs with the same name to a provider isn't
// just confusing — Anthropic specifically rejects the request outright.
func (o *Orchestrator) availableTools(ctx context.Context) []llm.ToolDef {
	var tools []llm.ToolDef
	names := make(map[string]bool)
	add := func(t llm.ToolDef) {
		tools = append(tools, t)
		names[t.Name] = true
	}

	if o.memoryCfg.ExplicitTool {
		add(llm.ToolDef{
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
		add(llm.ToolDef{
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
		add(llm.ToolDef{
			Name: endConversationToolName,
			Description: "End the current conversation right now — use when the user explicitly asks to start a " +
				"new conversation (e.g. \"давай начнём новую беседу\", \"let's start a new conversation\"), " +
				"instead of waiting for the idle timeout to close it.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}

	if o.memoryCfg.ForgetConversationTool {
		add(llm.ToolDef{
			Name: forgetConversationToolName,
			Description: "Delete this entire conversation with no memory of it — use when the user explicitly asks " +
				"to forget this conversation or start completely from scratch (e.g. \"забудь\", \"забудь этот диалог\", " +
				"\"давай с начала\").",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}

	if o.ttsCfg.SpeakReplyTool {
		add(llm.ToolDef{
			Name: speakReplyToolName,
			Description: "Speak text out loud through the physical speaker, even though this request didn't arrive " +
				"via the voice pipeline — use only when the user explicitly asks to hear something read aloud " +
				"(e.g. \"озвучь это\", \"скажи вслух\", \"read that out loud\"). Pass the text to speak — normally " +
				"the same as your written reply, but reworded speech-friendly (no markdown, links, code) if the " +
				"reply itself wouldn't sound natural read verbatim.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{"type": "string", "description": "the text to speak aloud"},
				},
				"required": []string{"text"},
			},
		})
	}

	if o.tts != nil && o.ttsCfg.StopSpeechTool {
		add(llm.ToolDef{
			Name: stopSpeechToolName,
			Description: "Stop speaking immediately — use when the user explicitly asks Miranda to stop talking " +
				"(e.g. \"хватит\", \"замолчи\", \"stop talking\") — clears anything still queued and silences " +
				"whatever is currently playing on the physical speaker.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}

	for _, t := range o.webTools {
		add(t.Def())
	}

	if o.telegram != nil && o.telegramCfg.SendMessageTool {
		add(llm.ToolDef{
			Name: sendTelegramToolName,
			Description: "Send a text message to a household member's Telegram — use when the user explicitly asks " +
				"to send something to a phone (e.g. \"отправь мне на телефон ...\", \"send that to my phone\", " +
				"\"отправь Ане на телефон ...\"). Only works for someone who has messaged the bot at least once.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "the message to send",
					},
					"recipient": map[string]any{
						"type": "string",
						"description": "the household member's name, exactly as the user said it (e.g. \"Аня\") — " +
							"omit this to send to whoever is currently talking to you",
					},
				},
				"required": []string{"text"},
			},
		})
	}

	mcpTools := o.tools.Tools(ctx)
	out := make([]llm.ToolDef, 0, len(mcpTools)+len(tools))
	for _, t := range mcpTools {
		if names[t.Name] {
			o.hub.Publish(hub.Event{Source: "error", Message: fmt.Sprintf(
				"mcp tool %q collides with a built-in tool of the same name — dropping the mcp one", t.Name)})
			continue
		}
		out = append(out, t)
	}
	return append(out, tools...)
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

		o.recordAssistantToolCallMessage(ctx, userID, conversationID, text, toolCalls)
		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Content: text, ToolCalls: toolCalls})
		for _, tc := range toolCalls {
			result := o.executeTool(ctx, userID, tc, control)
			o.recordToolCall(ctx, userID, conversationID, tc, result)
			messages = append(messages, llm.Message{Role: llm.RoleTool, ToolCallID: tc.ID, Content: result})
		}
	}

	return "", "", fmt.Errorf("orchestrator: exceeded %d tool-call iterations without a final reply", maxToolIterations)
}

// streamOneTurn consumes one router.Chat stream: text deltas are pushed
// through a tts.Accumulator so complete sentences get spoken as soon as
// they're available, and tool calls are collected for the caller to execute.
//
// Only ha_assist gets its streamed text spoken live here — that's the one
// channel where the model's plain reply text is itself the thing to say out
// loud. Every other source stays silent on this path no matter what the
// model's text says; the only way those sources ever reach the Yandex
// Station is the model explicitly calling speak_reply with the text to
// speak (see executeTool) — a real tool call with a real argument, not a
// flag inferred from *which* turn happened to contain the model's answer.
// That sidesteps an entire class of bugs an earlier version of this code
// had: content blocks stream in the order the model produced them (text,
// then tool_use), so "speak whatever text this turn already streamed"
// requires guessing, after the fact, whether a turn's text was the answer
// or just a throwaway post-tool-call remark — get that guess wrong and the
// same text can end up spoken twice. Passing the text as the tool's own
// argument removes the guess entirely.
func (o *Orchestrator) streamOneTurn(ctx context.Context, source string, messages []llm.Message, tools []llm.ToolDef, providerUsed *string) (string, []llm.ToolCall, error) {
	stream, err := o.router.Chat(ctx, llm.ChatRequest{Messages: messages, Tools: tools}, func(name string) { *providerUsed = name })
	if err != nil {
		return "", nil, fmt.Errorf("orchestrator: chat: %w", err)
	}

	speakLive := source == users.SourceHAAssist

	var fullText string
	var toolCalls []llm.ToolCall
	acc := tts.NewAccumulator(o.chunkMaxChars)

	for chunk := range stream {
		if chunk.Err != nil {
			return "", nil, fmt.Errorf("orchestrator: stream: %w", chunk.Err)
		}
		if chunk.TextDelta != "" {
			fullText += chunk.TextDelta
			ready := acc.Push(chunk.TextDelta)
			if speakLive {
				o.speakChunks(ctx, ready)
			} else {
				o.publishChunks(ready)
			}
		}
		if chunk.ToolCall != nil {
			toolCalls = append(toolCalls, *chunk.ToolCall)
		}
	}
	final := acc.Flush()
	if speakLive {
		o.speakChunks(ctx, final)
	} else {
		o.publishChunks(final)
	}

	return fullText, toolCalls, nil
}

// publishChunks puts each chunk on the hub as an "assistant" event (chat/log
// visibility) without any TTS decision — used for every source other than
// ha_assist; see streamOneTurn.
func (o *Orchestrator) publishChunks(chunks []string) {
	for _, chunk := range chunks {
		o.hub.Publish(hub.Event{Source: "assistant", Message: chunk})
	}
}

// speakChunks publishes each chunk (see publishChunks) and dispatches it to
// TTS — only ever called for ha_assist turns (see streamOneTurn), the one
// channel where the reply needs to come out of a physical speaker, separate
// from — and in addition to — the text reply HA's own pipeline may speak
// (see README).
func (o *Orchestrator) speakChunks(ctx context.Context, chunks []string) {
	o.publishChunks(chunks)
	for _, chunk := range chunks {
		o.speakText(ctx, chunk)
	}
}

// speakText enqueues one already-voice-approved piece of text onto
// tts.primary's background Player (via Dispatcher.Speak) — it returns
// immediately without waiting for synthesis or physical playback, and any
// eventual failure is published to the hub by the Player itself (Source:
// "tts"), not returned here, so a broken speaker shouldn't stop the
// assistant from answering.
func (o *Orchestrator) speakText(ctx context.Context, text string) {
	if o.tts == nil || text == "" {
		return
	}
	o.tts.Speak(ctx, text)
}

// executeTool runs one tool call: locally (remember_this, search_history,
// end_conversation, forget_conversation), via an internal/tools.Tool
// (tavily_web_search, tavily_web_fetch — see o.webTools), or via the MCP tool manager.
// Errors are turned into a result string rather than aborting the turn, so
// the model can see what went wrong and react (apologize, retry
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

	if tc.Name == speakReplyToolName {
		var args struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		if args.Text == "" {
			return "error: text is required"
		}
		o.speakText(ctx, args.Text)
		return "spoken"
	}

	if tc.Name == stopSpeechToolName {
		if o.tts != nil {
			o.tts.Stop(ctx)
		}
		return "stopped"
	}

	if tc.Name == sendTelegramToolName {
		var args struct {
			Text      string `json:"text"`
			Recipient string `json:"recipient"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}

		targetUsername := userID
		if args.Recipient != "" {
			if o.users == nil {
				return fmt.Sprintf("error: no household member matches %q", args.Recipient)
			}
			target, ok := o.users.ResolveByDisplayName(args.Recipient)
			if !ok {
				return fmt.Sprintf("error: no household member matches %q", args.Recipient)
			}
			targetUsername = target.Username
		}

		if err := o.telegram.SendToUser(ctx, targetUsername, args.Text); err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return "sent"
	}

	for _, t := range o.webTools {
		if t.Def().Name != tc.Name {
			continue
		}
		result, err := t.Call(ctx, tc.Arguments)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return result
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
func (o *Orchestrator) recordAssistantToolCallMessage(ctx context.Context, userID, conversationID, text string, toolCalls []llm.ToolCall) {
	refs := make([]history.ToolCallRef, len(toolCalls))
	for i, tc := range toolCalls {
		refs[i] = history.ToolCallRef{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments, ProviderMetadata: tc.ProviderMetadata}
	}
	msgID, err := o.history.AppendAssistantMessage(ctx, conversationID, text, refs)
	if err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: "record assistant tool-call message: " + err.Error()})
		return
	}
	// Published even though the chat UI doesn't render tool-call turns today
	// (see chat.js's chatMessages filter) — sent so a future debug view can
	// show tool activity live without any backend change, matching how
	// GET /api/dialogs/{id} already returns these rows unfiltered.
	o.publishChatMessage(userID, conversationID, history.Message{ID: msgID, ConversationID: conversationID, Role: "assistant", Content: text, ToolCalls: refs})
}

func (o *Orchestrator) recordToolCall(ctx context.Context, userID, conversationID string, tc llm.ToolCall, result string) {
	msgID, err := o.history.AppendToolResultMessage(ctx, conversationID, tc.ID, result)
	if err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: "record tool call: " + err.Error()})
		return
	}
	if err := o.history.AppendToolCall(ctx, msgID, tc.Name, "", tc.Arguments, result); err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: "record tool call detail: " + err.Error()})
	}
	o.publishChatMessage(userID, conversationID, history.Message{ID: msgID, ConversationID: conversationID, Role: "tool", Content: result, ToolCallID: tc.ID})
}
