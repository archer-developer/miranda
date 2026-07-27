package httpapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/llmtrace"
)

// preferencesSection is the memory section the summarization pass owns. It's
// separate from remember_this's append-only "Remembered" section (see
// internal/memory) so an LLM-derived re-summarization can never clobber a
// fact the user explicitly asked to be remembered.
const preferencesSection = "Preferences"

const summaryMarker = "## Summary"

const preferencesMarker = "## Preferences"

// summarizeSystemPrompt asks the model for two things in one call, to avoid
// double-billing the idle sweep: a short per-conversation recap (what
// search_history surfaces when the user asks "помнишь, мы говорили о...")
// and an updated durable-facts memory section. The recap is distinct from
// extracting durable facts — it's fine (expected, even) for it to mention
// one-off details that Preferences must not.
const summarizeSystemPrompt = `You maintain two things for a home voice assistant: a short recap of one
finished conversation, and a durable memory file about the user.
You'll be given the user's existing "Preferences" notes and the transcript of one finished conversation.
Reply with exactly two sections, in this order:

## Summary
A short (1-3 sentence) recap of what was discussed in this conversation, so it can be found and
recalled later if the user references it (e.g. "помнишь, мы говорили о...").

## Preferences
An updated bullet list of durable facts, preferences, and recurring patterns worth remembering in
future conversations.
- Do not restate details that only matter for this one conversation.
- Do not repeat facts already covered by the existing notes unless they changed.
- If nothing in the transcript is worth remembering long-term, leave this section empty.

Reply with only these two sections — no extra commentary.`

// SummarizeIdleSessions finds conversations that have sat idle past idleFor,
// distills each into the user's memory (internal/memory), and marks them
// ended so the sweeper doesn't revisit them. It's meant to be called
// periodically by a background ticker (see cmd/miranda), not from the
// request path — a conversation "ending" isn't a signal Home Assistant or
// the web UI ever sends explicitly.
func (o *Orchestrator) SummarizeIdleSessions(ctx context.Context, idleFor time.Duration) error {
	idle, err := o.history.IdleConversations(ctx, idleFor)
	if err != nil {
		return fmt.Errorf("orchestrator: list idle conversations: %w", err)
	}

	for _, conv := range idle {
		if err := o.summarizeConversation(ctx, conv.ID, conv.UserID); err != nil {
			// One bad conversation shouldn't block the rest of the sweep; it's
			// left un-ended so the next sweep retries it.
			o.hub.Publish(hub.Event{Source: "error", Message: fmt.Sprintf("summarize conversation %s: %v", conv.ID, err)})
			continue
		}
	}
	return nil
}

// summarizeConversation distills convID (belonging to userID) into a short
// recap (stored on the conversation itself) and, if anything durable came up,
// an updated Preferences memory section — then marks the conversation ended.
// Shared by the idle sweep and the explicit end_conversation tool, so both
// paths close a session the same way.
func (o *Orchestrator) summarizeConversation(ctx context.Context, convID, userID string) error {
	ctx = llmtrace.WithConversationID(ctx, convID)

	messages, err := o.history.ConversationMessages(ctx, convID)
	if err != nil {
		return fmt.Errorf("load messages: %w", err)
	}
	if len(messages) == 0 {
		if err := o.history.EndConversation(ctx, convID); err != nil {
			return err
		}
		o.publishConversationEnded(userID, convID)
		return nil
	}

	existing, err := o.memory.Read(userID)
	if err != nil {
		return fmt.Errorf("read memory: %w", err)
	}

	summary, preferences, err := o.distillConversation(ctx, existing, messages)
	if err != nil {
		return fmt.Errorf("distill: %w", err)
	}

	if strings.TrimSpace(preferences) != "" {
		if err := o.memory.ReplaceSection(userID, preferencesSection, preferences); err != nil {
			return fmt.Errorf("write memory: %w", err)
		}
	}

	if err := o.history.EndConversationWithSummary(ctx, convID, summary); err != nil {
		return err
	}
	o.publishConversationEnded(userID, convID)
	return nil
}

// publishConversationEnded notifies the user's open GET /ws/chat/{username}
// tab(s) that this conversation just closed (idle sweep or the explicit
// end_conversation tool) — shared by both summarizeConversation return paths
// above so neither can end a session without the UI finding out.
func (o *Orchestrator) publishConversationEnded(userID, convID string) {
	o.hub.Publish(hub.Event{Source: "chat", UserID: userID, Data: ChatEvent{Type: "conversation_ended", ConversationID: convID}})
}

// distillConversation asks the LLM router for a conversation recap plus an
// updated Preferences section body, given the current memory and one
// conversation's transcript. It uses the router directly (no tools, no TTS)
// since this is a background text distillation, not a user-facing turn.
func (o *Orchestrator) distillConversation(ctx context.Context, existingMemory string, transcript []history.Message) (summary, preferences string, err error) {
	var b strings.Builder
	b.WriteString("Existing Preferences notes:\n")
	if existingMemory == "" {
		b.WriteString("(none yet)\n")
	} else {
		b.WriteString(existingMemory)
	}
	b.WriteString("\nConversation transcript:\n")
	for _, m := range transcript {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
	}

	req := llm.ChatRequest{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: summarizeSystemPrompt},
		{Role: llm.RoleUser, Content: b.String()},
	}}

	stream, err := o.router.Chat(ctx, req, nil)
	if err != nil {
		return "", "", fmt.Errorf("chat: %w", err)
	}

	var text string
	for chunk := range stream {
		if chunk.Err != nil {
			return "", "", fmt.Errorf("stream: %w", chunk.Err)
		}
		text += chunk.TextDelta
	}

	summary, preferences = splitSummaryAndPreferences(text)
	return summary, preferences, nil
}

// splitSummaryAndPreferences parses the two "## Summary" / "## Preferences"
// sections out of the model's reply. If the model doesn't follow the
// expected shape (e.g. omits the Preferences heading), the whole reply is
// treated as the summary and preferences comes back empty — a safe
// degradation, since an empty preferences body is already a no-op for the
// caller.
func splitSummaryAndPreferences(text string) (summary, preferences string) {
	text = strings.TrimSpace(text)

	var summaryPart, preferencesPart string
	if idx := strings.Index(text, preferencesMarker); idx >= 0 {
		summaryPart = text[:idx]
		preferencesPart = text[idx+len(preferencesMarker):]
	} else {
		summaryPart = text
	}

	summaryPart = strings.TrimSpace(summaryPart)
	summaryPart = strings.TrimPrefix(summaryPart, summaryMarker)
	summaryPart = strings.TrimSpace(summaryPart)

	return summaryPart, strings.TrimSpace(preferencesPart)
}
