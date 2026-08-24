package agentloop

import (
	"context"

	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
)

// restoreConversation is the restore_conversation tool's deferred action
// (see turnControl.restoreConversationID and Handle's switch): it closes
// oldConvID exactly like end_conversation (recap + Preferences update, if
// AutoSummarize allows it), then starts a brand new conversation for userID
// and replays sourceConvID's plain user/assistant text turns into it — no
// tool calls, no tool results, so the restored dialog reads exactly like
// what the user said and heard, not the mechanics behind it. The new
// conversation becomes userID's open conversation from this point on purely
// because it's the newest row with ended_at IS NULL — history.
// OpenConversation's normal lookup picks it up automatically on the very
// next turn, so nothing else in the orchestrator needs to know a restore
// happened.
//
// Best-effort like forget/end (see their call sites in Handle): a failure
// here is only published to the hub, never returned — the user already got
// a correct reply to the turn that triggered this.
func (o *Orchestrator) restoreConversation(ctx context.Context, oldConvID, userID, source, sourceConvID string) {
	if err := o.summarizeConversation(ctx, oldConvID, userID); err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: "restore conversation: close current conversation: " + err.Error()})
		return
	}

	newConvID, err := o.history.StartConversation(ctx, userID, source)
	if err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: "restore conversation: start new conversation: " + err.Error()})
		return
	}

	sourceMessages, err := o.history.ConversationMessages(ctx, sourceConvID)
	if err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: "restore conversation: load source conversation: " + err.Error()})
		return
	}

	for _, m := range sourceMessages {
		// Skip "tool" rows and any assistant turn whose only content was a
		// tool call (empty Content) — see this function's own doc comment.
		// A "let me check that" assistant turn that also streamed real text
		// alongside its tool call IS kept: streamOneTurn speaks/publishes
		// that text live, so it's genuinely part of what the user heard.
		if (m.Role != "user" && m.Role != "assistant") || m.Content == "" {
			continue
		}
		msgID, err := o.history.AppendMessage(ctx, newConvID, m.Role, m.Content)
		if err != nil {
			o.hub.Publish(hub.Event{Source: "error", Message: "restore conversation: copy message: " + err.Error()})
			return
		}
		// Published as ordinary "message" chat events (same as a live turn)
		// rather than a new ChatEvent.Type the web UI wouldn't know how to
		// render — see internal/webui's chat.js: "conversation_ended",
		// published by summarizeConversation just above, already cleared
		// the pane, so these append into it exactly like a resumed session.
		o.publishChatMessage(userID, newConvID, history.Message{ID: msgID, ConversationID: newConvID, Role: m.Role, Content: m.Content})
	}
}
