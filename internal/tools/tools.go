// Package tools holds Miranda's own model-callable tools that don't need an
// MCP server or any Orchestrator-internal state (history, memory, TTS,
// Telegram) — currently web_search and web_fetch (internal/tavily). Each
// one only needs its own Tool.Def() advertised alongside every other
// provider's tool list and its own Tool.Call() invoked when the model
// requests it, so the same value works unmodified from any agent loop or
// against any llm.Provider, not just internal/httpapi.Orchestrator's — the
// Orchestrator (see availableTools/executeTool in
// internal/httpapi/agent_loop.go) is just the one caller that currently
// exists, not a dependency this package takes on.
package tools

import (
	"context"

	"github.com/archer-developer/miranda/internal/llm"
)

// Tool is one model-callable tool: Def is what's advertised to the LLM
// alongside MCP/other built-in tools, Call is what runs when the model
// actually invokes it by name. Call's (string, error) split mirrors
// mcp.Manager.Call's — the caller (executeTool) is what turns a non-nil
// error into the "error: ..." string the model sees, so a Tool doesn't
// need to know that formatting convention itself.
//
// Call's signature is deliberately narrow — ctx and the raw arguments JSON,
// nothing else — which is exactly what keeps a Tool reusable outside
// Orchestrator (see the package doc comment). That's also this interface's
// explicit scope boundary: it only fits a tool whose behavior depends on
// nothing but its own arguments. internal/httpapi's other built-in tools
// (remember_this needs a userID to pick which user's memory.md to write;
// end_conversation/forget_conversation need to set flags on that turn's
// turnControl; speak_reply/stop_speech/send_telegram need o.tts/o.telegram)
// each need state Call's signature has no way to express, which is why they
// stay as Orchestrator.executeTool's own hardcoded branches instead of
// implementing Tool — not an oversight to "eventually" migrate, but this
// interface's actual limit. Widening Call to accommodate them would
// reintroduce the Orchestrator coupling this package is built to avoid.
type Tool interface {
	Def() llm.ToolDef
	Call(ctx context.Context, argumentsJSON string) (string, error)
}
