package agentloop

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/schedule"
	"github.com/archer-developer/miranda/internal/users"
)

// RunScheduledTasks fires every scheduled task that's currently due. A
// schedule.KindTask row's stored free-text prompt is replayed through
// Handle exactly like a live turn — Orchestrator never interprets the
// prompt itself, the model does, using whatever tools it needs
// (speak_reply, send_telegram, etc.) at fire time. A schedule.KindReminder
// row instead goes through deliverReminder and never touches Handle/the LLM
// at all — see docs/adr/reminders-vs-scheduled-tasks.md for why: a
// reminder's Prompt is often phrased as "напомни..." by the very nature of
// what the user asked for, and replaying that back through Handle as a live
// message caused the model to reinterpret it as a fresh request and
// reschedule the same reminder forever. Meant to be called periodically by
// a background ticker (see cmd/miranda), the same way SummarizeIdleSessions
// is.
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
		var err error
		if task.Kind == schedule.KindReminder {
			err = o.deliverReminder(ctx, task, logger)
		} else {
			// DetachedTurnContext keeps this turn alive past the sweep tick
			// that triggered it, the same way it keeps a Telegram-triggered
			// turn alive past the inbound webhook connection closing — see
			// server.go.
			turnCtx, cancel := DetachedTurnContext(ctx)
			_, err = o.Handle(turnCtx, InputRequest{
				Source: users.SourceScheduled,
				UserID: task.UserID,
				Text:   task.Prompt,
			})
			cancel()
		}

		runStatus, errMsg := schedule.StatusSent, ""
		if err != nil {
			runStatus, errMsg = schedule.StatusError, err.Error()
		}
		if recordErr := o.schedule.RecordRun(ctx, task, runStatus, errMsg); recordErr != nil {
			logger.Error("scheduled task history record failed", "task_id", task.ID, "error", recordErr)
			o.hub.Publish(hub.Event{Source: "error", Message: fmt.Sprintf("record scheduled task history %s: %v", task.ID, recordErr)})
		}

		recurring := task.CronExpr != ""

		if err != nil {
			logger.Error("scheduled task fire failed", "task_id", task.ID, "user_id", task.UserID, "kind", string(task.Kind), "error", err)
			o.hub.Publish(hub.Event{Source: "error", Message: fmt.Sprintf("run scheduled task %s: %v", task.ID, err)})
			if !recurring {
				// One-off: leave the row due so the next sweep retries it.
				continue
			}
			// Recurring: fall through to the reschedule block below instead
			// of retrying every ~1-minute sweep tick forever. A deterministic
			// failure (e.g. a reminder whose Telegram-origin user has since
			// blocked the bot) would otherwise fire — and fail — once a
			// minute indefinitely instead of at its actual cron cadence.
		} else {
			logger.Info("scheduled task fired", "task_id", task.ID, "user_id", task.UserID, "kind", string(task.Kind), "recurring", recurring)
		}

		if recurring {
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

		// One-off task that fired successfully (a failed one-off already
		// `continue`d above): remove it rather than leaving a stale row for
		// DueTasks to keep matching every sweep tick.
		if err := o.schedule.DeleteFired(ctx, task.ID); err != nil {
			logger.Error("scheduled task cleanup failed", "task_id", task.ID, "error", err)
			o.hub.Publish(hub.Event{Source: "error", Message: fmt.Sprintf("delete fired task %s: %v", task.ID, err)})
		}
	}
	return nil
}

