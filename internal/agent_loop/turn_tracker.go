package agentloop

import (
	"sync"
	"time"
)

// turnState is the refcount+timestamp kept per user by TurnTracker.
type turnState struct {
	// count is how many Handle calls are currently in flight for this user
	// — see TurnTracker's doc comment for why more than one is possible.
	count int
	// startedAt is set only on the 0 -> 1 transition (the oldest still-
	// running turn), so a second concurrent turn beginning doesn't reset
	// the elapsed-time clock a client may already be displaying.
	startedAt time.Time
}

// TurnTracker records, per user, how many Orchestrator.Handle turns are
// currently in flight and when the oldest of them started — this is what
// lets a web UI tab ask "is Miranda still working on something for me right
// now, and since when" instead of guessing from a fixed client-side
// timeout. See GET /ws/chat/{username}'s turn_in_progress snapshot and
// GET /api/turn-status (internal/webui) for the two ways this is exposed.
//
// Keyed by user id, not conversation id or source: session continuity is
// server-owned and keyed only on user identity (see CLAUDE.md's "Session
// ownership"), and the same user's turn can in principle be triggered
// concurrently from more than one channel (HA voice, web UI, Telegram, a
// scheduled task) — a plain bool would let one call's completion clear
// state a still-running concurrent call needs, so this refcounts instead.
type TurnTracker struct {
	mu     sync.Mutex
	active map[string]*turnState
}

// NewTurnTracker returns an empty tracker.
func NewTurnTracker() *TurnTracker {
	return &TurnTracker{active: make(map[string]*turnState)}
}

// Begin records that a turn has started for userID and returns a function
// that must be called exactly once when that turn finishes (success, error,
// or timeout alike — callers should register it via defer immediately after
// calling Begin, before anything that can return early).
func (t *TurnTracker) Begin(userID string) (end func()) {
	t.mu.Lock()
	st, ok := t.active[userID]
	if !ok {
		st = &turnState{startedAt: time.Now()}
		t.active[userID] = st
	}
	st.count++
	t.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			st.count--
			if st.count <= 0 {
				delete(t.active, userID)
			}
		})
	}
}

// Status reports whether userID currently has any turn in flight and, if
// so, when the oldest still-running one started.
func (t *TurnTracker) Status(userID string) (inProgress bool, startedAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.active[userID]
	if !ok {
		return false, time.Time{}
	}
	return true, st.startedAt
}
