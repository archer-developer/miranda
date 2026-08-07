package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/archer-developer/miranda/internal/attachments"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/schedule"
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
	// downloadedFiles accumulates one entry per successful call to the
	// sandbox's download_file MCP tool this turn (see executeTool's
	// sandboxDownloadToolName check) — Handle reads this after the loop
	// finishes to append a <download>...</download> marker per file via
	// appendDownloadMarkers, deterministically, regardless of what the
	// model's own reply text says.
	downloadedFiles []downloadedFile
}

// downloadedFile captures the metadata the sandbox's download_file MCP tool
// reports in its result text (see downloadFileHandler in
// ../../miranda-code-execution-sandbox/internal/mcpserver/sessions.go).
type downloadedFile struct {
	fileID    string
	filename  string
	sizeBytes int64
	mimeType  string
}

// parseDownloadFileResult extracts file_id/filename/size_bytes/mime_type from
// the sandbox download_file tool's plain-text result. internal/mcp only ever
// surfaces MCP TextContent blocks (see SDKClient.CallTool) — there is no
// structured JSON to parse instead, so this reads the "key: value" lines
// download_file's handler formats its result as. ok is false if file_id or
// filename is missing, e.g. because the call itself failed and returned an
// error string instead of the success format.
//
// Only the FIRST line matching each key is kept, never the last: the
// sandbox's result text embeds the model-supplied filename verbatim (its
// only sanitization is filepath.Base(filepath.Clean(...)), which does not
// strip embedded newlines), so a session can create a file whose name is
// itself a crafted "\nfile_id: <victim-file-id>" line. Since the real
// file_id is always emitted before filename in the sandbox's fixed format,
// first-match-wins means an injected line appearing inside filename's own
// value can never override the genuine, server-generated file_id that
// already-attachStore.Put would otherwise associate with the wrong owner —
// see executeTool.
func parseDownloadFileResult(text string) (downloadedFile, bool) {
	var df downloadedFile
	for _, line := range strings.Split(text, "\n") {
		key, value, found := strings.Cut(line, ": ")
		if !found {
			continue
		}
		switch key {
		case "file_id":
			if df.fileID == "" {
				df.fileID = value
			}
		case "filename":
			if df.filename == "" {
				df.filename = value
			}
		case "size_bytes":
			if df.sizeBytes == 0 {
				if n, err := strconv.ParseInt(value, 10, 64); err == nil {
					df.sizeBytes = n
				}
			}
		case "mime_type":
			if df.mimeType == "" {
				df.mimeType = value
			}
		}
	}
	return df, df.fileID != "" && df.filename != ""
}

// appendDownloadMarkers deterministically appends one <download>...</download>
// marker per file the model retrieved via download_file this turn, so the web
// UI chat screen (chat.js's extractDownloadBlocks) can always render a working
// download chip instead of trusting the model to reproduce a link verbatim —
// the same "server, not the model, owns the reliable presentation" reasoning
// processAttachments applies on the inbound side. Each marker's payload is one
// line of JSON (not pipe-delimited) so a filename containing "|" or ">" can't
// break the client-side parse.
func appendDownloadMarkers(text string, files []downloadedFile) string {
	for _, f := range files {
		payload, err := json.Marshal(struct {
			FileID    string `json:"file_id"`
			Filename  string `json:"filename"`
			SizeBytes int64  `json:"size_bytes"`
			MIMEType  string `json:"mime_type"`
		}{f.fileID, f.filename, f.sizeBytes, f.mimeType})
		if err != nil {
			continue
		}
		text += "\n\n<download>" + string(payload) + "</download>"
	}
	return text
}

// downloadMarkerPattern matches one marker appendDownloadMarkers injects.
var downloadMarkerPattern = regexp.MustCompile(`\n\n<download>([\s\S]*?)</download>`)

