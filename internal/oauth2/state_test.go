package oauth2

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPendingAuthStore_ConsumeIsSingleUse(t *testing.T) {
	s := NewPendingAuthStore(time.Minute)
	defer s.Close()

	s.Put("state1", PendingAuth{Username: "alice", Provider: "google_calendar"})

	got, ok := s.Consume("state1")
	require.True(t, ok)
	require.Equal(t, "alice", got.Username)

	_, ok = s.Consume("state1")
	require.False(t, ok, "a state must not be consumable twice")
}

func TestPendingAuthStore_UnknownState(t *testing.T) {
	s := NewPendingAuthStore(time.Minute)
	defer s.Close()

	_, ok := s.Consume("does-not-exist")
	require.False(t, ok)
}

func TestPendingAuthStore_ExpiredEntryRejectedOnConsume(t *testing.T) {
	s := NewPendingAuthStore(10 * time.Millisecond)
	defer s.Close()

	s.Put("state1", PendingAuth{Username: "alice", Provider: "google_calendar"})
	time.Sleep(30 * time.Millisecond)

	_, ok := s.Consume("state1")
	require.False(t, ok, "an expired entry must not be consumable even before the sweeper runs")
}

func TestNewState_Unique(t *testing.T) {
	a, err := NewState()
	require.NoError(t, err)
	b, err := NewState()
	require.NoError(t, err)
	require.NotEqual(t, a, b)
	require.Len(t, a, 48) // 24 random bytes, hex-encoded
}
