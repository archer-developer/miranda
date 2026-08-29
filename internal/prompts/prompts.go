// Package prompts holds Miranda's long, freeform LLM prompts as embedded
// markdown files rather than Go string literals, so editing prompt wording
// doesn't require touching the Go files that wire them up (internal/config
// for the default persona prompt, internal/agent_loop for the memory
// summarization prompt) and diffs to prompt text stay separate from diffs to
// code. Short, schema-adjacent tool descriptions (internal/agent_loop's
// tool_catalog.go, calendar_tools.go, internal/tools' web_search/web_fetch)
// deliberately stay as Go string literals next to the JSON-schema structs
// they describe — this package is only for the few standalone documents.
package prompts

import (
	"embed"
	"strings"
)

//go:embed system_prompt.md summarize.md
var files embed.FS

// SystemPrompt is the default value of config.AgentConfig.SystemPrompt —
// Miranda's persona and behavior instructions, injected as the stable part
// of the system prompt on every turn (see internal/agent_loop/session.go's
// buildSystemPrompt). A deployment overrides it via config.yaml's
// agent.system_prompt, same as before this moved out of config.go.
var SystemPrompt = mustLoad("system_prompt.md")

// Summarize is the system prompt used by the idle-session/end_conversation
// summarization pass to distill a conversation's recap and durable memory
// (see internal/agent_loop/summarize.go).
var Summarize = mustLoad("summarize.md")

func mustLoad(name string) string {
	data, err := files.ReadFile(name)
	if err != nil {
		// Embedded at build time; a missing file is a packaging bug, not a
		// runtime condition to handle gracefully.
		panic("prompts: missing embedded file " + name + ": " + err.Error())
	}
	return strings.TrimSpace(string(data))
}