// renderDownloadMarkersForChannel converts every <download>{json}</download>
// marker in text into a form the given source's client can actually render.
// Only the web UI's chat screen (chat.js's extractDownloadBlocks) knows to
// parse the raw marker syntax into a clickable chip, so it's passed through
// unchanged for source == webUISource; every other channel — ha_assist (HA's
// Assist pipeline speaks/displays the reply text as-is) and Telegram
// (telegram.go sends resp.Reply verbatim via the Bot API, with no
// client-side JS to parse anything) — would otherwise show or speak the
// literal JSON tag. Those channels get a plain-text line instead.
//
// text is expected to already carry markers from appendDownloadMarkers.
// Handle always persists the raw-marker form to history regardless of
// source, so the web UI's history browser can still render a chip for a
// conversation that originated on another channel — only the outbound reply
// for a non-web channel needs converting, not what's stored.
func renderDownloadMarkersForChannel(text, source string) string {
	if source == webUISource {
		return text
	}
	return downloadMarkerPattern.ReplaceAllStringFunc(text, func(match string) string {
		sub := downloadMarkerPattern.FindStringSubmatch(match)
		if len(sub) < 2 {
			return ""
		}
		var df struct {
			Filename  string `json:"filename"`
			SizeBytes int64  `json:"size_bytes"`
		}
		if err := json.Unmarshal([]byte(sub[1]), &df); err != nil || df.Filename == "" {
			return ""
		}
		if df.SizeBytes > 0 {
			return fmt.Sprintf("\n\n📎 %s (%s)", df.Filename, formatByteSize(df.SizeBytes))
		}
		return fmt.Sprintf("\n\n📎 %s", df.Filename)
	})
}

