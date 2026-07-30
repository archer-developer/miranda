package schedule

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "schedule.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestCreateAndListForUser_RoundTripsScopedPerUser(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	idAlex, err := s.Create(ctx, Task{UserID: "alex", Prompt: "напомни выпить пива", RunAt: &next, NextRunAt: next})
	require.NoError(t, err)
	require.NotEmpty(t, idAlex)

	_, err = s.Create(ctx, Task{UserID: "anna", Prompt: "доброе утро", CronExpr: "1 9 * * *", NextRunAt: next.Add(time.Minute)})
	require.NoError(t, err)

	tasks, err := s.ListForUser(ctx, "alex")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, idAlex, tasks[0].ID)
	require.Equal(t, "напомни выпить пива", tasks[0].Prompt)
	require.NotNil(t, tasks[0].RunAt)
	require.Empty(t, tasks[0].CronExpr)

	tasksAnna, err := s.ListForUser(ctx, "anna")
	require.NoError(t, err)
	require.Len(t, tasksAnna, 1)
	require.Equal(t, "1 9 * * *", tasksAnna[0].CronExpr)
	require.Nil(t, tasksAnna[0].RunAt)
}

func TestListForUser_OrderedByNextRunAt(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	later := now.Add(2 * time.Hour)
	idLater, err := s.Create(ctx, Task{UserID: "alex", Prompt: "later", RunAt: &later, NextRunAt: later})
	require.NoError(t, err)
	sooner := now.Add(time.Hour)
	idSooner, err := s.Create(ctx, Task{UserID: "alex", Prompt: "sooner", RunAt: &sooner, NextRunAt: sooner})
	require.NoError(t, err)

	tasks, err := s.ListForUser(ctx, "alex")
	require.NoError(t, err)
	require.Len(t, tasks, 2)
	require.Equal(t, idSooner, tasks[0].ID)
	require.Equal(t, idLater, tasks[1].ID)
}

func TestDueTasks_BoundaryTime(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	past := now.Add(-time.Minute)
	future := now.Add(time.Hour)

	idDue, err := s.Create(ctx, Task{UserID: "alex", Prompt: "past due", RunAt: &past, NextRunAt: past})
	require.NoError(t, err)
	_, err = s.Create(ctx, Task{UserID: "alex", Prompt: "not yet", RunAt: &future, NextRunAt: future})
	require.NoError(t, err)

	due, err := s.DueTasks(ctx, now)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Equal(t, idDue, due[0].ID)
}

func TestDelete_OwnershipScoped(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	id, err := s.Create(ctx, Task{UserID: "alex", Prompt: "напомни", RunAt: &next, NextRunAt: next})
	require.NoError(t, err)

	// A different user's id must not be able to delete alex's task.
	err = s.Delete(ctx, id, "anna")
	require.ErrorIs(t, err, ErrNotFound)

	tasks, err := s.ListForUser(ctx, "alex")
	require.NoError(t, err)
	require.Len(t, tasks, 1)

	require.NoError(t, s.Delete(ctx, id, "alex"))
	tasks, err = s.ListForUser(ctx, "alex")
	require.NoError(t, err)
	require.Empty(t, tasks)
}

func TestDelete_UnknownID(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	err := s.Delete(ctx, "does-not-exist", "alex")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestReschedule_UpdatesNextRunAtAndLastFiredAt(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	id, err := s.Create(ctx, Task{UserID: "alex", Prompt: "morning routine", CronExpr: "1 9 * * *", NextRunAt: now})
	require.NoError(t, err)

	next := now.Add(24 * time.Hour)
	require.NoError(t, s.Reschedule(ctx, id, next))

	tasks, err := s.ListForUser(ctx, "alex")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.WithinDuration(t, next, tasks[0].NextRunAt, time.Second)
	require.NotNil(t, tasks[0].LastFiredAt)
}

func TestDeleteFired_RemovesRow(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	id, err := s.Create(ctx, Task{UserID: "alex", Prompt: "one-off", RunAt: &next, NextRunAt: next})
	require.NoError(t, err)

	require.NoError(t, s.DeleteFired(ctx, id))

	tasks, err := s.ListForUser(ctx, "alex")
	require.NoError(t, err)
	require.Empty(t, tasks)
}

func TestMigrate_IsIdempotentAcrossReopens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "schedule.db")

	s1, err := Open(path)
	require.NoError(t, err)
	next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	id, err := s1.Create(ctx, Task{UserID: "alex", Prompt: "hello", RunAt: &next, NextRunAt: next})
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	tasks, err := s2.ListForUser(ctx, "alex")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, id, tasks[0].ID)
}
