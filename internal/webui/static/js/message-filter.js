// Shared "is this a bubble/line a person should see" rule, used by every
// screen that renders a history.Message list (chat.js's live thread,
// history.js's read-only conversation browser) — both consume the same
// GET /api/dialogs/{id} shape (or the chat-ws.js stream of the identical
// history.Message struct, see internal/httpapi.Orchestrator publishing it
// on both paths), so a message either counts as a shown turn or it doesn't,
// regardless of which screen is asking.

/** Tool-call/tool-result turns (see internal/httpapi's
 * recordAssistantToolCallMessage/recordToolCall) are recorded and, in
 * chat.js's case, streamed over chat-ws.js too (for a future debug view),
 * but never rendered as a chat bubble or history line. A message with no
 * text but a non-empty downloads list still counts — the
 * turn-errored-after-a-successful-download recovery path in
 * orchestrator.Handle records exactly that (empty content, downloads set),
 * and it still needs its chip shown. */
export function isChatBubble(m) {
  if (m.role !== "user" && m.role !== "assistant") return false;
  return Boolean(m.content?.trim()) || (m.downloads?.length ?? 0) > 0;
}
