package agentloop

import (
	"context"
	"fmt"
	"strings"
	"time"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtrace"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/prompts"
)

// preferencesSection is the memory section the summarization pass owns. It's
// separate from remember_this's append-only "Remembered" section (see
// internal/memory) so an LLM-derived re-summarization can never clobber a
// fact the user explicitly asked to be remembered.
const preferencesSection = "Preferences"

const summaryMarker = "## Summary"

const preferencesMarker = "## Preferences"

// sharedMarker is no longer part of what this pass asks the model to
// produce (see summarizeSystemPrompt's doc comment) — kept only so
// splitSummaryPreferences can defensively trim off a stray "## Shared"
// heading the model imitates from the "Existing Shared notes" context it
// was shown, rather than accidentally folding that content into Preferences.
const sharedMarker = "## Shared"

// summarizeSystemPrompt (see prompts.Summarize, internal/prompts/summarize.md)
// asks the model for two things in one call, to avoid double-billing the
// idle sweep: a short per-conversation recap (what search_history surfaces
// when the user asks "помнишь, мы говорили о...."), and an updated
// durable-facts Preferences section for this one user. The recap is
// distinct from extracting durable facts — it's fine (expected, even) for
// it to mention one-off details that Preferences must not.
//
// This pass deliberately never writes to shared.md (household memory) —
// only the live remember_this(scope="shared") tool call does that. An
// earlier version also asked the model for a "## Shared" section here and
// wrote each line straight to memory.RememberShared, to close the gap
// where a household fact mentioned in conversation but never explicitly
// flagged live had no path into shared memory at all. In practice that
// path let shared.md balloon: the model routinely judged something
// belonging to one household member specifically (medical events, meal
// logs, personal errands) as "worth telling the whole household" simply
// because it affects them too, and every such judgment call is a
// permanent, append-only line nobody asked for. Losing that one gap is the
// accepted tradeoff for shared.md only ever growing from something a user
// (or the model, live, on their behalf) explicitly asked to remember.
var summarizeSystemPrompt = prompts.Summarize

// SummarizeIdleSessions finds conversations that have sat idle past idleFor
// and marks them ended, distilling each into the user's memory
// (internal/memory) first when memoryCfg.AutoSummarize allows it and the
// distillation call itself succeeds (see summarizeConversation) — it's meant
// to be called periodically by a background ticker (see cmd/miranda), not
// from the request path, since a conversation "ending" isn't a signal Home
// Assistant or the web UI ever sends explicitly.
func (o *Orchestrator) SummarizeIdleSessions(ctx context.Context, idleFor time.Duration) error {
	idle, err := o.history.IdleConversations(ctx, idleFor)
	if err != nil {
		return fmt.Errorf("orchestrator: list idle conversations: %w", err)
	}

	for _, conv := range idle {
		if err := o.summarizeConversation(ctx, conv.ID, conv.UserID); err != nil {
			// Only a genuine storage/IO failure reaches here — see
			// summarizeConversation's own doc comment for why a failed LLM
			// distillation never does. One bad conversation shouldn't block
			// the rest of the sweep; it's left un-ended so the next sweep
			// retries it.
			o.hub.Publish(hub.Event{Source: "error", Message: fmt.Sprintf("summarize conversation %s: %v", conv.ID, err)})
			continue
		}
	}
	return nil
}

// summarizeConversation always closes convID (belonging to userID) —
// idle-timeout closure must never depend on summarization succeeding, or
// even being attempted. When o.memoryCfg.AutoSummarize is true and the
// conversation has messages, it first tries to distill a short recap (stored
// on the conversation itself) and, if anything durable came up, an updated
// Preferences/Shared memory section. A failed distillation call (LLM error,
// rate limit, exhausted escalation chain, ...) is logged and skipped, not
// propagated as an error: the conversation still ends on this same pass,
// without a recap, rather than being left open for the sweep to retry
// forever — a persistently broken summarization provider must never keep a
// session pinned open, since that would silently stop conversation_id from
// ever rotating past its idle timeout. Only a genuine storage/IO failure
// (reading/writing history or memory) is returned as an error, so the sweep
// can legitimately retry those. Shared by the idle sweep and the explicit
// end_conversation tool, so both paths close a session the same way.
func (o *Orchestrator) summarizeConversation(ctx context.Context, convID, userID string) error {
	ctx = llmtrace.WithConversationID(ctx, convID)

	messages, err := o.history.ConversationMessages(ctx, convID)
	if err != nil {
		return fmt.Errorf("load messages: %w", err)
	}

	if len(messages) > 0 && o.memoryCfg.AutoSummarize {
		summary, ok, err := o.tryDistillConversation(ctx, convID, userID, messages)
		if err != nil {
			return err
		}
		if ok {
			if err := o.history.EndConversationWithSummary(ctx, convID, summary); err != nil {
				return err
			}
			o.clearConversationMemory(convID)
			o.publishConversationEnded(userID, convID)
			return nil
		}
	}

	if err := o.history.EndConversation(ctx, convID); err != nil {
		return err
	}
	o.clearConversationMemory(convID)
	o.publishConversationEnded(userID, convID)
	return nil
}

