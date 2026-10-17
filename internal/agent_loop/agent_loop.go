package agentloop

import (
	"context"
	"errors"
	"fmt"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtrace/anomaly"
	"github.com/archer-developer/miranda/internal/chunk"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/replyformat"
)

// turnControl lets a tool call executed mid-agent-loop (end_conversation,
// forget_conversation) signal Handle to close or delete the conversation
// once the turn finishes, instead of doing it immediately: the destructive
// action must wait until after the final assistant reply is recorded (and,
// via TTS, spoken), or that write would be orphaned/lost.
type turnControl struct {
	// source is the request source of this turn (users.SourceHAAssist, ...);
	// availableTools uses it to withhold speak_reply/stop_speech from
	// ha_assist turns, which the Assist pipeline already voices.
	source          string
	endRequested    bool
	forgetRequested bool
	// restoreConversationID is set by executeTool when the model calls
	// restore_conversation, naming the past conversation (already ended) to
	// bring back as the new active session — "" means no restore was
	// requested this turn. Deferred exactly like endRequested/
	// forgetRequested: Handle only acts on it after this turn's own reply is
	// safely recorded — see Orchestrator.restoreConversation.
	restoreConversationID string
	// loadedGroups accumulates which lazy MCP server names load_tool_group
	// has expanded so far THIS turn — reset implicitly every turn since
	// turnControl is constructed fresh in Handle. Never persisted to
	// history/memory: re-collapsing to the compact stub on the next turn is
	// intentional, not a bug — see docs/adr/lazy-mcp-tool-loading.md §3/§4.
	loadedGroups map[string]bool
	// groupsChanged is set by executeTool whenever loadedGroups grew this
	// iteration, so runAgentLoop knows to recompute the tool list before the
	// next call instead of doing it unconditionally every iteration.
	groupsChanged bool
	// downloadedFiles accumulates one entry per distinct remote file URI
	// executeTool's file-URI detector finds in a file-exposing server's
	// tool result this turn (see detectRemoteFileLinks) — Handle reads this
	// after the loop finishes and records it as the turn's structured
	// history.Message.Downloads (via toDownloadRefs), deterministically,
	// regardless of what the model's own reply text says.
	downloadedFiles []downloadedFile
	// remoteFileURLs dedups the detector's finds within this turn, keyed by
	// the exact remote URL — a document fetched twice in one turn (e.g.
	// get_document called again for the same documentId) must not get two
	// separate chips.
	remoteFileURLs map[string]bool
}

// hasRemoteFile reports whether url was already recorded this turn via
// recordRemoteFile.
func (c *turnControl) hasRemoteFile(url string) bool {
	return c.remoteFileURLs[url]
}

// recordRemoteFile marks url as seen this turn, for hasRemoteFile.
func (c *turnControl) recordRemoteFile(url string) {
	if c.remoteFileURLs == nil {
		c.remoteFileURLs = make(map[string]bool)
	}
	c.remoteFileURLs[url] = true
}

// recordDownloadedFile appends df to downloadedFiles, replacing — not
// skipping — any earlier entry recorded this turn with the same
// filename/size/mime. A second dedup layer on top of hasRemoteFile, needed
// because the sandbox mints a fresh file_id (and thus a fresh file_uri) on
// every download_file call even when the underlying file is unchanged, e.g.
// the model re-downloading the same generated document after a later tool
// call in the same turn — URL-only dedup misses that case since the two
// URLs genuinely differ. Replacing rather than skipping matters: the
// sandbox does not keep every previously-issued file_id resolvable
// indefinitely, so of two file_ids for what's nominally the same file, it's
// consistently the *earlier* one whose /files/<id> proxy request 404s by
// the time the chip is clicked — keeping the first (as an earlier version
// of this dedup did) surfaced one permanently broken chip instead of the
// one that still works. See git history for the incident.
func (c *turnControl) recordDownloadedFile(df downloadedFile) {
	for i, existing := range c.downloadedFiles {
		if existing.filename == df.filename && existing.sizeBytes == df.sizeBytes && existing.mimeType == df.mimeType {
			c.downloadedFiles[i] = df
			return
		}
	}
	c.downloadedFiles = append(c.downloadedFiles, df)
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
// outcome, if non-nil, is filled in with what only this loop knows
// authoritatively about how the turn ended (see anomaly.Outcome and
// Handle's own doc comment on why that can't be reliably reconstructed from
// the trace text alone) — passed in by Handle rather than returned
// alongside (string, string, error) so a caller with anomaly detection
// disabled pays nothing beyond one always-allocated struct.
func (o *Orchestrator) runAgentLoop(ctx context.Context, userID, conversationID, source string, messages []llm.Message, tools []llm.ToolDef, control *turnControl, outcome *anomaly.Outcome) (string, string, error) {
	var providerUsed string

	for i := 0; i < maxToolIterations; i++ {
		text, toolCalls, err := o.streamOneTurn(ctx, messages, tools, &providerUsed)
		if err != nil {
			outcome.IterationCount = i + 1
			outcome.TimedOut = errors.Is(err, context.DeadlineExceeded)
			return "", "", err
		}

		if len(toolCalls) == 0 {
			outcome.IterationCount = i + 1
			// A provider can stream Done with zero TextDelta and zero
			// ToolCall chunks and still report no error — observed in
			// practice from Gemini silently swallowing a prompt-level
			// safety block or an unpopulated finish reason (see
			// gemini.Provider.attempt's PromptFeedback/finishReason
			// handling, miranda-llm). Treating that as a valid "final
			// answer" here would forward an empty string all the way to
			// the end user as a silent blank reply, with nothing in
			// history or logs/anomalies/ to explain why. Erroring instead
			// routes through the same o.turnFailureReply(userID) fallback
			// every other agent-loop failure already uses, so the user
			// sees an honest "something went wrong" instead of nothing.
			if text == "" {
				return "", "", fmt.Errorf("orchestrator: provider returned an empty final reply with no tool calls")
			}
			return text, providerUsed, nil
		}

		// Image Parts are deliberately NOT stripped here. Gemini/Anthropic/
		// OpenAI's chat-completion APIs are stateless — every call resends
		// the full message history, and nothing on the server retains what
		// an earlier call in this same loop saw (Gemini's own context
		// caching isn't wired up — see gemini.ToolsConfig.ContextCaching).
		// An earlier version stripped Parts after the first successful call
		// on the theory that "the model's context already holds the
		// image" — false for a stateless API, and it silently blinded the
		// model on every iteration after the first tool call: a photo
		// question that needs even one tool round-trip (e.g. this
		// household's medical_card nutrition-guidance lookup, always
		// triggered before answering "what can I eat") got answered from a
		// text placeholder alone, with the model fabricating plausible-
		// looking but wrong contents (mandarins, then bananas, for a photo
		// of an apple and bell peppers) — 100% reproducible, confirmed by
		// replaying the exact captured request with and without the image
		// present. Re-sending the (now resized, see
		// processAttachments/imageutil) image bytes on every iteration
		// costs some tokens/latency, but a correct answer beats a fast
		// wrong one.
		o.recordAssistantToolCallMessage(ctx, userID, conversationID, text, toolCalls)
		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Content: text, ToolCalls: toolCalls})
		for _, tc := range toolCalls {
			result := o.executeTool(ctx, userID, conversationID, source, tc, control)
			o.recordToolCall(ctx, userID, conversationID, tc, result)
			messages = append(messages, llm.Message{Role: llm.RoleTool, ToolCallID: tc.ID, Content: result})
		}

		// A load_tool_group call this iteration means the next call's tool
		// list must grow to include that server's real schemas — see
		// availableTools' control param and docs/adr/lazy-mcp-tool-loading.md.
		// Skipped when nothing changed so an ordinary iteration doesn't pay
		// for rebuilding the list (which, unlike the built-ins half, may hit
		// every connected MCP server) on every single turn.
		if control.groupsChanged {
			tools = o.availableTools(ctx, userID, control)
			control.groupsChanged = false
		}
	}

	outcome.HitIterationCap = true
	outcome.IterationCount = maxToolIterations
	return "", "", fmt.Errorf("orchestrator: exceeded %d tool-call iterations without a final reply", maxToolIterations)
}

