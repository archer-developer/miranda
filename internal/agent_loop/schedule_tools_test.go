package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda-llm/router"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/memory"
	"github.com/archer-developer/miranda/internal/schedule"
	"github.com/archer-developer/miranda/internal/telegram"
	"github.com/archer-developer/miranda/internal/tts"
	"github.com/archer-developer/miranda/internal/users"
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

// --- create_reminder ---

func TestOrchestrator_CreateReminderTool_OneOff(t *testing.T) {
	runAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "create_reminder",
			Arguments: `{"message":"Полить кактус","run_at":"` + runAt + `"}`}},
		llmtest.Response{Text: "Хорошо, напомню."},
	)
	o, s := newTestOrchestratorWithSchedule(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "напомни мне полить кактус в 22:00"})
	require.NoError(t, err)
	require.Equal(t, "Хорошо, напомню.", resp.Reply)

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, schedule.KindReminder, tasks[0].Kind)
	require.Equal(t, "Полить кактус", tasks[0].Prompt)
	require.Equal(t, "cli", tasks[0].OriginSource)
	require.False(t, tasks[0].AnnounceAloud)
	require.NotNil(t, tasks[0].RunAt)
	require.Empty(t, tasks[0].CronExpr)
}

func TestOrchestrator_CreateReminderTool_Recurring(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "create_reminder",
			Arguments: `{"message":"Выпить воды","schedule":"0 */2 * * *","announce_aloud":true}`}},
		llmtest.Response{Text: "Хорошо."},
	)
	o, s := newTestOrchestratorWithSchedule(t, provider)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "напоминай мне пить воду каждые 2 часа"})
	require.NoError(t, err)
	require.Equal(t, "Хорошо.", resp.Reply)

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, schedule.KindReminder, tasks[0].Kind)
	require.Equal(t, "0 */2 * * *", tasks[0].CronExpr)
	require.Nil(t, tasks[0].RunAt)
	require.True(t, tasks[0].AnnounceAloud)
}

func TestOrchestrator_CreateReminderTool_CapturesOriginSource(t *testing.T) {
	runAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "create_reminder",
			Arguments: `{"message":"Позвонить маме","run_at":"` + runAt + `"}`}},
		llmtest.Response{Text: "Хорошо."},
	)
	o, s := newTestOrchestratorWithSchedule(t, provider)

	_, err := o.Handle(context.Background(), InputRequest{Source: users.SourceTelegram, UserID: "alex", Text: "напомни позвонить маме"})
	require.NoError(t, err)

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, users.SourceTelegram, tasks[0].OriginSource)
}

func TestOrchestrator_CreateReminderTool_RejectsBothRunAtAndSchedule(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "create_reminder",
			Arguments: `{"message":"x","run_at":"2099-01-01T00:00:00Z","schedule":"1 9 * * *"}`}},
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

func TestOrchestrator_CreateReminderTool_RejectsNeitherRunAtNorSchedule(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "create_reminder", Arguments: `{"message":"x"}`}},
		llmtest.Response{Text: "ok"},
	)
	o, s := newTestOrchestratorWithSchedule(t, provider)

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "x"})
	require.NoError(t, err)

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Empty(t, tasks)
}

func TestOrchestrator_CreateReminderTool_RejectsInvalidCron(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "create_reminder",
			Arguments: `{"message":"x","schedule":"not a cron expression"}`}},
		llmtest.Response{Text: "ok"},
	)
	o, s := newTestOrchestratorWithSchedule(t, provider)

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "x"})
	require.NoError(t, err)

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Empty(t, tasks)
}

func TestOrchestrator_CreateReminderTool_RejectsPastRunAt(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "create_reminder",
			Arguments: `{"message":"x","run_at":"` + past + `"}`}},
		llmtest.Response{Text: "ok"},
	)
	o, s := newTestOrchestratorWithSchedule(t, provider)

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "x"})
	require.NoError(t, err)

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Empty(t, tasks)
}

