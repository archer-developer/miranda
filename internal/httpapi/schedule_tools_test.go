package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda/internal/schedule"
)

// newTestOrchestratorWithSchedule is like newTestOrchestrator but also wires
// a real schedule.Store in via SetSchedule, so create/list/delete-scheduled-
// task tests can exercise the actual store instead of a fake.
func newTestOrchestratorWithSchedule(t *testing.T, provider *llmtest.FakeProvider) (*Orchestrator, *schedule.Store) {
	t.Helper()

	o, _, _ := newTestOrchestrator(t, provider)

	s, err := schedule.Open(filepath.Join(t.TempDir(), "schedule.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	o.SetSchedule(s)
	return o, s
}

func TestOrchestrator_CreateScheduledTaskTool_OneOff(t *testing.T) {
	runAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "create_scheduled_task",
			Arguments: `{"task":"напомни выпить пива","run_at":"` + runAt + `"}`}},
		llmtest.Response{Text: "Хорошо, напомню."},
	)
	o, s := newTestOrchestratorWithSchedule(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "сегодня в 22:00 напомни мне выпить пива"})
	require.NoError(t, err)
	require.Equal(t, "Хорошо, напомню.", resp.Reply)

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, "напомни выпить пива", tasks[0].Prompt)
	require.NotNil(t, tasks[0].RunAt)
	require.Empty(t, tasks[0].CronExpr)
}

func TestOrchestrator_CreateScheduledTaskTool_Recurring(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "create_scheduled_task",
			Arguments: `{"task":"доброе утро","schedule":"1 9 * * *"}`}},
		llmtest.Response{Text: "Хорошо."},
	)
	o, s := newTestOrchestratorWithSchedule(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "каждое утро в 9:01 желай доброго утра"})
	require.NoError(t, err)
	require.Equal(t, "Хорошо.", resp.Reply)

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, "1 9 * * *", tasks[0].CronExpr)
	require.Nil(t, tasks[0].RunAt)
	require.True(t, tasks[0].NextRunAt.After(time.Now()))
}

func TestOrchestrator_CreateScheduledTaskTool_RejectsBothRunAtAndSchedule(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "create_scheduled_task",
			Arguments: `{"task":"x","run_at":"2099-01-01T00:00:00Z","schedule":"1 9 * * *"}`}},
		llmtest.Response{Text: "ok"},
	)
	o, s := newTestOrchestratorWithSchedule(t, provider)

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "x"})
	require.NoError(t, err)

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Empty(t, tasks)

	last := provider.Requests[len(provider.Requests)-1].Messages
	require.Contains(t, last[len(last)-1].Content, "error")
}

func TestOrchestrator_CreateScheduledTaskTool_RejectsNeitherRunAtNorSchedule(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "create_scheduled_task", Arguments: `{"task":"x"}`}},
		llmtest.Response{Text: "ok"},
	)
	o, s := newTestOrchestratorWithSchedule(t, provider)

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "x"})
	require.NoError(t, err)

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Empty(t, tasks)
}

func TestOrchestrator_CreateScheduledTaskTool_RejectsInvalidCron(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "create_scheduled_task",
			Arguments: `{"task":"x","schedule":"not a cron expression"}`}},
		llmtest.Response{Text: "ok"},
	)
	o, s := newTestOrchestratorWithSchedule(t, provider)

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "x"})
	require.NoError(t, err)

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Empty(t, tasks)
}

func TestOrchestrator_CreateScheduledTaskTool_RejectsPastRunAt(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "create_scheduled_task",
			Arguments: `{"task":"x","run_at":"` + past + `"}`}},
		llmtest.Response{Text: "ok"},
	)
	o, s := newTestOrchestratorWithSchedule(t, provider)

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "x"})
	require.NoError(t, err)

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Empty(t, tasks)
}

func TestOrchestrator_ListScheduledTasksTool_ScopedToCallingUser(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "list_scheduled_tasks", Arguments: `{}`}},
		llmtest.Response{Text: "вот список"},
	)
	o, s := newTestOrchestratorWithSchedule(t, provider)

	next := time.Now().Add(time.Hour).UTC()
	_, err := s.Create(context.Background(), schedule.Task{UserID: "alex", Prompt: "alex's task", RunAt: &next, NextRunAt: next})
	require.NoError(t, err)
	_, err = s.Create(context.Background(), schedule.Task{UserID: "anna", Prompt: "anna's task", RunAt: &next, NextRunAt: next})
	require.NoError(t, err)

	_, err = o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "что у меня запланировано?"})
	require.NoError(t, err)

	last := provider.Requests[len(provider.Requests)-1].Messages
	toolResult := last[len(last)-1].Content
	require.Contains(t, toolResult, "alex's task")
	require.NotContains(t, toolResult, "anna's task")
}

