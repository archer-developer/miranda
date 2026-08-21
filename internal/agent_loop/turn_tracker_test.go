package agentloop

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTurnTracker_NotInProgressInitially(t *testing.T) {
	tr := NewTurnTracker()
	inProgress, startedAt := tr.Status("alex")
	require.False(t, inProgress)
	require.True(t, startedAt.IsZero())
}

func TestTurnTracker_BeginEndRoundTrip(t *testing.T) {
	tr := NewTurnTracker()
	end := tr.Begin("alex")

	inProgress, startedAt := tr.Status("alex")
	require.True(t, inProgress)
	require.False(t, startedAt.IsZero())

	end()

	inProgress, _ = tr.Status("alex")
	require.False(t, inProgress)
}

func TestTurnTracker_ConcurrentTurnsDoNotResetStartedAt(t *testing.T) {
	tr := NewTurnTracker()
	end1 := tr.Begin("alex")
	_, firstStart := tr.Status("alex")

	time.Sleep(2 * time.Millisecond)
	end2 := tr.Begin("alex")
	_, secondStart := tr.Status("alex")
	require.Equal(t, firstStart, secondStart, "a second concurrent turn must not reset the elapsed-time clock")

	// Only fully clears once every concurrent turn has ended.
	end1()
	inProgress, _ := tr.Status("alex")
	require.True(t, inProgress, "one turn (end2) is still in flight")

	end2()
	inProgress, _ = tr.Status("alex")
	require.False(t, inProgress)
}

func TestTurnTracker_EndIsIdempotent(t *testing.T) {
	tr := NewTurnTracker()
	end := tr.Begin("alex")
	end()
	end() // must not double-decrement into a negative/ghost state

	inProgress, _ := tr.Status("alex")
	require.False(t, inProgress)

	// A fresh Begin after the idempotent double-end should behave normally.
	end2 := tr.Begin("alex")
	inProgress, _ = tr.Status("alex")
	require.True(t, inProgress)
	end2()
	inProgress, _ = tr.Status("alex")
	require.False(t, inProgress)
}

func TestTurnTracker_PerUserIsolation(t *testing.T) {
	tr := NewTurnTracker()
	end := tr.Begin("alex")
	defer end()

	inProgress, _ := tr.Status("someone-else")
	require.False(t, inProgress)
}

func TestTurnTracker_ConcurrentAccess(t *testing.T) {
	tr := NewTurnTracker()
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			end := tr.Begin("alex")
			tr.Status("alex")
			end()
		}()
	}
	wg.Wait()

	inProgress, _ := tr.Status("alex")
	require.False(t, inProgress)
}