func TestOrchestrator_ListScheduledTasksTool_ShowsKind(t *testing.T) {
	next := time.Now().Add(time.Hour).UTC()
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "list_scheduled_tasks", Arguments: `{}`}},
		llmtest.Response{Text: "вот список"},
	)
	o, s := newTestOrchestratorWithSchedule(t, provider)

	_, err := s.Create(context.Background(), schedule.Task{UserID: "alex", Prompt: "проверь дверь", RunAt: &next, NextRunAt: next})
	require.NoError(t, err)
	_, err = s.Create(context.Background(), schedule.Task{UserID: "alex", Prompt: "Полить кактус", Kind: schedule.KindReminder, RunAt: &next, NextRunAt: next})
	require.NoError(t, err)

	_, err = o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "что у меня запланировано?"})
	require.NoError(t, err)

	last := provider.Requests[len(provider.Requests)-1].Messages
	toolResult := last[len(last)-1].Content
	require.Contains(t, toolResult, "kind=task")
	require.Contains(t, toolResult, "kind=reminder")
}

// --- RunScheduledTasks: reminder delivery (the actual bug-fix path) ---

// newTestOrchestratorWithReminderChannels wires TTS (fakeHAClient),
// Telegram (fake Bot API server), and a real schedule.Store into one
// Orchestrator, so a single test can assert on every reminder delivery leg
// (origin channel, always-additionally Telegram, history append) at once.
func newTestOrchestratorWithReminderChannels(t *testing.T, provider *llmtest.FakeProvider, configs []config.UserConfig) (*Orchestrator, *fakeHAClient, *[]sentTelegramMessage, *schedule.Store) {
	t.Helper()

	r, err := router.New([]llm.Provider{provider}, selfEscalation(provider.Name()), "")
	require.NoError(t, err)

	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	ha := &fakeHAClient{}
	yandexCfg := config.YandexStationConfig{
		ChunkMaxChars:          100,
		IdlePollIntervalMS:     1,
		PlaybackStartTimeoutMS: 1,
	}
	ttsCfg := config.TTSConfig{
		DefaultDevice: "test-kitchen",
		Primary:       "yandex_station_text",
		YandexStation: yandexCfg,
	}
	primary := tts.NewTextProvider(yandexCfg, ha, nil)
	dispatcher := tts.NewDispatcher(primary, nil, ha, "test-kitchen", hub.New(100, nil), nil)

	var sent []sentTelegramMessage
	fakeAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = append(sent, sentTelegramMessage{ChatID: body["chat_id"].(float64), Text: body["text"].(string)})
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(fakeAPI.Close)

	registry, err := users.NewRegistry(configs)
	require.NoError(t, err)
	chats, err := telegram.OpenChatStore(filepath.Join(t.TempDir(), "chats.json"))
	require.NoError(t, err)
	sender := telegram.NewSender(telegram.NewWithAPIBase("test-token", fakeAPI.URL), chats)
	for i, c := range configs {
		require.NoError(t, chats.Save(c.Username, int64(i+1)))
	}

	o := NewOrchestrator(
		r, mcp.NewManager(nil), h, mem, dispatcher, hub.New(100, nil), registry,
		config.AgentConfig{},
		config.MemoryConfig{},
		ttsCfg,
		100, "debug",
	)
	o.SetTelegram(sender, config.TelegramConfig{SendMessageTool: true})

	s, err := schedule.Open(filepath.Join(t.TempDir(), "schedule.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	o.SetSchedule(s)

	return o, ha, &sent, s
}

func TestOrchestrator_RunScheduledTasks_FiresReminder_NeverCallsHandle(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "should never be used"})
	o, ha, _, s := newTestOrchestratorWithReminderChannels(t, provider, []config.UserConfig{{Username: "alex"}})

	due := time.Now().Add(-time.Minute).UTC()
	id, err := s.Create(context.Background(), schedule.Task{
		UserID: "alex", Prompt: "Полить кактус", Kind: schedule.KindReminder,
		OriginSource: users.SourceHAAssist, RunAt: &due, NextRunAt: due,
	})
	require.NoError(t, err)

	require.NoError(t, o.RunScheduledTasks(context.Background(), nil))

	require.Empty(t, provider.Requests, "a reminder must never trigger an LLM call — that's exactly the reinterpretation/reschedule loop this split fixes")

	requireEventuallySpoken(t, ha)

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Empty(t, tasks, "one-off reminder must be removed after firing")

	hist, err := s.HistoryForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Len(t, hist, 1)
	require.Equal(t, id, hist[0].TaskID)
	require.Equal(t, schedule.StatusSent, hist[0].Status)
	require.Equal(t, schedule.KindReminder, hist[0].Kind)
}

