# Data encryption / keyring (`internal/keyring`)

Always on — no config toggle. An earlier version gated this behind
`keyring.enabled` (off by default), which let a user's first-ever login
predate the feature and left them stuck with no master key until a later
password login bootstrapped one. See git history for the incident.

## Key-wrapping model

One random 32-byte master key per user, wrapped independently under:
- Every registered WebAuthn passkey's PRF output.
- A password-derived Argon2id key as a fallback.

The Argon2id key is deliberately **not** the same bcrypt hash
`users.Registry.Authenticate` checks logins against — an attacker with
disk access must not be able to use the persisted login hash to also
unlock encrypted data.

The unwrapped key lives only in an in-memory `keyring.Cache` (no
persistence, no auto-lock timer — only explicit logout or process restart
clears it). This reflects the accepted threat model: disk access is what
this defends against, not a live memory-dump attacker.

## Key injection into MCP tool calls

`Orchestrator.executeTool` injects the key into a whitelisted,
`https://`-only MCP server's tool-call arguments right before dispatch,
on a local variable only — never mutating the `tc` that
`history`/`llmtrace` already recorded — so the key structurally cannot
leak into persisted history or `llm.log`.

Whitelisting is controlled by `MCPServer.EncryptionKeyPermitted`, checked
at both config-load and startup, held on `Orchestrator` rather than
`mcp.Manager` since it's static config data.

## Full design

See **`docs/adr/encryption.md`** for: wrap/unwrap sequencing (including
the per-username lock that closes a real bootstrap race between two unlock
methods), PRF ceremony details (including a real client-side
`ArrayBuffer`/`JSON.stringify` bug this had to fix), MCP whitelist/HTTPS
gating, and known limitations (no recovery-key mechanism, no
change-password hook yet).
