# Scheduled tasks (`internal/schedule`)

Optional (`config.ScheduleConfig.Enabled`, default **true** — unlike
Telegram/WebAuthn this needs no deployment secret/URL, so it's opt-out,
not opt-in).

## Reminders vs. scheduled tasks (`Kind`)

Every `schedule.Task` is one of two kinds — see
`docs/adr/reminders-vs-scheduled-tasks.md` for the full rationale:

- **`schedule.KindTask`** (default; every row created before this field
  existed reads back as this) — a free-text agentic instruction. Fired by
  replaying `Prompt` through `Orchestrator.Handle` exactly like a live
  message, same as always: the model decides what tools to call at fire
  time. Use for cases that genuinely need a live decision (`create_scheduled_task`).
- **`schedule.KindReminder`** — a passive notification. `Prompt` holds the
  plain reminder content, never phrased as an instruction to the model.
  Fired by `deliverReminder` (`internal/agent_loop/schedule.go`), which
  **never calls `Handle`** — no LLM turn happens, so the model can never
  reinterpret the fired text as a fresh request and reschedule itself. This
  exists specifically because a `KindTask`-style reminder phrased as
  "напомни X" and replayed as a live user message caused exactly that loop.

`OriginSource` (the `InputRequest.Source` the reminder was created from)
and `AnnounceAloud` (an explicit "speak this aloud even from a non-voice
origin" flag) are only meaningful for `KindReminder` and drive its delivery
channel (see "Firing" below).

## Tools

Four tools back onto their own SQLite file (`Storage.ScheduleSQLitePath`),
wired via `Orchestrator.SetSchedule` (nil = tools never offered):

- `create_scheduled_task` — creates a `KindTask` row. Validates
  `run_at`/`schedule` (exactly one required, cron syntax checked, no past
  `run_at`) at tool-call time. Its tool description steers the model away
  from plain "remind me" requests toward `create_reminder`.
- `create_reminder` — creates a `KindReminder` row, capturing the calling
  turn's `Source` as `OriginSource`. Same `run_at`/`schedule` validation
  (shared via `Orchestrator.resolveTaskTiming`), plus an `announce_aloud`
  flag.
- `list_scheduled_tasks` — scoped to the calling `userID`, shows both kinds
  together tagged by `kind`.
- `delete_scheduled_task` — scoped same way, kind-agnostic; returns
  `ErrNotFound` whether an id doesn't exist or belongs to someone else.

## Task model

`schedule.Task` stores a `UserID`, a free-text `Prompt`, a `Kind`
(`OriginSource`/`AnnounceAloud` alongside it for `KindReminder`), and
exactly one of:
- `RunAt` (`time.Time`) — one-off.
- `CronExpr` (5-field `robfig/cron/v3` standard expression) — recurring.

`internal/schedule` itself never imports `robfig/cron` or interprets
prompts — callers compute `NextRunAt` and pass it in; the store only
persists it. It doesn't interpret `Kind` either — `RunScheduledTasks` is
the one place that branches on it.

## Firing

A ticker in `cmd/miranda` (`sweepScheduledTasks`, modeled on
`sweepIdleSessions`) calls `Orchestrator.RunScheduledTasks` once a minute.
For each due task, behavior branches on `Kind`:

- **`KindTask`**: builds
  `InputRequest{Source: users.SourceScheduled, UserID: task.UserID, Text: task.Prompt}`
  and calls `Handle` inside a `DetachedTurnContext` (same helper Telegram's
  webhook uses) so the turn survives past the sweep tick. The scheduler
  never interprets the prompt — at fire time the model decides what tools
  to call (`speak_reply`, `send_telegram`, HA MCP tools, …), exactly like a
  live turn. A scheduled turn is silent by default (same as Telegram/web UI
  sources) — it must call `speak_reply`/`send_telegram`/etc. explicitly for
  any output.
- **`KindReminder`**: calls `deliverReminder`, which never touches `Handle`.
  It speaks `Prompt` aloud via TTS if `OriginSource == users.SourceHAAssist`
  or `AnnounceAloud` is set; otherwise delivers via `OriginSource`'s own
  channel (Telegram send, or an appended history message for web/unknown
  origins); then, unless the origin was already Telegram, always
  additionally attempts a best-effort Telegram send. Only the
  origin-channel leg's failure counts as the firing failing.

After firing (either kind):
- Recurring task (`CronExpr` set): rescheduled via
  `cron.ParseStandard(...).Next(time.Now())` — **regardless of whether the
  firing succeeded or failed**. A deterministic failure (e.g. a
  Telegram-origin reminder whose user has since blocked the bot) still
  advances to the next occurrence rather than being retried every
  ~1-minute sweep tick forever, which would otherwise both spam
  `scheduled_task_history` and fire far more often than the task's own
  cadence says it should.
- One-off task (`RunAt` set): deleted outright on success; left in place
  (still due) on failure, so the next sweep retries it.

## Audit log

Every firing — success or failure, one-off or recurring, either kind — is
recorded as a `schedule.TaskRun` row (`Store.RecordRun`, table
`scheduled_task_history`, its own `Kind` column) with `Status` `StatusSent`
or `StatusError`. This keeps firings auditable even though one-off rows are
deleted and recurring rows are overwritten in place on every reschedule.

There is no tool exposing this history to the model (`Store.HistoryForUser`
queries the DB directly) — deliberately kept out of the agent loop for now.

## Logging

`RunScheduledTasks` logs every firing (fired, rescheduled, failed, with
`task_id`/`user_id`) via `*slog.Logger` rather than `o.hub` — a logger
call reaches `logs/miranda.log`, stdout, *and* the `app_log` tab (via
`eventHub.Writer("app_log")`), which is the only durable trace that the
scheduler actually ran, separate from the fired task's own conversation
content (captured in `logs/llm.log` via the normal `Handle`/`llmtrace`
path).