func TestOrchestrator_RunScheduledTasks_ReminderOriginTelegram_SendsOnce(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "unused"})
	o, _, sent, s := newTestOrchestratorWithReminderChannels(t, provider, []config.UserConfig{{Username: "alex"}})

	due := time.Now().Add(-time.Minute).UTC()
	_, err := s.Create(context.Background(), schedule.Task{
		UserID: "alex", Prompt: "Полить кактус", Kind: schedule.KindReminder,
		OriginSource: users.SourceTelegram, RunAt: &due, NextRunAt: due,
	})
	require.NoError(t, err)

	require.NoError(t, o.RunScheduledTasks(context.Background(), nil))

	require.Len(t, *sent, 1, "telegram-origin reminder must be sent exactly once, not duplicated by the always-additionally leg")
	require.Equal(t, "Полить кактус", (*sent)[0].Text)
}

func TestOrchestrator_RunScheduledTasks_ReminderOriginWebUI_AlsoSendsTelegram(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "unused"})
	o, _, sent, s := newTestOrchestratorWithReminderChannels(t, provider, []config.UserConfig{{Username: "alex"}})

	due := time.Now().Add(-time.Minute).UTC()
	_, err := s.Create(context.Background(), schedule.Task{
		UserID: "alex", Prompt: "Полить кактус", Kind: schedule.KindReminder,
		OriginSource: "web_ui", RunAt: &due, NextRunAt: due,
	})
	require.NoError(t, err)

	open, err := o.history.StartConversation(context.Background(), "alex", "web_ui")
	require.NoError(t, err)

	require.NoError(t, o.RunScheduledTasks(context.Background(), nil))

	require.Len(t, *sent, 1, "non-telegram origin must still get the always-additionally Telegram push")

	msgs, err := o.history.ConversationMessages(context.Background(), open)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	require.Equal(t, "Полить кактус", msgs[len(msgs)-1].Content)
}

func TestOrchestrator_RunScheduledTasks_ReminderAnnounceAloud_LayeredOnTopOfOrigin(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "unused"})
	o, ha, sent, s := newTestOrchestratorWithReminderChannels(t, provider, []config.UserConfig{{Username: "alex"}})

	due := time.Now().Add(-time.Minute).UTC()
	_, err := s.Create(context.Background(), schedule.Task{
		UserID: "alex", Prompt: "Позвонить маме", Kind: schedule.KindReminder,
		OriginSource: users.SourceTelegram, AnnounceAloud: true, RunAt: &due, NextRunAt: due,
	})
	require.NoError(t, err)

	require.NoError(t, o.RunScheduledTasks(context.Background(), nil))

	requireEventuallySpoken(t, ha)
	require.Len(t, *sent, 1, "AnnounceAloud must not suppress the telegram-origin delivery it was created through")
}

func TestOrchestrator_RunScheduledTasks_ReminderTelegramSendFails_StillRecordsError(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "unused"})
	// "ghost" is never Save()'d into the chat store, so SendToUser errors.
	o, _, _, s := newTestOrchestratorWithReminderChannels(t, provider, []config.UserConfig{{Username: "alex"}})

	due := time.Now().Add(-time.Minute).UTC()
	id, err := s.Create(context.Background(), schedule.Task{
		UserID: "ghost", Prompt: "Полить кактус", Kind: schedule.KindReminder,
		OriginSource: users.SourceTelegram, RunAt: &due, NextRunAt: due,
	})
	require.NoError(t, err)

	require.NoError(t, o.RunScheduledTasks(context.Background(), nil))

	tasks, err := s.ListForUser(context.Background(), "ghost")
	require.NoError(t, err)
	require.Len(t, tasks, 1, "a failed origin-channel delivery must leave the one-off row due for retry")

	hist, err := s.HistoryForUser(context.Background(), "ghost")
	require.NoError(t, err)
	require.Len(t, hist, 1)
	require.Equal(t, id, hist[0].TaskID)
	require.Equal(t, schedule.StatusError, hist[0].Status)
}

