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

## Known issue: tool name resolution ambiguity

`mcp.Manager.ServerForTool`/`ServerAndTool` (and `Call`) resolve a prefixed
tool name back to its owning server by scanning configured server names with
`strings.HasPrefix` against the `"<server>_<tool>"` convention
`prefixedToolName` builds. This is ambiguous:

- Two enabled servers where one name is a `_`-delimited prefix of the other
  (e.g. `medical` and `medical_card`) can resolve to the wrong server.
- Tool names can themselves contain underscores: server `a` + tool `b_c`
  and server `a_b` + tool `c` both mint the identical prefixed name `a_b_c`.

`validateMCPServerNames` only rejects exact duplicate server names, not
this. Every consumer of this resolution — encryption-key injection,
session-id injection, file-exposing-server detection, and `Call`'s own
dispatch — inherits the misattribution risk.

**Intended fix:** stop re-deriving `(server, tool)` from the string at all.
Have `Manager.Tools()` record the exact `prefixedName → (server, tool)`
pairing at the moment it mints each prefixed name (it already knows this
unambiguously there), and have `ServerForTool`/`ServerAndTool`/`Call` look
that mapping up instead of re-parsing.
