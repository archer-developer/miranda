# LLM providers, escalation, and web tools (`internal/config`)

The LLM plumbing itself (provider adapters, router, key rotation, tracing)
lives in the shared [`miranda-llm`](https://github.com/archer-developer/miranda-llm)
module, not in this repo — see that module's own CLAUDE.md for its
package-by-package details. This file documents the layer that's still
Miranda's own: `internal/config`'s YAML-tagged types, and how
`cmd/miranda/main.go` wires their values into `miranda-llm`'s constructors
(each provider/router config struct there has an identical-shape
counterpart here — e.g. `config.AnthropicToolsConfig` ↔
`anthropic.ToolsConfig` — converted via a plain Go struct conversion at the
call site, not a field-by-field mapping function).

## Provider types

`config.LLMConfig.Providers` is an ordered fallback chain
(`router.Router`, from `miranda-llm/router`, tries each in list order on a
connection failure):

- `type: openai_compat` — any OpenAI Chat Completions-compatible backend.
- `type: anthropic` — native Claude (`miranda-llm/anthropic`).
- `type: gemini` — native Gemini (`miranda-llm/gemini`,
  `google.golang.org/genai`).

`LLMConfig.DefaultProvider`, if set, is moved to the front of the fallback
order by `router.New` regardless of `Providers`' list position (this field
existed before but had no effect until it was wired up — don't assume list
order alone determines the default).

## Escalation

Each provider entry carries its own `Escalation EscalationConfig`
(`enabled`, `tool_name`, `target_provider`, optional `description`) — not
a global setting. This lets a chain escalate in hops: cheap default →
stronger model → Claude for genuinely hard turns, rather than everything
skipping straight to the most expensive model. See `config/llm.yaml` for a
worked 3-tier example.

`description` overrides the escalation tool's default "too
complex/ambiguous/high-stakes" wording (`router.defaultEscalationDescription`).
Worth setting when a provider has a concrete, known-missing capability
(e.g. `config/llm.yaml`'s Gemini tiers name code execution explicitly,
since neither has a working code-execution tool on the free-tier Gemini
Developer API) — a generic "too hard" prompt doesn't reliably make the
model connect "I can't run code" to "escalate."

`Router` builds and injects the *active* provider's own escalation
`ToolDef` right before each `Chat()` call — not
`Orchestrator.availableTools`, which only builds the shared base list. Each
hop in the chain sees only its own escalation tool. `Router.Chat` walks the
chain to any depth; a small hop cap catches a misconfigured cycle (two
providers escalating to each other), not a legitimate deep ladder.

## Gemini key rotation

`type: gemini` rotates across every resolved `api_key_envs` key on a quota
error (HTTP 429/`RESOURCE_EXHAUSTED`) **or a 5xx server error**. This is
broader than `gemini_tts`'s rotation (quota-only — see
`internal/tts/gemini.go`), because a conversational turn can't afford to
drop the whole turn on a transient upstream failure. Don't "fix" this back
to match TTS's narrower behavior.

`anthropic`/`openai_compat` providers accept `api_key_envs` for config
consistency but only ever use the first entry — those SDKs take a single
credential per client.

## Web tools (`internal/tools`, `internal/tavily`)

`web_search`/`web_fetch` are Miranda's own tools for live web access,
backed by the [Tavily](https://tavily.com) API (`internal/tavily` — a
minimal client for `/search` and `/extract`). They run through the
Orchestrator's ordinary custom-tool path (`Orchestrator.SetWebTools`,
`availableTools`, `executeTool`) — offered to every LLM provider
identically, not tied to one backend's native tool.

`Orchestrator.webTools` is a **slice, not a map** — list order must stay
identical turn to turn because `anthropic.Provider.buildTools` places its
prompt-cache breakpoint on the *last* tool in the list, and a map's
randomized iteration order would silently defeat that cache on every call.

**Why Tavily replaced `gemini_tools.google_search`:** Grounding with Google
Search has a zero quota on the free-tier Gemini Developer API (not merely
low — every request fails with `RESOURCE_EXHAUSTED`, including the first
call of the day on a fresh key). Gemini key rotation can't route around it
since the exhaustion is per-feature, not per-key.

**Name-collision risk:** Anthropic requires unique tool names on one
request, so enabling a provider's native `web_search`/`web_fetch` *and* a
same-named custom tool together would be a real conflict — except Tavily's
own tools are deliberately named `tavily_web_search`/`tavily_web_fetch`
(`internal/tools/websearch.go`'s `WebSearchToolName`,
`internal/tools/webfetch.go`'s `WebFetchToolName`), not `web_search`/
`web_fetch`, specifically so they never collide with Anthropic's native
ones — enabling `anthropic_tools.web_search` alongside
`tavily.web_search.enabled` is fully supported today (see
`TestLoad_AnthropicNativeToolsAlongsideTavilyIsAllowed` in
`internal/config/config_test.go`). There is no config-load-time rejection
of this combination — the rename is the whole fix.

The residual risk this doesn't cover is an MCP server whose *own* prefixed
tool name happens to match a built-in — that name isn't known until the
server connects, so it can't be caught at config-load time. Two safety
nets:

1. `httpapi.ReservedToolNames()` lists every name the agent loop can ever
   advertise; `cmd/miranda.validateEscalationToolNames` checks every
   provider's `escalation.tool_name` against it at startup.
2. `availableTools` de-duplicates at runtime: built-in names are collected
   first, and any MCP `ToolDef` sharing one is dropped (logged via the hub)
   rather than being sent to the provider twice or shadowing the built-in.

#1 only catches an escalation tool name misconfigured to collide; #2 is the
runtime backstop for everything else, including an MCP server discovered
only after connecting.