// deliverReminder is the schedule.KindReminder sibling of the Handle-based
// agentic firing above: a reminder's Prompt is never fed back through the
// LLM at all — there is no turn, no tool calls, and therefore no chance for
// the model to reinterpret e.g. "напомни полить кактус" as a fresh
// imperative request and call create_reminder/create_scheduled_task again
// (the infinite reschedule loop this whole split exists to fix — see
// docs/adr/reminders-vs-scheduled-tasks.md).
//
// Delivery is: the origin channel first — TTS if created over voice
// (OriginSource == ha_assist), a direct Telegram send if created via
// Telegram, otherwise appended straight into the user's own conversation
// history so the web UI shows it — then, always in addition, unless the
// origin channel was already Telegram (to avoid sending it twice), a
// best-effort Telegram push (skipped entirely, not just logged, when
// Telegram isn't configured at all — see the guard below). AnnounceAloud
// layers one more speak-aloud leg on top of all that when the origin wasn't
// already voice — it never overrides which channel OriginSource itself
// resolves to, so e.g. a Telegram-origin reminder with AnnounceAloud still
// reaches Telegram, it's just also spoken. Every leg is independent; only
// the origin-channel leg's failure is treated as the firing having failed
// (see the two Telegram calls below for why).
func (o *Orchestrator) deliverReminder(ctx context.Context, task schedule.Task, logger *slog.Logger) error {
	voiceOrigin := task.OriginSource == users.SourceHAAssist
	telegramOrigin := task.OriginSource == users.SourceTelegram

	// Origin-channel delivery — chosen strictly by OriginSource, never
	// overridden by AnnounceAloud (see the doc comment above): a
	// Telegram-origin reminder with AnnounceAloud still reaches Telegram
	// here, with speaking-aloud layered on top separately below, not
	// substituted for it.
	var originErr error
	switch {
	case voiceOrigin:
		// speakTextChecked only catches the device-can't-be-resolved class of
		// failure synchronously — actual synthesis/playback still happens
		// asynchronously via the Player and remains undetectable here (same
		// limitation speak_reply has) — but that's still strictly better than
		// speakText's pure fire-and-forget, which could never report this
		// firing as failed even when there was never anywhere for it to go.
		originErr = o.speakTextChecked(ctx, voiceText(task.Prompt))
	case telegramOrigin:
		originErr = o.sendTelegramReminder(ctx, task)
	default: // web UI or any other/unknown origin
		originErr = o.appendReminderToHistory(ctx, task)
	}
	if originErr != nil {
		logger.Error("reminder origin-channel delivery failed", "task_id", task.ID, "user_id", task.UserID, "origin_source", task.OriginSource, "error", originErr)
	}

	// AnnounceAloud layers an additional speak-aloud leg on top of whatever
	// the origin channel already did — only meaningful when the origin
	// channel wasn't already voice (spoken above already), and best-effort:
	// this is an add-on, not the channel the reminder was actually created
	// through, so its own failure doesn't affect originErr.
	if task.AnnounceAloud && !voiceOrigin {
		if err := o.speakTextChecked(ctx, voiceText(task.Prompt)); err != nil {
			logger.Warn("reminder announce_aloud delivery failed", "task_id", task.ID, "user_id", task.UserID, "error", err)
		}
	}

	// Always-additionally Telegram, unless origin was already Telegram —
	// best-effort and deliberately NOT folded into originErr: a user with no
	// known Telegram chat id must not turn an otherwise-successful origin
	// delivery into a StatusError firing. Skipped (not even attempted, no
	// log) when Telegram isn't configured at all: that's a static,
	// server-wide fact, not a per-firing transient condition, so warning
	// about it on every single firing of every non-Telegram-origin reminder
	// forever would just be permanent, un-actionable log noise.
	if !telegramOrigin && o.telegram != nil {
		if err := o.sendTelegramReminder(ctx, task); err != nil {
			logger.Warn("reminder telegram delivery (always-on) failed", "task_id", task.ID, "user_id", task.UserID, "error", err)
		}
	}

	return originErr
}

// sendTelegramReminder is the direct (non-tool) Telegram send both legs of
// deliverReminder use — mirrors oauth_authorize's own direct call
// (tool_dispatch.go: "push it out-of-band regardless of which channel
// actually asked"), not send_telegram's SendHTMLToUser+replyformat.Parse
// path, since a reminder's stored message is plain text, not model-composed
// markdown-lite.
func (o *Orchestrator) sendTelegramReminder(ctx context.Context, task schedule.Task) error {
	if o.telegram == nil {
		return fmt.Errorf("telegram not configured")
	}
	return o.telegram.SendToUser(ctx, task.UserID, task.Prompt)
}

// appendReminderToHistory delivers a web/unknown-origin reminder by
// appending it straight into the user's conversation history — reusing
// AppendMessage/publishChatMessage exactly as every other assistant-role
// message is recorded, so a currently-open web UI tab sees it live over the
// per-user chat stream, and it's there in the dialog log the next time they
// open it even if no tab was open at all.
func (o *Orchestrator) appendReminderToHistory(ctx context.Context, task schedule.Task) error {
	convID, _, err := o.openOrStartConversation(ctx, task.UserID, users.SourceScheduled)
	if err != nil {
		return err
	}

	msgID, err := o.history.AppendMessage(ctx, convID, "assistant", task.Prompt)
	if err != nil {
		return fmt.Errorf("append reminder message: %w", err)
	}
	o.publishChatMessage(task.UserID, convID, history.Message{ID: msgID, ConversationID: convID, Role: "assistant", Content: task.Prompt})
	return nil
}