// TestOrchestrator_RunScheduledTasks_RecurringReschedulesEvenOnFailure pins
// down the fix for a retry-forever bug: before it, a recurring row whose
// firing failed hit `continue` before ever reaching the reschedule block, so
// NextRunAt never advanced and the same due row fired (and failed) again on
// every ~1-minute sweep tick forever instead of its real cron cadence. A
// deterministic failure (e.g. a Telegram-origin reminder whose user has
// since blocked the bot, as here) must still advance to the next occurrence.
func TestOrchestrator_RunScheduledTasks_RecurringReschedulesEvenOnFailure(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "unused"})
	o, _, _, s := newTestOrchestratorWithReminderChannels(t, provider, []config.UserConfig{{Username: "alex"}})

	due := time.Now().Add(-time.Minute).UTC()
	// "ghost" is never Save()'d into the chat store, so every firing's
	// origin-channel Telegram send deterministically fails.
	id, err := s.Create(context.Background(), schedule.Task{
		UserID: "ghost", Prompt: "Полить кактус", Kind: schedule.KindReminder,
		OriginSource: users.SourceTelegram, CronExpr: "* * * * *", NextRunAt: due,
	})
	require.NoError(t, err)

	require.NoError(t, o.RunScheduledTasks(context.Background(), nil))

	tasks, err := s.ListForUser(context.Background(), "ghost")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, id, tasks[0].ID)
	require.True(t, tasks[0].NextRunAt.After(due),
		"a recurring reminder must reschedule to its next cadence even when delivery fails, not stay due for every sweep tick")

	hist, err := s.HistoryForUser(context.Background(), "ghost")
	require.NoError(t, err)
	require.Len(t, hist, 1)
	require.Equal(t, schedule.StatusError, hist[0].Status)
}

// TestOrchestrator_RunScheduledTasks_ReminderVoiceDeliveryFails_RecordsError
// pins down the fix making a voice-origin reminder's device-resolution
// failure detectable: before it, deliverReminder called the fire-and-forget
// speakText, which can never return an error, so a reminder aimed at a
// misconfigured/unresolvable device was unconditionally recorded
// StatusSent even though it could never have reached the user.
func TestOrchestrator_RunScheduledTasks_ReminderVoiceDeliveryFails_RecordsError(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "unused"})

	r, err := router.New([]llm.Provider{provider}, selfEscalation(provider.Name()), "")
	require.NoError(t, err)
	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })
	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	// fakeHAClient.ResolveMediaPlayer only resolves "test-kitchen" (see
	// orchestrator_test.go) — pointing the dispatcher at any other name
	// makes every resolution attempt fail deterministically.
	ha := &fakeHAClient{}
	yandexCfg := config.YandexStationConfig{ChunkMaxChars: 100, IdlePollIntervalMS: 1, PlaybackStartTimeoutMS: 1}
	const unresolvableDevice = "no-such-device"
	ttsCfg := config.TTSConfig{DefaultDevice: unresolvableDevice, Primary: "yandex_station_text", YandexStation: yandexCfg}
	primary := tts.NewTextProvider(yandexCfg, ha, nil)
	dispatcher := tts.NewDispatcher(primary, nil, ha, unresolvableDevice, hub.New(100, nil), nil)

	o := NewOrchestrator(
		r, mcp.NewManager(nil), h, mem, dispatcher, hub.New(100, nil), nil,
		config.AgentConfig{}, config.MemoryConfig{}, ttsCfg, 100, "debug",
	)

	s, err := schedule.Open(filepath.Join(t.TempDir(), "schedule.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	o.SetSchedule(s)

	due := time.Now().Add(-time.Minute).UTC()
	id, err := s.Create(context.Background(), schedule.Task{
		UserID: "alex", Prompt: "Полить кактус", Kind: schedule.KindReminder,
		OriginSource: users.SourceHAAssist, RunAt: &due, NextRunAt: due,
	})
	require.NoError(t, err)

	require.NoError(t, o.RunScheduledTasks(context.Background(), nil))

	require.Empty(t, ha.Calls(), "an unresolvable device must never reach play_media")

	tasks, err := s.ListForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Len(t, tasks, 1, "a failed one-off reminder delivery must stay due for retry, not be deleted as if it had succeeded")

	hist, err := s.HistoryForUser(context.Background(), "alex")
	require.NoError(t, err)
	require.Len(t, hist, 1)
	require.Equal(t, id, hist[0].TaskID)
	require.Equal(t, schedule.StatusError, hist[0].Status)
}
