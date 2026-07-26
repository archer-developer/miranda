package webauthn

import (
	"testing"
	"time"

	webauthnlib "github.com/go-webauthn/webauthn/webauthn"
	"github.com/stretchr/testify/require"
)

func TestCeremonyStore_PutThenTakeRoundTrips(t *testing.T) {
	c := NewCeremonyStore(time.Minute)
	data := webauthnlib.SessionData{Challenge: "chal-1"}

	c.Put("session-token", data)

	got, ok := c.Take("session-token")
	require.True(t, ok)
	require.Equal(t, "chal-1", got.Challenge)
}

func TestCeremonyStore_TakeIsConsuming(t *testing.T) {
	c := NewCeremonyStore(time.Minute)
	c.Put("session-token", webauthnlib.SessionData{Challenge: "chal-1"})

	_, ok := c.Take("session-token")
	require.True(t, ok)

	// A second Take (replayed finish) must fail outright.
	_, ok = c.Take("session-token")
	require.False(t, ok)
}

func TestCeremonyStore_PutNewGeneratesUniqueKeys(t *testing.T) {
	c := NewCeremonyStore(time.Minute)

	key1, err := c.PutNew(webauthnlib.SessionData{Challenge: "a"})
	require.NoError(t, err)
	key2, err := c.PutNew(webauthnlib.SessionData{Challenge: "b"})
	require.NoError(t, err)

	require.NotEmpty(t, key1)
	require.NotEqual(t, key1, key2)

	got1, ok := c.Take(key1)
	require.True(t, ok)
	require.Equal(t, "a", got1.Challenge)

	got2, ok := c.Take(key2)
	require.True(t, ok)
	require.Equal(t, "b", got2.Challenge)
}

func TestCeremonyStore_TakeExpiredEntryFails(t *testing.T) {
	c := NewCeremonyStore(0) // expires immediately
	c.Put("session-token", webauthnlib.SessionData{Challenge: "chal-1"})

	time.Sleep(time.Millisecond)

	_, ok := c.Take("session-token")
	require.False(t, ok)
}

func TestCeremonyStore_TakeMissingKeyFails(t *testing.T) {
	c := NewCeremonyStore(time.Minute)

	_, ok := c.Take("does-not-exist")
	require.False(t, ok)

	_, ok = c.Take("")
	require.False(t, ok)
}

func TestCeremonyStore_PutOverwritesPriorEntryForSameKey(t *testing.T) {
	c := NewCeremonyStore(time.Minute)
	c.Put("session-token", webauthnlib.SessionData{Challenge: "first"})
	c.Put("session-token", webauthnlib.SessionData{Challenge: "second"})

	got, ok := c.Take("session-token")
	require.True(t, ok)
	require.Equal(t, "second", got.Challenge)
}
