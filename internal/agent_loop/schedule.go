package agentloop

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/schedule"
	"github.com/archer-developer/miranda/internal/users"
)

// RunScheduledTasks fires every scheduled task that's currently due: each
// one's stored free-text prompt is replayed through Handle exactly like a
// live turn — Orchestrator never interprets the prompt itself, the model
// does, using whatever tools it needs (speak_reply, send_telegram, etc.) at
// fire time. Meant to be called periodically by a background ticker (see
// cmd/miranda), the same way SummarizeIdleSessions is.
//
// Every firing (success or failure) is logged via logger — Orchestrator's
// other operational errors only go to o.hub (which nothing in the web UI
// currently subscribes to; see internal/webui/static/js/screens/logs.js's
// two-tab app_log/llm_log split), so without this a scheduled task silently
// firing or silently failing would leave no trace in logs/miranda.log at
// all. logger may be nil (falls back to slog.Default()), matching
// cmd/miranda's other Open/New helpers.
//
// Every firing also gets a schedule.TaskRun row via RecordRun — status
// StatusSent or StatusError — before either branch below runs, so a
// recurring task's history survives across every occurrence and a one-off
// task's single firing is preserved even though DeleteFired removes its
// scheduled_tasks row right after.
func (o *Orchestrator) RunScheduledTasks(ctx context.Context, logger *slog.Logger) error {
	if o.schedule == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	due, err := o.schedule.DueTasks(ctx, time.Now())
	if err != nil {
		return fmt.Errorf("orchestrator: list due scheduled tasks: %w", err)
	}

	for _, task := range due {
		// DetachedTurnContext keeps this turn alive past the sweep tick that
		// triggered it, the same way it keeps a Telegram-triggered turn alive
		// past the inbound webhook connection closing — see server.go.
		turnCtx, cancel := DetachedTurnContext(ctx)
		_, err := o.Handle(turnCtx, InputRequest{
			Source: users.SourceScheduled,
			UserID: task.UserID,
			Text:   task.Prompt,
		})
		cancel()

		runStatus, errMsg := schedule.StatusSent, ""
		if err != nil {
			runStatus, errMsg = schedule.StatusError, err.Error()
		}
		if recordErr := o.schedule.RecordRun(ctx, task, runStatus, errMsg); recordErr != nil {
			logger.Error("scheduled task history record failed", "task_id", task.ID, "error", recordErr)
			o.hub.Publish(hub.Event{Source: "error", Message: fmt.Sprintf("record scheduled task history %s: %v", task.ID, recordErr)})
		}

		if err != nil {
			logger.Error("scheduled task fire failed", "task_id", task.ID, "user_id", task.UserID, "error", err)
			o.hub.Publish(hub.Event{Source: "error", Message: fmt.Sprintf("run scheduled task %s: %v", task.ID, err)})
			continue
		}
		logger.Info("scheduled task fired", "task_id", task.ID, "user_id", task.UserID, "recurring", task.CronExpr != "")

		if task.CronExpr != "" {
			sched, parseErr := cron.ParseStandard(task.CronExpr)
			if parseErr != nil {
				logger.Error("scheduled task reschedule failed: invalid cron_expr", "task_id", task.ID, "cron_expr", task.CronExpr, "error", parseErr)
				o.hub.Publish(hub.Event{Source: "error", Message: fmt.Sprintf("reschedule task %s: invalid cron_expr %q: %v", task.ID, task.CronExpr, parseErr)})
				continue
			}
			// Use the task owner's timezone when computing the next run so
			// that "1 9 * * *" fires at 09:01 in the user's local time on
			// every reschedule, not just the first one.
			if specSched, ok := sched.(*cron.SpecSchedule); ok {
				specSched.Location = o.userLocation(task.UserID)
			}
			nextRunAt := sched.Next(time.Now())
			if err := o.schedule.Reschedule(ctx, task.ID, nextRunAt); err != nil {
				logger.Error("scheduled task reschedule failed", "task_id", task.ID, "error", err)
				o.hub.Publish(hub.Event{Source: "error", Message: fmt.Sprintf("reschedule task %s: %v", task.ID, err)})
				continue
			}
			logger.Info("scheduled task rescheduled", "task_id", task.ID, "next_run_at", nextRunAt)
			continue
		}

		// One-off task: it only ever fires once, so remove it rather than
		// leaving a stale row for DueTasks to keep matching every sweep tick.
		if err := o.schedule.DeleteFired(ctx, task.ID); err != nil {
			logger.Error("scheduled task cleanup failed", "task_id", task.ID, "error", err)
			o.hub.Publish(hub.Event{Source: "error", Message: fmt.Sprintf("delete fired task %s: %v", task.ID, err)})
		}
	}
	return nil
}