// publishChunkMaxChars bounds one assistant-text chunk published to the hub
// (chat/log visibility) while a reply streams in.
const publishChunkMaxChars = 200

// streamOneTurn consumes one router stream: text deltas are pushed through a
// chunk.Accumulator so complete sentences reach the hub (live chat/log
// view) as soon as they're available, and tool calls are collected for the
// caller to execute. providerUsed is read before the call to pin the request
// to whichever provider answered the previous iteration of this same agent
// loop (see runAgentLoop and router.ChatPinned) — "" on the loop's first
// iteration, which is a no-op and behaves exactly like an unpinned
// router.Chat.
//
// Nothing is spoken from here, for any source. An ha_assist reply is voiced
// by Home Assistant's Assist pipeline (its TTS engine is miranda-tts over
// Wyoming) from the text this turn returns to conversation.py; every other
// source stays silent unless the model explicitly calls speak_reply with the
// text to speak (see executeTool) — a real tool call with a real argument,
// not a flag inferred from *which* turn happened to contain the model's
// answer. That sidesteps an entire class of bugs an earlier version of this
// code had: content blocks stream in the order the model produced them
// (text, then tool_use), so "speak whatever text this turn already
// streamed" requires guessing, after the fact, whether a turn's text was the
// answer or just a throwaway post-tool-call remark — get that guess wrong
// and the same text can end up spoken twice. Passing the text as the tool's
// own argument removes the guess entirely.
func (o *Orchestrator) streamOneTurn(ctx context.Context, messages []llm.Message, tools []llm.ToolDef, providerUsed *string) (string, []llm.ToolCall, error) {
	stream, err := o.router.ChatPinned(ctx, llm.ChatRequest{Messages: messages, Tools: tools}, *providerUsed, func(name string) { *providerUsed = name })
	if err != nil {
		return "", nil, fmt.Errorf("orchestrator: chat: %w", err)
	}

	var fullText string
	var toolCalls []llm.ToolCall
	acc := chunk.NewAccumulator(publishChunkMaxChars)

	for part := range stream {
		if part.Err != nil {
			return "", nil, fmt.Errorf("orchestrator: stream: %w", part.Err)
		}
		if part.TextDelta != "" {
			fullText += part.TextDelta
			o.publishChunks(acc.Push(part.TextDelta))
		}
		if part.ToolCall != nil {
			toolCalls = append(toolCalls, *part.ToolCall)
		}
	}
	o.publishChunks(acc.Flush())

	return fullText, toolCalls, nil
}

// publishChunks puts each chunk on the hub as an "assistant" event (chat/log
// visibility).
func (o *Orchestrator) publishChunks(chunks []string) {
	for _, c := range chunks {
		o.hub.Publish(hub.Event{Source: "assistant", Message: c})
	}
}

// voiceText strips markdown-lite markup down to plain spoken text (see
// replyformat.ToVoiceText) — applied to the reply returned to Home
// Assistant's pipeline, to speak_reply's argument and to spoken reminders.
func voiceText(text string) string {
	return replyformat.ToVoiceText(replyformat.Parse(text))
}
