package schedule

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeRedactor stands in for internal/redact — see history.fakeRedactor.
type fakeRedactor struct{}

func (fakeRedactor) Redact(s string) string {
	return strings.ReplaceAll(s, "665533", "******")
}

func openRedactingStore(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t)
	s.SetRedactor(fakeRedactor{})
	return s
}

func TestCreate_RedactsPrompt(t *testing.T) {
	ctx := context.Background()
	s := openRedactingStore(t)

	next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	_, err := s.Create(ctx, Task{UserID: "alex", Prompt: "напомни Ане пин-код 665533", RunAt: &next, NextRunAt: next})
	require.NoError(t, err)

	tasks, err := s.ListForUser(ctx, "alex")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, "напомни Ане пин-код ******", tasks[0].Prompt)
}

// TestRecordRun_RedactsPrompt — scheduled_task_history is the copy that
// outlives the task itself (DeleteFired removes a one-off task's own row but
// never its history), so it needs masking just as much as scheduled_tasks.
func TestRecordRun_RedactsPrompt(t *testing.T) {
	ctx := context.Background()
	s := openRedactingStore(t)

	next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	task := Task{ID: "task-1", UserID: "alex", Prompt: "напомни пин 665533", RunAt: &next, NextRunAt: next}
	require.NoError(t, s.RecordRun(ctx, task, StatusSent, ""))

	runs, err := s.HistoryForUser(ctx, "alex")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, "напомни пин ******", runs[0].Prompt)
}

func TestStore_WithoutRedactorStoresVerbatim(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	_, err := s.Create(ctx, Task{UserID: "alex", Prompt: "пин 665533", RunAt: &next, NextRunAt: next})
	require.NoError(t, err)

	tasks, err := s.ListForUser(ctx, "alex")
	require.NoError(t, err)
	require.Equal(t, "пин 665533", tasks[0].Prompt)
}