// formatByteSize renders a byte count as a short human-readable string,
// mirroring the web UI's formatFileSize (internal/webui/static/js/downloads.js)
// for the plain-text fallback renderDownloadMarkersForChannel produces.
func formatByteSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KB", n/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
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
//
// Both the display name and the raw userID are spelled out explicitly. The
// display name alone isn't enough: several external MCP servers are
// multi-tenant and take a technical user/user_id argument that is the
// userID itself, not the display name — e.g. yazio's "user" on every
// nutrition tool, or miranda-diary's "user_id" on add_record/search/remove
// (the latter doesn't even validate it against a known list, so a wrong
// value doesn't error, it just silently starts a new, unsearchable bucket).
// The display name and userID only coincide by lexical accident (e.g. "Аня"
// vs. "anna") for some household members and not others (e.g. "Саша" vs.
// "archer" — no relation at all). Without the userID spelled out, the model
// has been observed defaulting to whichever username it saw most recently in
// context rather than the current speaker's, silently misattributing tool
// calls (e.g. food log entries) to the wrong account. Miranda's own built-in
// tools don't have this problem — they take userID from the server-side
// session, never as a model-supplied argument — this is only for
// external/MCP tools whose schema asks the model for one explicitly.
//
// The user's current local time is injected so the model can correctly
// interpret relative time references ("в 22:00", "через 10 минут") and
// generate proper RFC3339 timestamps for create_scheduled_task.
func (o *Orchestrator) buildSystemPrompt(userID, sharedMemory, userMemory string) string {
	prompt := o.baseSystemPrompt
	if name := o.currentUserName(userID); name != "" {
		prompt += "\n\nСейчас с тобой разговаривает: " + name + " (технический userID: \"" + userID + "\"). " +
			"Если вызываешь MCP-тул с параметром user или user_id (yazio, diary и любой другой multi-tenant тул) — " +
			"передавай туда именно эту строку, \"" + userID + "\", а не имя " + name + " и не чьё-либо ещё имя из контекста."
	}
	now := time.Now().In(o.userLocation(userID))
	prompt += "\n\nТекущее время пользователя: " + now.Format("2006-01-02 15:04 MST") + "."
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

// userLocation returns the configured IANA timezone for userID, or time.Local
// when the user is not in the registry or has no timezone configured. Used by
// the scheduler to interpret cron expressions and format timestamps.
func (o *Orchestrator) userLocation(userID string) *time.Location {
	if o.users != nil {
		if u, ok := o.users.Get(userID); ok {
			return u.Location()
		}
	}
	return time.Local
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
				"(e.g. \"озвучь это\", \"расскажи голосом\", \"скажи вслух\", \"read that out loud\"). Pass the text to speak — normally " +
				"the same as your written reply, but reworded speech-friendly (no markdown, links, code) if the " +
				"reply itself wouldn't sound natural read verbatim.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{"type": "string", "description": "the text to speak aloud"},
					"device": map[string]any{
						"type":        "string",
						"description": "friendly name of the speaker to use (e.g. \"Станция Мини 3 Про\") — omit to use the default device",
					},
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

	if o.schedule != nil {
		add(llm.ToolDef{
			Name: createScheduledTaskToolName,
			Description: "Schedule a free-text instruction to be carried out later, either once or on a " +
				"recurring basis — use when the user explicitly asks to be reminded/have something done " +
				"at a future time (e.g. \"сегодня в 22:00 напомни мне...\", \"каждое утро в 9:01 ...\"). " +
				"The instruction is replayed through you later exactly like a live message from the user — " +
				"at that point you decide which of your own tools (speak_reply, send_telegram, etc.) to call " +
				"to actually carry it out, so write it as a clear, self-contained instruction, not a summary.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task": map[string]any{
						"type":        "string",
						"description": "the instruction to carry out when this fires, written exactly as you'd want to receive it as a live message",
					},
					"run_at": map[string]any{
						"type":        "string",
						"description": "RFC3339 datetime for a one-off task — use the user's current local timezone offset shown in the system prompt (e.g. \"2026-07-30T22:00:00+03:00\") — provide exactly one of run_at or schedule, never both",
					},
					"schedule": map[string]any{
						"type":        "string",
						"description": "5-field cron expression (minute hour day-of-month month day-of-week) for a recurring task — times are interpreted in the user's local timezone, e.g. \"1 9 * * *\" for every day at 09:01 local time, or \"20 22 * * 2\" for every Tuesday at 22:20 local time — provide exactly one of run_at or schedule, never both",
					},
				},
				"required": []string{"task"},
			},
		})

		add(llm.ToolDef{
			Name:        listScheduledTasksToolName,
			Description: "List this user's currently scheduled tasks (id, next run time, and instruction) — use when the user asks what's scheduled, or before deleting one.",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		})

		add(llm.ToolDef{
			Name:        deleteScheduledTaskToolName,
			Description: "Cancel a scheduled task by id (from list_scheduled_tasks) — use when the user asks to cancel/remove a reminder or scheduled routine.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string", "description": "the task id, from list_scheduled_tasks"},
				},
				"required": []string{"id"},
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
// feeds their results back in for the next iteration. providerUsed is
// threaded through every iteration via streamOneTurn/router.ChatPinned so
// that once a turn escalates (e.g. gemini-lite hands off to gemini-strong),
// every later iteration of THIS loop stays pinned to whichever provider
// answered last — a tool call the escalated model requested must be
// answered by that same model once the tool result comes back, not silently
// downgraded back to the chain's default provider on the very next
// iteration just because the tool call happened in between.
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

		// Strip image Parts from all user messages now that the model has
		// seen them on this first successful call. Subsequent iterations
		// re-send the full accumulated message history, and re-transmitting
		// large base64 blobs on every tool-call round-trip wastes both
		// tokens and latency — the model's context already holds the image.
		for j := range messages {
			messages[j].Parts = nil
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

// streamOneTurn consumes one router stream: text deltas are pushed through a
// tts.Accumulator so complete sentences get spoken as soon as they're
// available, and tool calls are collected for the caller to execute.
// providerUsed is read before the call to pin the request to whichever
// provider answered the previous iteration of this same agent loop (see
// runAgentLoop and router.ChatPinned) — "" on the loop's first iteration,
// which is a no-op and behaves exactly like an unpinned router.Chat.
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
	stream, err := o.router.ChatPinned(ctx, llm.ChatRequest{Messages: messages, Tools: tools}, *providerUsed, func(name string) { *providerUsed = name })
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
			Text   string `json:"text"`
			Device string `json:"device"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		if args.Text == "" {
			return "error: text is required"
		}
		if args.Device != "" && o.tts != nil {
			o.tts.SpeakTo(ctx, args.Text, args.Device)
		} else {
			o.speakText(ctx, args.Text)
		}
		return "spoken"
	}

	if tc.Name == stopSpeechToolName {
		if o.tts != nil {
			o.tts.Stop()
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

	if tc.Name == createScheduledTaskToolName {
		var args struct {
			Task     string `json:"task"`
			RunAt    string `json:"run_at"`
			Schedule string `json:"schedule"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		if args.Task == "" {
			return "error: task is required"
		}
		if (args.RunAt == "") == (args.Schedule == "") {
			return "error: provide exactly one of run_at or schedule"
		}

		task := schedule.Task{UserID: userID, Prompt: args.Task}
		if args.Schedule != "" {
			sched, err := cron.ParseStandard(args.Schedule)
			if err != nil {
				return fmt.Sprintf("error: invalid schedule expression: %v", err)
			}
			// Interpret the cron expression in the user's local timezone so
			// "1 9 * * *" means 09:01 in the user's time, not the server's.
			if specSched, ok := sched.(*cron.SpecSchedule); ok {
				specSched.Location = o.userLocation(userID)
			}
			task.CronExpr = args.Schedule
			task.NextRunAt = sched.Next(time.Now())
		} else {
			runAt, err := time.Parse(time.RFC3339, args.RunAt)
			if err != nil {
				return fmt.Sprintf("error: invalid run_at (expected RFC3339): %v", err)
			}
			if !runAt.After(time.Now()) {
				return "error: run_at is in the past"
			}
			task.RunAt = &runAt
			task.NextRunAt = runAt
		}

		id, err := o.schedule.Create(ctx, task)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return "scheduled: " + id
	}

	if tc.Name == listScheduledTasksToolName {
		tasksList, err := o.schedule.ListForUser(ctx, userID)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		if len(tasksList) == 0 {
			return "no scheduled tasks"
		}
		userLoc := o.userLocation(userID)
		var b strings.Builder
		for _, t := range tasksList {
			fmt.Fprintf(&b, "[%s] next: %s — %s\n", t.ID, t.NextRunAt.In(userLoc).Format(time.RFC3339), t.Prompt)
		}
		return b.String()
	}

	if tc.Name == deleteScheduledTaskToolName {
		var args struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		if err := o.schedule.Delete(ctx, args.ID, userID); err != nil {
			if err == schedule.ErrNotFound {
				return "error: no such scheduled task"
			}
			return fmt.Sprintf("error: %v", err)
		}
		return "deleted"
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

	// For alice MCP tool calls that carry a "device" argument (the target
	// speaker's friendly name), coordinate with the TTS Player before firing:
	// drain any queued TTS first (WaitIdle), then confirm the physical station
	// is idle via alice_state polling (WaitEntityIdle). This prevents a model
	// that batches speak_reply + an alice command from having the alice command
	// interrupt the TTS mid-utterance. Both steps are best-effort: errors are
	// published to the hub but never abort the tool call.
	if o.tts != nil && o.speakerHA != nil {
		if device := aliceToolDevice(tc.Arguments); device != "" {
			o.tts.WaitIdle(ctx)
			if entityID, err := o.speakerHA.ResolveMediaPlayer(ctx, device); err == nil {
				if err := o.tts.WaitEntityIdle(ctx, entityID); err != nil && ctx.Err() == nil {
					o.hub.Publish(hub.Event{Source: "error", Message: "speaker coordination: " + err.Error()})
				}
			}
		}
	}

	// Build the wire payload as a local variable, never overwriting
	// tc.Arguments itself — runAgentLoop already called
	// recordAssistantToolCallMessage with the model's original, unmutated
	// toolCalls before this iteration's executeTool call, and calls
	// recordToolCall with that same outer, unmutated tc right after this
	// call returns (Go passes tc into executeTool by value, so a local
	// mutation here is invisible to the caller's copy either way). Keeping
	// the mutation local-only is what keeps a real key from ever reaching
	// history's SQLite tables or internal/llmtrace's llm.log. See
	// docs/encryption.md.
	callArgs := tc.Arguments
	if o.keyring != nil {
		// key stays nil unless tc.Name targets a whitelisted server AND
		// userID's key is currently unlocked — setEncryptionKeyArg treats a
		// nil key as "strip", which also covers the not-whitelisted case:
		// a compromised/malicious MCP tool description must never be able
		// to trick the model into forwarding a previously-seen key value to
		// a different, non-whitelisted server. argName follows the same
		// resolution: it's only switched to the target server's own
		// configured EncryptionKeyArg() when that server is whitelisted, so
		// the strip path for a non-whitelisted call always falls back to
		// the package default name below.
		var key []byte
		argName := defaultEncryptionKeyArgName
		if server, ok := o.tools.ServerForTool(tc.Name); ok {
			if a, permitted := o.encryptionKeyAllowed[server]; permitted {
				argName = a
				key, _ = o.keyring.Get(userID)
			}
		}
		var decodeOK bool
		callArgs, decodeOK = setEncryptionKeyArg(tc.Arguments, argName, key)
		if !decodeOK && len(key) > 0 {
			o.hub.Publish(hub.Event{Source: "error", Message: "keyring: tool call arguments were not valid JSON, encryption key not attached to " + tc.Name})
		}
	}

	result, err := o.tools.Call(ctx, tc.Name, callArgs)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	// Recognize the sandbox's download_file tool by its namespaced MCP name
	// (set via SetSandboxDownload) and, on success, both remember who
	// requested it (attachStore, so GET /api/files/{file_id} can reject a
	// different household member fetching it) and queue it for
	// appendDownloadMarkers to inject into the final reply.
	if o.sandboxDownloadToolName != "" && tc.Name == o.sandboxDownloadToolName {
		if df, ok := parseDownloadFileResult(result); ok {
			control.downloadedFiles = append(control.downloadedFiles, df)
			if o.attachStore != nil {
				o.attachStore.Put(attachments.Record{
					UserID:   userID,
					FileID:   df.fileID,
					Filename: df.filename,
					MIMEType: df.mimeType,
					Size:     df.sizeBytes,
					TTL:      o.downloadRecordTTL,
				})
			}
		}
	}

	return result
}

// aliceToolDevice extracts the "device" string field from a tool call's JSON
// arguments, returning "" if the field is absent, not a string, or the JSON
// is malformed. Used by executeTool to detect alice MCP tool calls that
// target a speaker by friendly name.
func aliceToolDevice(args string) string {
	var m struct {
		Device string `json:"device"`
	}
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return ""
	}
	return m.Device
}

