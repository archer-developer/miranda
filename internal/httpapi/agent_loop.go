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
	// speakRequested is set by the speak_reply tool: the model's signal that
	// the user explicitly asked to hear this turn's reply aloud, so
	// speakChunks should dispatch to TTS even on a source other than
	// ha_assist.
	speakRequested bool
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
				"to forget this conversation or start completely from scratch (e.g. \"забудь\", \"забудь этот диалог\", " +
				"\"давай с начала\").",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}

	if o.ttsCfg.SpeakReplyTool {
		tools = append(tools, llm.ToolDef{
			Name: speakReplyToolName,
			Description: "Speak this turn's reply out loud, even though the request didn't arrive via the voice " +
				"pipeline — use only when the user explicitly asks to hear the answer read aloud " +
				"(e.g. \"озвучь это\", \"скажи вслух\", \"read that out loud\").",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}

	if o.tts != nil && o.ttsCfg.StopSpeechTool {
		tools = append(tools, llm.ToolDef{
			Name: stopSpeechToolName,
			Description: "Stop speaking immediately — use when the user explicitly asks Miranda to stop talking " +
				"(e.g. \"хватит\", \"замолчи\", \"stop talking\") — clears anything still queued and silences " +
				"whatever is currently playing on the physical speaker.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}

	if o.telegram != nil && o.telegramCfg.SendMessageTool {
		tools = append(tools, llm.ToolDef{
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
		text, toolCalls, err := o.streamOneTurn(ctx, source, control, messages, tools, &providerUsed)
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
//
// voiceKnownUpfront captures whether this turn should be spoken *before* the
// stream has produced anything — true for ha_assist, or when an earlier
// turn in this same request already called speak_reply. When it's false, we
// can't speak chunks live as they arrive: a model can place its entire
// answer in this turn's text and only afterward emit the speak_reply tool
// call asking for it to be voiced (content blocks stream in the order the
// model produced them — text, then tool_use — so the tool call is only
// known once the text is already fully generated). In that case chunks are
// only published to the hub as they stream, and — if a speak_reply call
// does show up once the stream ends — the whole turn's text is spoken in
// one shot afterward, instead of being silently dropped because the voice
// decision arrived one turn too late.
func (o *Orchestrator) streamOneTurn(ctx context.Context, source string, control *turnControl, messages []llm.Message, tools []llm.ToolDef, providerUsed *string) (string, []llm.ToolCall, error) {
	stream, err := o.router.Chat(ctx, llm.ChatRequest{Messages: messages, Tools: tools}, func(name string) { *providerUsed = name })
	if err != nil {
		return "", nil, fmt.Errorf("orchestrator: chat: %w", err)
	}

	voiceKnownUpfront := source == users.SourceHAAssist || control.speakRequested

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
			if voiceKnownUpfront {
				o.speakChunks(ctx, source, control, ready)
			} else {
				o.publishChunks(ready)
			}
		}
		if chunk.ToolCall != nil {
			toolCalls = append(toolCalls, *chunk.ToolCall)
		}
	}
	final := acc.Flush()
	if voiceKnownUpfront {
		o.speakChunks(ctx, source, control, final)
	} else {
		o.publishChunks(final)
	}

	if !voiceKnownUpfront {
		for _, tc := range toolCalls {
			if tc.Name == speakReplyToolName {
				control.speakRequested = true
				o.speakText(ctx, fullText)
				break
			}
		}
	}

	return fullText, toolCalls, nil
}

// publishChunks puts each chunk on the hub as an "assistant" event (chat/log
// visibility) without any TTS decision — used for text streamed before we
// yet know whether this turn should be voiced; see streamOneTurn.
func (o *Orchestrator) publishChunks(chunks []string) {
	for _, chunk := range chunks {
		o.hub.Publish(hub.Event{Source: "assistant", Message: chunk})
	}
}

// speakChunks publishes each chunk (see publishChunks) and, for turns that
// either arrived via Home Assistant's voice pipeline (source == ha_assist:
// that's the one channel where the reply also needs to come out of a
// physical speaker, separate from — and in addition to — the text reply
// HA's pipeline itself may speak, see README) or where the speak_reply tool
// was called this turn because the user explicitly asked to hear the answer
// (control.speakRequested), also dispatches it to TTS. Every other turn must
// never trigger the shared Yandex Station, or testing via the web UI's debug
// box would make it talk unprompted.
func (o *Orchestrator) speakChunks(ctx context.Context, source string, control *turnControl, chunks []string) {
	o.publishChunks(chunks)
	if source != users.SourceHAAssist && !control.speakRequested {
		return
	}
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

	if tc.Name == speakReplyToolName {
		control.speakRequested = true
		return "will speak the reply aloud"
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
