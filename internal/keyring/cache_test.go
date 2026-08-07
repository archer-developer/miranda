package keyring

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCache_UnlockGetLock(t *testing.T) {
	c := NewCache()

	_, ok := c.Get("archer")
	require.False(t, ok)

	key := []byte{1, 2, 3, 4}
	c.Unlock("archer", key)

	got, ok := c.Get("archer")
	require.True(t, ok)
	require.Equal(t, key, got)

	c.Lock("archer")
	_, ok = c.Get("archer")
	require.False(t, ok)
}

func TestCache_LockZeroesKeyBytes(t *testing.T) {
	c := NewCache()
	key := []byte{9, 9, 9, 9}
	c.Unlock("archer", key)
	c.Lock("archer")
	require.Equal(t, []byte{0, 0, 0, 0}, key)
}

func TestCache_ScopedByUsername(t *testing.T) {
	c := NewCache()
	c.Unlock("archer", []byte{1})
	c.Unlock("anna", []byte{2})

	c.Lock("archer")
	_, ok := c.Get("archer")
	require.False(t, ok)

	got, ok := c.Get("anna")
	require.True(t, ok)
	require.Equal(t, []byte{2}, got)
}

func TestCache_GetReturnsIndependentCopy(t *testing.T) {
	c := NewCache()
	c.Unlock("archer", []byte{1, 2, 3, 4})

	got, ok := c.Get("archer")
	require.True(t, ok)
	got[0] = 99 // mutate the caller's copy

	got2, ok := c.Get("archer")
	require.True(t, ok)
	require.Equal(t, []byte{1, 2, 3, 4}, got2, "mutating a value returned by Get must never affect the cache's own stored key")
}

// TestCache_GetDuringConcurrentLockDoesNotRace guards a real data race: Get
// used to return the cache's own backing array, and Lock zeroes that same
// array in place — a caller encoding the bytes returned by Get (e.g. for a
// tool-call argument) could read a torn or zeroed key if that raced with a
// concurrent Lock (e.g. from a logout). Run with -race.
func TestCache_GetDuringConcurrentLockDoesNotRace(t *testing.T) {
	c := NewCache()
	c.Unlock("archer", []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if key, ok := c.Get("archer"); ok {
				_ = append([]byte(nil), key...) // simulate encoding the returned bytes
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			c.Unlock("archer", []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
			c.Lock("archer")
		}
	}()
	wg.Wait()
}
