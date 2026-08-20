# MCP tool manager (`internal/mcp`)

## MCPServerExtension: per-server opt-ins

Three per-server behaviors are config-driven opt-ins of the same underlying
shape, so `httpapi.Orchestrator` bundles all three into one
`MCPServerExtension` per server name (`SetMCPServerExtensions`) rather than
three independent maps/setters. `executeTool` resolves a tool call's owning
server once (`mcp.Manager.ServerAndTool`) and reuses that lookup for
whichever of the three behaviors apply:

1. **Encryption-key injection** (`MCPServer.EncryptionKeyPermitted`): a
   real `encryption_key` argument is injected server-side right before
   dispatch, on a local variable only — never mutating the `tc` that
   `history`/`llmtrace` already recorded. Server-wide grant. See
   `internal/keyring/CLAUDE.md` and `docs/adr/encryption.md`.

2. **Session-id injection** (`MCPServer.SessionIDTools`): Miranda's
   resolved conversation id is injected under `SessionIDArg()`,
   unconditionally overwriting any value the model supplied, since the model
   must never be trusted to invent this id. Per-tool scope (not server-wide)
   — most tools on a server don't declare a session-id parameter at all.
   See `docs/adr/medical-card-session-injection.md`.

3. **File-download proxying** (`MCPServer.ExposeFiles`): `executeTool`
   scans tool call results from opted-in servers for URLs matching their
   `FilesEndpoint()`, stages an `attachments.Record`, and replaces the raw
   URL with a `GET /api/files/{id}` chip in the UI. See
   `internal/attachments/CLAUDE.md` and `docs/adr/file-staging-refactor.md`.

## Tool name resolution: aliases map, with a legacy fallback

`Manager.listTools` mints each provider-facing tool name via
`sanitizeToolName(prefixedToolName(server, tool))` and records the exact
`sanitizedName → (server, tool)` pairing in `Manager.aliases` at that same
moment. `ServerForTool`/`ServerAndTool`/`Call` all resolve through
`resolveTool`, which checks `aliases` first — this is what lets
`CallForUser` hand a server's `CallTool` the tool's real, unsanitized name
even when the provider-facing name had to be rewritten (see next section).

`resolveTool` falls back to the old `strings.HasPrefix` scan
(`serverAndTool`) only for a name that was never minted by `listTools` in
this process (in practice: tests constructing a `Manager` and calling
`Call`/`ServerAndTool` directly, without a prior `Tools()`). That fallback
is still ambiguous in the ways it always was:

- Two enabled servers where one name is a `_`-delimited prefix of the other
  (e.g. `medical` and `medical_card`) can resolve to the wrong server.
- Tool names can themselves contain underscores: server `a` + tool `b_c`
  and server `a_b` + tool `c` both mint the identical prefixed name `a_b_c`.

`validateMCPServerNames` only rejects exact duplicate server names, not
this. In production this is moot for any name a model could plausibly call
— it can only call a name Miranda already showed it via `Tools()`, which
means `aliases` already has the unambiguous answer by the time a real tool
call needs resolving.

## Tool name sanitization for stricter providers

Some MCP servers use characters in their own (unprefixed) tool names that
not every provider's tool-name grammar accepts — e.g. miranda-medical-card
uses a `domain.action` convention (`medical.ask`, `medical.planned_actions`,
...). Gemini- and OpenAI-shaped APIs accept the dot; Anthropic's Messages
API rejects it outright (`tools.N.custom.name: String should match pattern
'^[a-zA-Z0-9_-]{1,128}$'`), which only ever surfaced once a turn escalated
to Claude — see the incident behind this fix, 2026-08-20: Gemini was
returning 503s, escalation to Claude then 400'd on
`medical_card_medical.ask`.

`sanitizeToolName` replaces every character outside that pattern with `_`
at the one place `listTools` mints each prefixed name, so the name
Miranda advertises is identical (and valid) across every provider,
regardless of which one is actually handling a given turn. The `aliases`
map (see above) is what makes this reversible: `resolveTool` maps the
sanitized name back to the tool's real, dotted name before calling
`Client.CallTool`, so the MCP server itself never sees the sanitized form.