// defaultEncryptionKeyArgName is the tool-call argument name used to strip
// a model-supplied key field from a call to a server that isn't (or isn't
// currently known to be) whitelisted — see executeTool's argName
// resolution. A whitelisted server's own injected argument name instead
// comes from that server's config.MCPServer.EncryptionKeyArg(), which may
// override this default (e.g. the external miranda-diary repo's tools use
// "record_encryption_key", not this default).
const defaultEncryptionKeyArgName = "encryption_key"

// setEncryptionKeyArg sets or strips argName in args' JSON. A non-empty key
// sets it (lowercase hex-encoded, 64 chars for a 32-byte key — the encoding
// the external miranda-diary repo's schema requires, confirmed against its
// own parseEncryptionKey; the same real-world incident that necessitated
// the per-server EncryptionKeyArg() name override also surfaced this: an
// earlier version of this function base64-encoded instead, which a diary
// account with encryption disabled silently accepted (the raw value is
// wholly ignored server-side when disabled) until encryption was actually
// turned on for that account and every call started failing "must be 64
// lowercase hex characters"), overwriting any value the model may have set
// itself. A nil/empty key strips it instead — defense against a
// compromised/malicious MCP tool description tricking the model into
// forwarding a previously-seen key value somewhere it shouldn't go.
//
// Always decodes args (no substring pre-check): JSON allows any character,
// including every letter of argName, to be written as a \uXXXX escape, so a
// literal-substring fast path can be silently dodged by a tool call whose
// key field is spelled with an escape — exactly the prompt-injection
// scenario this function exists to defend against. The post-decode "field
// not present" check below is what actually avoids a wasted re-encode for
// the overwhelmingly common case (any non-whitelisted or locked-key call),
// safely, since it runs after the field has genuinely been looked for in
// the parsed object rather than guessed at from raw text.
//
// ok is false only when args itself isn't valid JSON — the caller should
// treat that as "the key was not attached" and log it when key was
// non-empty, since it means an intended injection silently didn't happen
// (a fully malformed tool call is rare and will fail against the MCP server
// on its own terms regardless, but a merely-unparseable-here edge case
// deserves a trace, not silence).
func setEncryptionKeyArg(args, argName string, key []byte) (result string, ok bool) {
	m, decodeOK := decodeToolArgs(args)
	if !decodeOK {
		return args, false
	}
	if len(key) == 0 {
		if _, present := m[argName]; !present {
			return args, true
		}
		delete(m, argName)
	} else {
		m[argName] = hex.EncodeToString(key)
	}
	return encodeToolArgs(m, args), true
}

func decodeToolArgs(args string) (map[string]any, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return nil, false
	}
	return m, true
}

// encodeToolArgs marshals m back to JSON, falling back to the original
// (pre-decode) args string on a marshal error — this should be unreachable
// since m was itself produced by unmarshaling valid JSON, but never worth
// silently losing a tool call's arguments over.
func encodeToolArgs(m map[string]any, original string) string {
	b, err := json.Marshal(m)
	if err != nil {
		return original
	}
	return string(b)
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
