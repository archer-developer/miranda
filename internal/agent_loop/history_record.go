package agentloop

import (
	"context"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
)

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
	// nil downloads: a mid-loop tool-calling turn never carries a file
	// reference — those are only ever flushed once, on the turn's final
	// reply (Handle, after runAgentLoop returns) — see history.Message.Downloads.
	msgID, err := o.history.AppendAssistantMessage(ctx, conversationID, text, refs, nil)
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
