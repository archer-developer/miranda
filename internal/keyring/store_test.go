package keyring

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "keyring.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestPutAndGetSlot_RoundTrips(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	slot := Slot{
		Username: "archer", SlotType: SlotTypePassword, SlotRef: "",
		WrappedKey: []byte{1, 2, 3}, Nonce: []byte{4, 5, 6},
		KDFSalt: []byte{7, 8, 9}, KDFParams: "argon2id$v=19$m=65536,t=1,p=4",
	}
	require.NoError(t, s.PutSlot(ctx, slot))

	got, ok, err := s.GetSlot(ctx, "archer", SlotTypePassword, "")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, slot.WrappedKey, got.WrappedKey)
	require.Equal(t, slot.Nonce, got.Nonce)
	require.Equal(t, slot.KDFSalt, got.KDFSalt)
	require.Equal(t, slot.KDFParams, got.KDFParams)
}

func TestGetSlot_ReturnsFalseWhenMissing(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	_, ok, err := s.GetSlot(ctx, "archer", SlotTypePassword, "")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestPutSlot_UpsertsRatherThanDuplicates(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	require.NoError(t, s.PutSlot(ctx, Slot{
		Username: "archer", SlotType: SlotTypeWebAuthn, SlotRef: "cred-1",
		WrappedKey: []byte{1}, Nonce: []byte{1},
	}))
	require.NoError(t, s.PutSlot(ctx, Slot{
		Username: "archer", SlotType: SlotTypeWebAuthn, SlotRef: "cred-1",
		WrappedKey: []byte{2}, Nonce: []byte{2},
	}))

	n, err := s.CountSlots(ctx, "archer")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	got, ok, err := s.GetSlot(ctx, "archer", SlotTypeWebAuthn, "cred-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []byte{2}, got.WrappedKey)
}

func TestCountSlots_ScopedByUsername(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	require.NoError(t, s.PutSlot(ctx, Slot{Username: "archer", SlotType: SlotTypePassword, WrappedKey: []byte{1}, Nonce: []byte{1}}))
	require.NoError(t, s.PutSlot(ctx, Slot{Username: "archer", SlotType: SlotTypeWebAuthn, SlotRef: "cred-1", WrappedKey: []byte{1}, Nonce: []byte{1}}))
	require.NoError(t, s.PutSlot(ctx, Slot{Username: "anna", SlotType: SlotTypePassword, WrappedKey: []byte{1}, Nonce: []byte{1}}))

	n, err := s.CountSlots(ctx, "archer")
	require.NoError(t, err)
	require.Equal(t, 2, n)

	n, err = s.CountSlots(ctx, "anna")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	n, err = s.CountSlots(ctx, "nobody")
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

func TestDeleteSlot_RemovesOnlyThatSlot(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	require.NoError(t, s.PutSlot(ctx, Slot{Username: "archer", SlotType: SlotTypeWebAuthn, SlotRef: "cred-1", WrappedKey: []byte{1}, Nonce: []byte{1}}))
	require.NoError(t, s.PutSlot(ctx, Slot{Username: "archer", SlotType: SlotTypeWebAuthn, SlotRef: "cred-2", WrappedKey: []byte{1}, Nonce: []byte{1}}))

	require.NoError(t, s.DeleteSlot(ctx, "archer", SlotTypeWebAuthn, "cred-1"))

	_, ok, err := s.GetSlot(ctx, "archer", SlotTypeWebAuthn, "cred-1")
	require.NoError(t, err)
	require.False(t, ok)

	_, ok, err = s.GetSlot(ctx, "archer", SlotTypeWebAuthn, "cred-2")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestBootstrapSlotIfEmpty_FirstCallBootstraps(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	bootstrapped, err := s.BootstrapSlotIfEmpty(ctx, "archer", Slot{
		SlotType: SlotTypePassword, WrappedKey: []byte{1, 2, 3}, Nonce: []byte{4, 5, 6},
	})
	require.NoError(t, err)
	require.True(t, bootstrapped)

	n, err := s.CountSlots(ctx, "archer")
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func TestBootstrapSlotIfEmpty_NoopWhenUserAlreadyHasASlot(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	require.NoError(t, s.PutSlot(ctx, Slot{
		Username: "archer", SlotType: SlotTypePassword, WrappedKey: []byte{1}, Nonce: []byte{1},
	}))

	bootstrapped, err := s.BootstrapSlotIfEmpty(ctx, "archer", Slot{
		SlotType: SlotTypeWebAuthn, SlotRef: "cred-1", WrappedKey: []byte{2}, Nonce: []byte{2},
	})
	require.NoError(t, err)
	require.False(t, bootstrapped, "must not insert a second slot once the user already has one")

	n, err := s.CountSlots(ctx, "archer")
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

// TestBootstrapSlotIfEmpty_AtomicAcrossSeparateStoreInstances is the
// multi-process regression test: two independent *Store instances (each
// with their own connection pool, exactly as two separate Miranda processes
// sharing the same keyring_sqlite_path would have — see docs/encryption.md
// and CLAUDE.md's MIRANDA_CONFIG_DIR second-instance pattern) opened
// against the SAME database file, racing to bootstrap the same brand-new
// user concurrently. Only one may ever succeed — an in-process-only lock
// (e.g. a sync.Mutex keyed by username) could never catch this, since each
// Store here has its own independent Go memory space; only a real
// transaction backed by SQLite's own file-level locking (_txlock=immediate,
// see Open) can.
func TestBootstrapSlotIfEmpty_AtomicAcrossSeparateStoreInstances(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "keyring.db")

	s1, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s1.Close() })

	s2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	const attempts = 20
	results := make(chan bool, 2*attempts)
	var wg sync.WaitGroup
	wg.Add(2 * attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			bootstrapped, err := s1.BootstrapSlotIfEmpty(ctx, "archer", Slot{
				SlotType: SlotTypePassword, WrappedKey: []byte{1}, Nonce: []byte{1},
			})
			require.NoError(t, err)
			results <- bootstrapped
		}()
		go func() {
			defer wg.Done()
			bootstrapped, err := s2.BootstrapSlotIfEmpty(ctx, "archer", Slot{
				SlotType: SlotTypeWebAuthn, SlotRef: "cred-1", WrappedKey: []byte{2}, Nonce: []byte{2},
			})
			require.NoError(t, err)
			results <- bootstrapped
		}()
	}
	wg.Wait()
	close(results)

	wins := 0
	for r := range results {
		if r {
			wins++
		}
	}
	require.Equal(t, 1, wins, "exactly one of the racing bootstrap attempts across two independent Store instances must win")

	n, err := s1.CountSlots(ctx, "archer")
	require.NoError(t, err)
	require.Equal(t, 1, n)
}
