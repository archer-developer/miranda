package keyring

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "keyring.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return NewService(store, NewCache())
}

func TestUnlockWithPassword_BootstrapsOnFirstLogin(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	require.NoError(t, svc.UnlockWithPassword(ctx, "archer", "hunter2"))

	key, ok := svc.Get("archer")
	require.True(t, ok)
	require.Len(t, key, 32)

	n, err := svc.store.CountSlots(ctx, "archer")
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func TestUnlockWithPassword_SecondLoginUnwrapsSameKey(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	require.NoError(t, svc.UnlockWithPassword(ctx, "archer", "hunter2"))
	key1, _ := svc.Get("archer")

	// Simulate a fresh process: a new Service sharing the same Store, cold cache.
	svc2 := NewService(svc.store, NewCache())
	require.NoError(t, svc2.UnlockWithPassword(ctx, "archer", "hunter2"))
	key2, ok := svc2.Get("archer")
	require.True(t, ok)

	require.Equal(t, key1, key2, "relogin must unwrap the same master key, not mint a new one")

	n, err := svc.store.CountSlots(ctx, "archer")
	require.NoError(t, err)
	require.Equal(t, 1, n, "must not have created a second password slot")
}

func TestUnlockWithPassword_WrongPasswordFails(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	require.NoError(t, svc.UnlockWithPassword(ctx, "archer", "hunter2"))

	svc2 := NewService(svc.store, NewCache())
	err := svc2.UnlockWithPassword(ctx, "archer", "wrong-password")
	require.Error(t, err)

	_, ok := svc2.Get("archer")
	require.False(t, ok)
}

func TestUnlockWithPRF_BootstrapsOnFirstLogin(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	credID := []byte("credential-1")
	prfOutput := make([]byte, 32)
	for i := range prfOutput {
		prfOutput[i] = byte(i)
	}

	require.NoError(t, svc.UnlockWithPRF(ctx, "archer", credID, prfOutput))

	key, ok := svc.Get("archer")
	require.True(t, ok)
	require.Len(t, key, 32)
}

func TestUnlockWithPRF_EmptyOutputIsNoop(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	require.NoError(t, svc.UnlockWithPRF(ctx, "archer", []byte("cred-1"), nil))

	_, ok := svc.Get("archer")
	require.False(t, ok)
	n, err := svc.store.CountSlots(ctx, "archer")
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

func TestAddPasskeySlot_RequiresWarmCache(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	err := svc.AddPasskeySlot(ctx, "archer", []byte("cred-1"), make([]byte, 32))
	require.ErrorIs(t, err, ErrNotUnlocked)
}

func TestAddPasskeySlot_WrapsAlreadyUnlockedKey_NeverMintsSecondKey(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	// Bootstrap via password.
	require.NoError(t, svc.UnlockWithPassword(ctx, "archer", "hunter2"))
	passwordKey, _ := svc.Get("archer")

	// Add a passkey while already logged in.
	credID := []byte("credential-1")
	prfOutput := make([]byte, 32)
	for i := range prfOutput {
		prfOutput[i] = byte(i + 1)
	}
	require.NoError(t, svc.AddPasskeySlot(ctx, "archer", credID, prfOutput))

	n, err := svc.store.CountSlots(ctx, "archer")
	require.NoError(t, err)
	require.Equal(t, 2, n, "password slot + new webauthn slot")

	// Logging in via the new passkey must unwrap the SAME key, not a new one.
	svc2 := NewService(svc.store, NewCache())
	require.NoError(t, svc2.UnlockWithPRF(ctx, "archer", credID, prfOutput))
	passkeyKey, ok := svc2.Get("archer")
	require.True(t, ok)
	require.Equal(t, passwordKey, passkeyKey, "must unwrap the same master key added via AddPasskeySlot, not mint a second one")
}

func TestUnlockWithPassword_NoopWhenOnlyOtherSlotExists(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	// Bootstrap via PRF only — no password slot ever created.
	credID := []byte("credential-1")
	prfOutput := make([]byte, 32)
	require.NoError(t, svc.UnlockWithPRF(ctx, "archer", credID, prfOutput))

	svc2 := NewService(svc.store, NewCache())
	require.NoError(t, svc2.UnlockWithPassword(ctx, "archer", "hunter2"))

	_, ok := svc2.Get("archer")
	require.False(t, ok, "password login must not mint a second master key when a webauthn slot already owns one")

	n, err := svc.store.CountSlots(ctx, "archer")
	require.NoError(t, err)
	require.Equal(t, 1, n, "must still only have the one webauthn slot")
}

func TestRemoveSlotForCredential_DropsOnlyThatSlot(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	credID := []byte("credential-1")
	require.NoError(t, svc.UnlockWithPRF(ctx, "archer", credID, make([]byte, 32)))

	require.NoError(t, svc.RemoveSlotForCredential(ctx, "archer", credID))

	n, err := svc.store.CountSlots(ctx, "archer")
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

// TestBootstrap_ConcurrentUnlocksNeverMintTwoMasterKeys guards the fix for a
// real race: a password login and a PRF login racing on the same brand-new
// user's very first-ever login (e.g. two browser tabs, or a retried
// request) must never both observe "no slots yet" and each mint their own
// master key — see unlockOrBootstrap's per-username lock. Run with -race.
func TestBootstrap_ConcurrentUnlocksNeverMintTwoMasterKeys(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	credID := []byte("credential-1")
	prfOutput := make([]byte, 32)
	for i := range prfOutput {
		prfOutput[i] = byte(i)
	}

	const attempts = 50
	var wg sync.WaitGroup
	wg.Add(2 * attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			_ = svc.UnlockWithPassword(ctx, "archer", "hunter2")
		}()
		go func() {
			defer wg.Done()
			_ = svc.UnlockWithPRF(ctx, "archer", credID, prfOutput)
		}()
	}
	wg.Wait()

	// Whichever unlock method's goroutine happened to win the race gets to
	// bootstrap — but only ONE of them ever should. Before the fix, both
	// UnlockWithPassword and UnlockWithPRF could independently observe "no
	// slots yet" and each mint their own master key, leaving two slots
	// wrapping two unrelated keys; a concurrent unlock attempt of the
	// *other* type is correctly a no-op once a slot already exists (see
	// unlockOrBootstrap — enrolling a second unlock method requires an
	// already-warm cache and AddPasskeySlot, not a racing bootstrap call).
	n, err := svc.store.CountSlots(ctx, "archer")
	require.NoError(t, err)
	require.Equal(t, 1, n, "exactly one slot must have bootstrapped — never two independently minted master keys")

	// Whichever method won, repeating it afterward must keep unwrapping to
	// the same persisted key, never mint a second one on a later call.
	svcPassword := NewService(svc.store, NewCache())
	require.NoError(t, svcPassword.UnlockWithPassword(ctx, "archer", "hunter2"))
	_, passwordUnlocked := svcPassword.Get("archer")

	svcPRF := NewService(svc.store, NewCache())
	require.NoError(t, svcPRF.UnlockWithPRF(ctx, "archer", credID, prfOutput))
	_, prfUnlocked := svcPRF.Get("archer")

	require.True(t, passwordUnlocked || prfUnlocked, "whichever method bootstrapped the slot must still be able to unlock it afterward")
	require.False(t, passwordUnlocked && prfUnlocked, "only the slot type that actually won the race has a slot — the other method must not also unlock")
}
