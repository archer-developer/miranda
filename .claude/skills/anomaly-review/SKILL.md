---
name: anomaly-review
description: Fetch and analyze flagged agent-loop turns from logs/anomalies/ on the production Miranda server (or a local run) — slow LLM calls, stuck tool retries, unknown tools, bad arguments, tool errors, hitting the iteration cap or a timeout. Use when the user asks to check/review anomalies, look at logs/anomalies, or investigate a flagged agent-loop turn.
---

# Reviewing Miranda's `logs/anomalies/`

## Background — what this is

Every agent-loop turn (`internal/agent_loop.Orchestrator.Handle`) is checked, as it ends, for a set of
**mechanical** anomalies (`miranda-llm/llmtrace/anomaly.Detect`, wired in via
`internal/agent_loop/anomaly.go`). This detection is deterministic Go code — it does **not** judge
whether something is a real bug, just flags the shape. **Your job when this skill is invoked is the
judgment call the detector can't make**: read the flagged turn's actual trace and decide whether it's a
real problem worth fixing, a benign edge case, or infrastructure noise (a provider rate-limit blip).

Unlike medical-card's equivalent feature, this runs **unconditionally** — Miranda's `logs/llm.log` is
always on (no debug-only gate), so anomaly detection is always active in production.

### The 7 anomaly kinds (`llmtrace/anomaly` package constants)

| Kind | What it means |
|---|---|
| `slow_call` | One LLM call in the turn took longer than the configured threshold (default 20s) — includes real model latency AND any tool-execution time between iterations (it's a wall-clock gap, not pure LLM time — check surrounding calls/the app log around that timestamp before assuming the model itself was slow; a provider retry/rate-limit backoff shows up here too). |
| `repeated_tool_call` | The model called the same tool with the *same* arguments more than once in one turn — usually a sign it's stuck, not making progress on what the result told it. |
| `unknown_tool` | The model called a tool name that isn't resolvable to any MCP server (`internal/mcp/mcp.go`'s `resolveTool` miss, surfaced as `"mcp: no configured server matches tool ..."`) — either a hallucinated name, or a lazy MCP group (`docs/adr/lazy-mcp-tool-loading.md`) that was never loaded via `load_tool_group`. |
| `invalid_arguments` | A built-in tool's arguments failed to decode (`internal/agent_loop/tool_dispatch.go`'s `"error: invalid arguments: ..."` convention) — MCP tool arguments aren't locally validated the same way, so this kind mostly shows up for Miranda's own built-in tools, not MCP-routed ones. |
| `tool_error` | A tool call reached its target (built-in or MCP) but execution itself failed for some other reason. |
| `iteration_cap` | The loop hit `maxToolIterations` (15 — `internal/agent_loop/orchestrator.go`) without a final reply — a real, hard stop. |
| `timeout` | The turn's context deadline was exceeded (`errors.Is(err, context.DeadlineExceeded)`) — Miranda also has an outer 5-minute per-turn timeout (`TurnTimeout`, `internal/agent_loop/turn_context.go`) independent of this check. |

### Where files live and what's in one

- **Server**: `archer@miranda:~/miranda/logs/anomalies/` (SSH already configured — see memory
  `reference_miranda_server_ssh.md`).
- **Local dev run**: `logs/anomalies/` under the repo root — but see the note in `CLAUDE.md`/this
  session's own history about **not** starting Miranda's dev server locally unless the user has
  explicitly asked for it, since it connects to live MCP servers/Telegram/HA integrations; prefer working
  from server-fetched files or the test suite over a live local run for this feature specifically.
- **Filename**: `<UTC timestamp>_<kind1-kind2-...>.log` — e.g. `20260821T124009Z_slow_call.log`. The
  kind(s) in the filename alone often tell you whether it's worth opening at all.
