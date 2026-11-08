package agentloop

import (
	"context"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtrace/anomaly"
)

// turnEngine is the swappable "brain" of one Handle turn: given everything
// Handle has already prepared (resolved conversation, memory-bearing system
// prompt, prior messages, the new user message), it decides which tools to
// call — executing them through Orchestrator.executeTool — and produces the
// final reply text.
//
// Everything outside the engine is shared, deliberately: conversation
// resolution, recording the user/assistant messages, attachments, the
// end/forget/restore lifecycle, turn tracking, anomaly reporting and the
// fallback reply all live in Handle, so switching agent.orchestrator in
// config changes only how a turn reasons, never what it persists or how a
// session behaves. See docs/adr/claude-code-orchestrator.md.
type turnEngine interface {
	// runTurn returns the final reply text and the name of the provider that
	// produced it (InputResponse.ProviderUsed). It must fill t.outcome for
	// anomaly detection and record every tool call to history itself (via
	// recordAssistantToolCallMessage/recordToolCall), since only the engine
	// sees them; Handle records just the user message and the final reply.
	runTurn(ctx context.Context, t engineTurn) (finalText, providerUsed string, err error)
	// close releases whatever the engine holds (processes, listeners) at
	// shutdown.
	close()
}

// engineTurn is one turn's input to a turnEngine, prepared by Handle.
type engineTurn struct {
	userID, convID, source string
	// stableSystem/volatileSystem are buildSystemPrompt's two halves — see
	// its doc comment for why they're kept apart.
	stableSystem, volatileSystem string
	// prior is the conversation so far (resolveConversation), without the
	// system prompt and without this turn's user message.
	prior []llm.Message
	// userContent/imageParts are this turn's user message (processAttachments).
	userContent string
	imageParts  []llm.ContentPart
	control     *turnControl
	outcome     *anomaly.Outcome
}

// mirandaEngine is the default engine: Miranda's own agent loop over the
// llm.providers router (runAgentLoop).
type mirandaEngine struct{ o *Orchestrator }

func (e mirandaEngine) runTurn(ctx context.Context, t engineTurn) (string, string, error) {
	// Sent as two separate RoleSystem messages, stable first — see
	// buildSystemPrompt's doc comment and docs/adr/system-prompt-caching.md:
	// anthropic.Provider places its prompt-cache breakpoint on the FIRST
	// system message specifically so the volatile one (current time,
	// different on every turn) never defeats reuse of the stable one.
	messages := append([]llm.Message{
		{Role: llm.RoleSystem, Content: t.stableSystem},
		{Role: llm.RoleSystem, Content: t.volatileSystem},
	}, t.prior...)
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: t.userContent, Parts: t.imageParts})

	tools := e.o.availableTools(ctx, t.userID, t.control)
	return e.o.runAgentLoop(ctx, t.userID, t.convID, t.source, messages, tools, t.control, t.outcome)
}

func (mirandaEngine) close() {}

// Close releases the active engine's resources (for the claude_code
// orchestrator: every running Claude Code process and its loopback MCP
// listener). Call once at shutdown; a no-op for the default engine.
func (o *Orchestrator) Close() {
	o.engine.close()
}