// tryDistillConversation attempts the LLM-based recap/memory-distillation
// step for summarizeConversation. ok is false (with a nil error) when the
// distillation call itself fails — logged to the hub here so the caller can
// fall back to closing the conversation without a recap instead of treating
// this as a retryable failure (see summarizeConversation's doc comment). A
// non-nil error means a genuine storage/IO failure reading existing memory,
// which the caller should propagate so the sweep retries this conversation.
func (o *Orchestrator) tryDistillConversation(ctx context.Context, convID, userID string, messages []history.Message) (summary string, ok bool, err error) {
	existing, err := o.memory.Read(userID)
	if err != nil {
		return "", false, fmt.Errorf("read memory: %w", err)
	}
	// Read-only context for the prompt (helps it avoid restating a fact the
	// household already has via Shared) — this pass never writes here; see
	// summarizeSystemPrompt's doc comment for why.
	existingShared, err := o.memory.ReadShared()
	if err != nil {
		return "", false, fmt.Errorf("read shared memory: %w", err)
	}

	summary, preferences, err := o.distillConversation(ctx, existing, existingShared, messages)
	if err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: fmt.Sprintf("summarize conversation %s: distill: %v", convID, err)})
		return "", false, nil
	}

	if hasContent(preferences) {
		if err := o.memory.ReplaceSection(userID, preferencesSection, preferences); err != nil {
			return "", false, fmt.Errorf("write memory: %w", err)
		}
	}

	return summary, true, nil
}

// publishConversationEnded notifies the user's open GET /ws/chat/{username}
// tab(s) that this conversation just closed (idle sweep or the explicit
// end_conversation tool) — shared by both summarizeConversation return paths
// above so neither can end a session without the UI finding out.
func (o *Orchestrator) publishConversationEnded(userID, convID string) {
	o.hub.Publish(hub.Event{Source: "chat", UserID: userID, Data: ChatEvent{Type: "conversation_ended", ConversationID: convID}})
}

// distillConversation asks the LLM router for a conversation recap and an
// updated Preferences section body, given the current per-user memory
// (plus the household's Shared notes, passed only as read-only context —
// see summarizeSystemPrompt) and one conversation's transcript. It uses the
// router directly (no tools, no TTS) since this is a background text
// distillation, not a user-facing turn.
func (o *Orchestrator) distillConversation(ctx context.Context, existingMemory, existingShared string, transcript []history.Message) (summary, preferences string, err error) {
	var b strings.Builder
	b.WriteString("Existing Preferences notes:\n")
	if existingMemory == "" {
		b.WriteString("(none yet)\n")
	} else {
		b.WriteString(existingMemory)
	}
	b.WriteString("\nExisting Shared notes:\n")
	if existingShared == "" {
		b.WriteString("(none yet)\n")
	} else {
		b.WriteString(existingShared)
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

	summary, preferences = splitSummaryPreferences(text)
	return summary, preferences, nil
}

// splitSummaryPreferences parses the "## Summary" / "## Preferences"
// sections out of the model's reply. If the model omits the Preferences
// heading entirely, the whole reply is treated as the summary and
// preferences comes back empty — a safe degradation, since an empty
// section is already a no-op for the caller. A stray "## Shared" heading —
// the model imitating the "Existing Shared notes" context it was shown, or
// an older scripted test response from before that section was removed
// (see summarizeSystemPrompt's doc comment) — is trimmed off the end of
// preferences and its content simply discarded: this pass never writes to
// shared.md, on purpose, regardless of what the model produces.
func splitSummaryPreferences(text string) (summary, preferences string) {
	text = strings.TrimSpace(text)

	var summaryPart, preferencesPart string
	if idx := strings.Index(text, preferencesMarker); idx >= 0 {
		summaryPart = text[:idx]
		preferencesPart = text[idx+len(preferencesMarker):]
		if sharedIdx := strings.Index(preferencesPart, sharedMarker); sharedIdx >= 0 {
			preferencesPart = preferencesPart[:sharedIdx]
		}
	} else {
		summaryPart = text
	}

	summaryPart = strings.TrimSpace(summaryPart)
	summaryPart = strings.TrimPrefix(summaryPart, summaryMarker)
	summaryPart = strings.TrimSpace(summaryPart)

	return summaryPart, strings.TrimSpace(preferencesPart)
}

// hasContent reports whether s is worth writing to memory: non-blank and
// not just the model's NONE sentinel (see summarizeSystemPrompt).
func hasContent(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && !isNoneSentinel(s)
}

// isNoneSentinel reports whether s is the model's explicit "nothing to add"
// signal after stripping incidental markdown wrapping (*emphasis*,
// (parens), a leading bullet dash, trailing punctuation, ...) that a model
// sometimes adds despite the plain-text instruction to reply with exactly
// NONE. This deliberately matches only that literal sentinel, not free-form
// phrases like "nothing new to report" in whatever language the model
// picks — a loose match on words like "none"/"no" could discard a real fact
// that happens to start with one.
func isNoneSentinel(s string) bool {
	s = strings.Trim(s, " \t\n*_()[]`\"'.-")
	return strings.EqualFold(s, "none")
}
