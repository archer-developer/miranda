# Scheduled tasks (`internal/schedule`)

Optional (`config.ScheduleConfig.Enabled`, default **true** — unlike
Telegram/WebAuthn this needs no deployment secret/URL, so it's opt-out,
not opt-in).

## Tools

Three tools back onto their own SQLite file (`Storage.ScheduleSQLitePath`),
wired via `Orchestrator.SetSchedule` (nil = tools never offered):

- `create_scheduled_task` — validates `run_at`/`schedule` (exactly one
  required, cron syntax checked, no past `run_at`) at tool-call time.
- `list_scheduled_tasks` — scoped to the calling `userID`.
- `delete_scheduled_task` — scoped same way; returns `ErrNotFound` whether
  an id doesn't exist or belongs to someone else.

## Task model

`schedule.Task` stores a `UserID`, a free-text `Prompt`, and exactly one
of:
- `RunAt` (`time.Time`) — one-off.
- `CronExpr` (5-field `robfig/cron/v3` standard expression) — recurring.

`internal/schedule` itself never imports `robfig/cron` or interprets
prompts — callers compute `NextRunAt` and pass it in; the store only
persists it.

## Firing

A ticker in `cmd/miranda` (`sweepScheduledTasks`, modeled on
`sweepIdleSessions`) calls `Orchestrator.RunScheduledTasks` once a minute.
For each due task it builds
`InputRequest{Source: users.SourceScheduled, UserID: task.UserID, Text: task.Prompt}`
and calls `Handle` inside a `detachedTurnContext` (same helper Telegram's
webhook uses) so the turn survives past the sweep tick.

The scheduler never interprets the prompt — at fire time the model decides
what tools to call (`speak_reply`, `send_telegram`, HA MCP tools, …),
exactly like a live turn. A scheduled turn is silent by default (same as
Telegram/web UI sources) — it must call `speak_reply`/`send_telegram`/etc.
explicitly for any output.

After firing:
- Recurring task (`CronExpr` set): rescheduled via
  `cron.ParseStandard(...).Next(time.Now())`.
- One-off task (`RunAt` set): deleted outright.

## Audit log

Every firing — success or failure, one-off or recurring — is recorded as a
`schedule.TaskRun` row (`Store.RecordRun`, table `scheduled_task_history`)
with `Status` `StatusSent` or `StatusError`. This keeps firings auditable
even though one-off rows are deleted and recurring rows are overwritten in
place on every reschedule.

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