func TestOrchestrator_DeleteScheduledTaskTool_OwnershipEnforced(t *testing.T) {
	next := time.Now().Add(time.Hour).UTC()

	// annaOnlyStore holds anna's task; alex tries (and must fail) to delete it.
	s, err := schedule.Open(filepath.Join(t.TempDir(), "schedule.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	annaTaskID, err := s.Create(context.Background(), schedule.Task{UserID: "anna", Prompt: "anna's task", RunAt: &next, NextRunAt: next})
	require.NoError(t, err)

	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "delete_scheduled_task", Arguments: `{"id":"` + annaTaskID + `"}`}},
		llmtest.Response{Text: "готово"},
	)
	o, _, _ := newTestOrchestrator(t, provider)
	o.SetSchedule(s)

	_, err = o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "удали задачу " + annaTaskID})
	require.NoError(t, err)

	last := provider.Requests[len(provider.Requests)-1].Messages
	require.Contains(t, last[len(last)-1].Content, "error")

	tasks, err := s.ListForUser(context.Background(), "anna")
	require.NoError(t, err)
	require.Len(t, tasks, 1) // untouched
}

func TestOrchestrator_ScheduledToolsNotOfferedWhenScheduleNotConfigured(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "Привет!"})
	o, _, _ := newTestOrchestrator(t, provider) // SetSchedule never called

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	require.Len(t, provider.Requests, 1)
	for _, tool := range provider.Requests[0].Tools {
		require.NotEqual(t, "create_scheduled_task", tool.Name)
		require.NotEqual(t, "list_scheduled_tasks", tool.Name)
		require.NotEqual(t, "delete_scheduled_task", tool.Name)
	}
}

func TestOrchestrator_RunScheduledTasks_FiresDueTaskThroughHandleAndDeletesOneOff(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "speak_reply", Arguments: `{"text":"доброе утро"}`}},
		llmtest.Response{Text: "готово"},
	)
	o, ha := newTestOrchestratorWithTTS(t, provider)

	s, err := schedule.Open(filepath.Join(t.TempDir(), "schedule.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	o.SetSchedule(s)

	due := time.Now().Add(-time.Minute).UTC()
	id, err := s.Create(context.Background(), schedule.Task{UserID: "alex", Prompt: "скажи доброе утро", RunAt: &due, NextRunAt: due})
	require.NoError(t, err)

	require.NoError(t, o.RunScheduledTasks(context.Background(), nil))

	requireEventuallySpoken(t, ha)

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Empty(t, tasks, "one-off task must be removed after firing")

	history, err := s.HistoryForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Len(t, history, 1, "the fired one-off task must survive in history even though its scheduled_tasks row is gone")
	require.Equal(t, id, history[0].TaskID)
	require.Equal(t, schedule.StatusSent, history[0].Status)
	require.Empty(t, history[0].Error)
}

func TestOrchestrator_RunScheduledTasks_LogsEachFiring(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "speak_reply", Arguments: `{"text":"доброе утро"}`}},
		llmtest.Response{Text: "готово"},
	)
	o, ha := newTestOrchestratorWithTTS(t, provider)

	s, err := schedule.Open(filepath.Join(t.TempDir(), "schedule.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	o.SetSchedule(s)

	due := time.Now().Add(-time.Minute).UTC()
	id, err := s.Create(context.Background(), schedule.Task{UserID: "alex", Prompt: "скажи доброе утро", RunAt: &due, NextRunAt: due})
	require.NoError(t, err)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	require.NoError(t, o.RunScheduledTasks(context.Background(), logger))
	requireEventuallySpoken(t, ha)

	logOutput := logBuf.String()
	require.Contains(t, logOutput, "scheduled task fired")
	require.Contains(t, logOutput, id)
	require.Contains(t, logOutput, "alex")
}

func TestOrchestrator_RunScheduledTasks_LogsFailureWhenHandleErrors(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Err: errors.New("boom")})
	o, _, _ := newTestOrchestrator(t, provider)

	s, err := schedule.Open(filepath.Join(t.TempDir(), "schedule.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	o.SetSchedule(s)

	due := time.Now().Add(-time.Minute).UTC()
	_, err = s.Create(context.Background(), schedule.Task{UserID: "alex", Prompt: "x", RunAt: &due, NextRunAt: due})
	require.NoError(t, err)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	require.NoError(t, o.RunScheduledTasks(context.Background(), logger))

	require.Contains(t, logBuf.String(), "scheduled task fire failed")

	// A failed fire must not be treated as done — it stays due for the next sweep.
	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Len(t, tasks, 1)

	history, err := s.HistoryForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Equal(t, schedule.StatusError, history[0].Status)
	require.Contains(t, history[0].Error, "boom")
}

func TestOrchestrator_RunScheduledTasks_ReschedulesRecurringTask(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "готово"})
	o, _, _ := newTestOrchestrator(t, provider)

	s, err := schedule.Open(filepath.Join(t.TempDir(), "schedule.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	o.SetSchedule(s)

	due := time.Now().Add(-time.Minute).UTC()
	id, err := s.Create(context.Background(), schedule.Task{UserID: "alex", Prompt: "утренняя рутина", CronExpr: "* * * * *", NextRunAt: due})
	require.NoError(t, err)

	require.NoError(t, o.RunScheduledTasks(context.Background(), nil))

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Len(t, tasks, 1, "recurring task must still exist after firing")
	require.Equal(t, id, tasks[0].ID)
	require.True(t, tasks[0].NextRunAt.After(due), "next_run_at must have advanced")
	require.NotNil(t, tasks[0].LastFiredAt)

	history, err := s.HistoryForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Len(t, history, 1, "a recurring task's firing must also be recorded even though its scheduled_tasks row stays")
	require.Equal(t, id, history[0].TaskID)
	require.Equal(t, schedule.StatusSent, history[0].Status)
}
