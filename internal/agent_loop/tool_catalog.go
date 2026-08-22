package agentloop

import (
	"context"
	"fmt"
	"sort"
	"strings"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda/internal/hub"
)

// availableTools combines every connected MCP server's tools with the
// agent's built-in tools (remember_this, etc.). The escalation tool is NOT
// added here: since each provider in the router's fallback/escalation chain
// can configure its own escalation target and tool name (see
// config.LLMProvider.Escalation), only the Router knows which one applies
// to whichever provider is active at a given hop — it appends that
// provider's own escalation ToolDef to this base list right before each
// Chat() call (see miranda-llm/router.requestFor), and intercepts calls to
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
//
// control gates which lazy MCP servers' (config.MCPServer.Lazy) real tool
// schemas are included: a lazy server not yet named in control.loadedGroups
// contributes nothing here except a one-line entry inside the shared
// load_tool_group stub (see loadToolGroupStub); once the model calls
// load_tool_group with that server's name, executeTool records it in
// control.loadedGroups and runAgentLoop calls this again to splice in that
// server's real tools for the rest of the turn. Called with a control whose
// loadedGroups is empty/nil on a turn's first iteration (from Handle) — see
// docs/adr/lazy-mcp-tool-loading.md.
func (o *Orchestrator) availableTools(ctx context.Context, userID string, control *turnControl) []llm.ToolDef {
	var tools []llm.ToolDef
	names := make(map[string]bool)
	add := func(t llm.ToolDef) {
		tools = append(tools, t)
		names[t.Name] = true
	}

	if o.memoryCfg.ExplicitTool {
		add(llm.ToolDef{
			Name: rememberToolName,
			Description: "Remember a durable fact for future conversations. " +
				"By default (scope=\"personal\") the fact is saved to the current user's private memory. " +
				"Set scope=\"shared\" to save to shared household memory visible to all users — " +
				"use this only when the fact belongs to the household, not to one person " +
				"(e.g. \"у нас живёт кот Барсик\", \"wifi пароль: ...\").",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"fact": map[string]any{"type": "string"},
					"scope": map[string]any{
						"type":        "string",
						"enum":        []string{"personal", "shared"},
						"description": "\"personal\" (default) writes to the current user's memory; \"shared\" writes to household-wide shared memory.",
					},
				},
				"required": []string{"fact"},
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
				"via the voice pipeline — use only when the user's own message explicitly asks to hear something " +
				"read aloud (e.g. \"озвучь это\", \"расскажи голосом\", \"скажи вслух\", \"read that out loud\"). Never call this " +
				"on your own initiative just because you judge the content important, urgent, or worth emphasizing " +
				"(e.g. a concerning lab result) — significance is not a request for voice. A plain acknowledgement " +
				"like \"верно\"/\"ок\" is not a voice request either. Pass the text to speak — normally " +
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

	if o.oauth != nil {
		add(llm.ToolDef{
			Name: oauthAuthorizeToolName,
			Description: "Start connecting a third-party account (e.g. Google Calendar) so its tools can act on the " +
				"current user's own data — call this when the user asks to connect/link/authorize a service, or when " +
				"a previous tool call failed because this user hasn't authorized it yet. Returns a link the user must " +
				"open and approve; the link is also proactively sent to their Telegram when known, since a spoken " +
				"reply can't usefully read a URL aloud.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider": map[string]any{
						"type":        "string",
						"enum":        o.oauth.ProviderNames(),
						"description": "which service to connect, e.g. \"google_calendar\"",
					},
				},
				"required": []string{"provider"},
			},
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

	addMCP := func(t llm.ToolDef) {
		if names[t.Name] {
			o.hub.Publish(hub.Event{Source: "error", Message: fmt.Sprintf(
				"mcp tool %q collides with a built-in tool of the same name — dropping the mcp one", t.Name)})
			return
		}
		tools = append(tools, t)
		names[t.Name] = true
	}

	// pending collects every lazy group not yet loaded this turn — MCP
	// servers marked config.MCPServer.Lazy, plus the native (non-MCP)
	// google_calendar group below — each contributing only a one-line entry
	// inside the shared load_tool_group stub, not its own real tool
	// schemas, until the model asks for it. A group absent from
	// o.lazyServerDescriptions and not google_calendar (the common case:
	// lazy loading unconfigured, or this particular server isn't lazy) is
	// unaffected and always included via ToolsExcluding.
	//
	// Every lazy MCP server's name goes into skip regardless of loaded
	// state — a loaded one is added explicitly via ToolsForServer just
	// below, so it must NOT also come back through ToolsExcluding's own
	// listing, or it would be fetched (and ListTools-RPC'd) twice, with the
	// second occurrence of each tool tripping addMCP's same-name collision
	// guard and logging a spurious "collides with a built-in tool" error.
	// google_calendar has no entry in o.tools at all (it isn't an MCP
	// server — see internal/calendar's package doc comment), so it never
	// needs to be in skip.
	var pending []string
	groupDescriptions := make(map[string]string, len(o.lazyServerDescriptions)+1)
	skip := make(map[string]bool, len(o.lazyServerDescriptions))
	for name, desc := range o.lazyServerDescriptions {
		groupDescriptions[name] = desc
		skip[name] = true
		if control.loadedGroups[name] {
			for _, t := range o.tools.ToolsForServerAndUser(ctx, name, userID) {
				addMCP(t)
			}
		} else {
			pending = append(pending, name)
		}
	}
	// google_calendar is always lazy, unconditionally, when enabled at
	// all — no config.MCPServer.Lazy-style toggle exists (or is needed) for
	// it, since there's no MCP server to mark lazy; six tool schemas
	// (including two multi-field EventDateTime objects) for a domain that's
	// relevant to a small minority of turns is exactly the case
	// docs/adr/lazy-mcp-tool-loading.md's Lazy field targets for MCP
	// servers like diary/yazio/medical_card.
	if o.calendarEnabled() {
		groupDescriptions[googleCalendarProvider] = calendarToolGroupDescription
		if control.loadedGroups[googleCalendarProvider] {
			for _, t := range calendarToolDefs() {
				addMCP(t)
			}
		} else {
			pending = append(pending, googleCalendarProvider)
		}
	}
	for _, t := range o.tools.ToolsExcludingForUser(ctx, skip, userID) {
		addMCP(t)
	}
	if len(pending) > 0 {
		addMCP(o.loadToolGroupStub(pending, groupDescriptions))
	}

	return tools
}

// loadToolGroupStub builds the single load_tool_group ToolDef standing in
// for every not-yet-loaded lazy group in pending (MCP server or native
// group — see availableTools) — one line per domain, drawn from
// descriptions, so the model can decide which (if any) is worth loading
// before it ever sees that domain's real tool schemas. See
// docs/adr/lazy-mcp-tool-loading.md §2.6.
func (o *Orchestrator) loadToolGroupStub(pending []string, descriptions map[string]string) llm.ToolDef {
	sort.Strings(pending) // deterministic order across calls — stable prompt for identical state
	var desc strings.Builder
	desc.WriteString("Load the real tools for one of these domains before calling anything in it — you currently only see this one-line summary, not their actual tool schemas:\n")
	for _, name := range pending {
		fmt.Fprintf(&desc, "- %s: %s\n", name, descriptions[name])
	}
	return llm.ToolDef{
		Name:        loadToolGroupToolName,
		Description: desc.String(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"group": map[string]any{"type": "string", "enum": pending},
			},
			"required": []string{"group"},
		},
	}
}