- **Content**: a `#`-prefixed header (one line per anomaly found, with a human-readable detail) followed
  by the turn's trace blocks in the **exact same format** `logs/llm.log` itself uses — opens directly
  with the existing `miranda llm-trace` tool, no special-casing needed. The file holds the **whole
  conversation up to that point** (re-read from `logs/llm.log` at the moment the anomaly fired), falling
  back to just the flagged turn's own blocks if that conversation wasn't found there — e.g. it had
  already rotated out of the current `llm.log` file (size-rotated via `lumberjack`, unlike medical-card's
  plain append-only log) by the time the anomaly fired.

## Workflow

1. **List what's there** (server):
   ```bash
   ssh archer@miranda 'ls -la ~/miranda/logs/anomalies/'
   ```
   The filenames alone (timestamp + kind(s)) are often enough to triage what's worth a closer look, or to
   notice a pattern (e.g. a burst of `slow_call` around the same time — a provider rate-limit incident,
   not a prompt bug).

2. **Fetch the ones you need** — either read in place over SSH (works well for a handful of files):
   ```bash
   ssh archer@miranda 'cat ~/miranda/logs/anomalies/<file>'
   ```
   or copy a batch down for local analysis via the actual CLI tool (recommended once you're looking at
   more than one or two):
   ```bash
   scp archer@miranda:~/miranda/logs/anomalies/*.log /tmp/anomalies/
   ```

3. **Render each one as a readable turn-by-turn table** with the existing CLI, exactly like debugging a
   normal `llm.log` conversation:
   ```bash
   go run ./cmd/miranda llm-trace -file /tmp/anomalies/<file>.log -untagged
   # or, if the header/blocks show a conversation= tag:
   go run ./cmd/miranda llm-trace -file /tmp/anomalies/<file>.log -latest
   ```
   This gives you: what tool got called with what arguments each iteration, what came back, and where it
   ended — the same view the header's anomaly summary is pointing you at.

4. **Diagnose, don't just report the shape**:
   - **Missing capability**: no tool/parameter exists for what the model was trying to do, or a needed
     MCP server's tools were never loaded (see `load_tool_group`/lazy MCP servers above).
   - **Bad data**: an ambiguous/unhelpful tool result the model reasonably kept retrying against.
   - **A genuine bug** in tool dispatch, argument handling, or escalation/provider-fallback behavior.
   - For `slow_call` specifically: check `logs/miranda.log`/the journal around the same timestamp for a
     provider rate-limit/retry warning — that's infrastructure noise, not a code bug, and isn't worth
     chasing as one.
   - For `repeated_tool_call`/`iteration_cap`: read the full turn — is the tool result actually
     unhelpful/ambiguous, or is there a real gap in what the model can ask for?

5. **Report findings back to the user** — which files you looked at, what kind each genuinely was (a real
   bug vs. a benign transient blip vs. expected behavior), and for anything that looks like a real bug, a
   proposed fix, following this repo's normal fix → verify (tests) → deploy (`./scripts/deploy.sh`, ask
   first) → re-verify loop, rather than just re-describing the anomaly.

## What NOT to do

- Don't propose raising `maxToolIterations`/the `slow_call` threshold/`TurnTimeout` as a fix for a real
  bug — find and fix the actual root cause instead, mirroring medical-card's own documented stance on
  this exact temptation.
- Don't treat detection itself as verdict — a flagged file means "worth a look," not "confirmed bug."
  Plenty of `slow_call`s are just a provider having a slow moment.
- This is a **read-only, manual, on-demand** investigation — don't set up any automation, cron job, or
  recurring check as part of running this skill; that was explicitly ruled out when this feature was
  designed (detection is always-on and mechanical; the *analysis* stays human-initiated, every time).
- Don't delete anomaly files after reviewing them unless the user explicitly asks.
- Don't start Miranda's own dev server locally as part of this investigation unless the user explicitly
  asks for that — it touches live Telegram/HA integrations; work from fetched files and the test suite
  instead (see the note above).
