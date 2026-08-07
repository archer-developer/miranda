package keyring

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
)

// ErrNotUnlocked is returned by AddPasskeySlot when the caller's master key
// isn't currently unlocked in the Cache — see Service's doc comment for why
// this deliberately never mints a fresh key itself in that case.
var ErrNotUnlocked = errors.New("keyring: master key not unlocked")

// Service is the only thing internal/webui and internal/httpapi should call
// — it owns the mint-vs-unwrap branching so it's never duplicated across
// call sites. Critical invariant: a fresh master key is only ever minted
// when a user has zero existing slots of any type. Every other unlock path
// only ever tries to unwrap an existing slot of its own type and otherwise
// no-ops — if two different unlock methods each independently minted a key
// whenever their own slot was missing, a user could end up with two
// unrelated master keys wrapped under different authenticators, silently
// splitting their encrypted data. unlockOrBootstrap enforces this by
// delegating the check-then-insert to Store.BootstrapSlotIfEmpty, a real
// SQLite transaction — not an in-process lock — so the invariant holds even
// across two Miranda processes sharing the same database file, not just
// concurrent goroutines within one. See docs/encryption.md for the full
// design.
type Service struct {
	store *Store
	cache *Cache
}

// NewService builds a Service over store (persistence) and cache (the
// in-memory unlocked-key cache).
func NewService(store *Store, cache *Cache) *Service {
	return &Service{store: store, cache: cache}
}

// slotKeyDeriver computes the wrapping key for one unlock attempt against
// one slot type — the only thing that actually differs between
// UnlockWithPassword and UnlockWithPRF; everything else about the
// found-vs-bootstrap branching is identical and lives once in
// unlockOrBootstrap.
type slotKeyDeriver interface {
	// forExisting returns the wrap key to unwrap an already-persisted slot.
	forExisting(slot Slot) ([]byte, error)
	// forBootstrap returns the wrap key for a brand-new slot, plus any extra
	// Slot fields (e.g. a password slot's freshly generated KDF salt/params)
	// that must be persisted alongside it.
	forBootstrap() (key []byte, extra Slot, err error)
}

type passwordDeriver struct{ password string }

func (d passwordDeriver) forExisting(slot Slot) ([]byte, error) {
	params, err := parseKDFParams(slot.KDFParams)
	if err != nil {
		return nil, err
	}
	return deriveFromPassword(d.password, slot.KDFSalt, params), nil
}

func (d passwordDeriver) forBootstrap() ([]byte, Slot, error) {
	salt, err := newSalt()
	if err != nil {
		return nil, Slot{}, err
	}
	derived := deriveFromPassword(d.password, salt, defaultKDFParams())
	return derived, Slot{KDFSalt: salt, KDFParams: currentKDFParams()}, nil
}

type prfDeriver struct{ output []byte }

func (d prfDeriver) forExisting(Slot) ([]byte, error)    { return d.output, nil }
func (d prfDeriver) forBootstrap() ([]byte, Slot, error) { return d.output, Slot{}, nil }

// unlockOrBootstrap implements the single-mint invariant for one unlock
// attempt against one (slotType, slotRef): if that slot already exists,
// unwrap it and cache the result; if it doesn't, try to bootstrap it — which
// only actually mints and persists a fresh master key if
// Store.BootstrapSlotIfEmpty finds the user still has zero slots of any
// type at that exact moment (atomically, via a real transaction), and
// otherwise no-ops, leaving this unlock method still unable to reach
// whatever other slot already owns the key.
func (svc *Service) unlockOrBootstrap(ctx context.Context, username, slotType, slotRef string, deriver slotKeyDeriver) error {
	slot, ok, err := svc.store.GetSlot(ctx, username, slotType, slotRef)
	if err != nil {
		return err
	}
	if ok {
		wrapKey, err := deriver.forExisting(slot)
		if err != nil {
			return err
		}
		key, err := unwrap(wrapKey, slot.WrappedKey, slot.Nonce, slotAAD(username, slotType, slotRef))
		if err != nil {
			return fmt.Errorf("keyring: unwrap %s slot for %s: %w", slotType, username, err)
		}
		svc.cache.Unlock(username, key)
		return nil
	}

	wrapKey, extra, err := deriver.forBootstrap()
	if err != nil {
		return err
	}
	key, err := GenerateMasterKey()
	if err != nil {
		return err
	}
	wrapped, nonce, err := wrap(wrapKey, key, slotAAD(username, slotType, slotRef))
	if err != nil {
		return err
	}
	extra.Username, extra.SlotType, extra.SlotRef = username, slotType, slotRef
	extra.WrappedKey, extra.Nonce = wrapped, nonce

	bootstrapped, err := svc.store.BootstrapSlotIfEmpty(ctx, username, extra)
	if err != nil {
		return err
	}
	if !bootstrapped {
		// Lost the bootstrap race to some other slot type (possibly from a
		// different Miranda process sharing this database file) — not an
		// error, this unlock method just still has no slot of its own.
		return nil
	}
	svc.cache.Unlock(username, key)
	return nil
}

// UnlockWithPassword is called from the login handler while the plaintext
// password is still in scope — it is never persisted, only used here to
// derive a wrapping key via Argon2id. See unlockOrBootstrap for the
// found/bootstrap branching this shares with UnlockWithPRF.
func (svc *Service) UnlockWithPassword(ctx context.Context, username, password string) error {
	return svc.unlockOrBootstrap(ctx, username, SlotTypePassword, "", passwordDeriver{password: password})
}

// UnlockWithPRF is the WebAuthn PRF analogue of UnlockWithPassword, keyed by
// credential id instead of "password". prfOutput may legitimately be empty
// (the authenticator or browser doesn't support the PRF extension) — that's
// not an error, just a no-op leaving the cache as it was.
func (svc *Service) UnlockWithPRF(ctx context.Context, username string, credentialID, prfOutput []byte) error {
	if len(prfOutput) == 0 {
		return nil
	}
	slotRef := slotRefForCredential(credentialID)
	return svc.unlockOrBootstrap(ctx, username, SlotTypeWebAuthn, slotRef, prfDeriver{output: prfOutput})
}

// AddPasskeySlot wraps the caller's already-unlocked master key under a
// newly registered credential's PRF output. Deliberately never mints a
// fresh key itself — it requires the Cache to already be warm (ErrNotUnlocked
// otherwise) — so there is exactly one bootstrap path (unlockOrBootstrap,
// via UnlockWithPassword/UnlockWithPRF above), never a race between "just
// registered a passkey" and "just logged in" each independently minting a
// key. Safe without its own transaction: it never mints, only wraps a key
// that (by the time the cache is warm) already has at least one persisted
// slot, so it can't collide with a concurrent bootstrap for a different slot
// type.
func (svc *Service) AddPasskeySlot(ctx context.Context, username string, credentialID, prfOutput []byte) error {
	key, ok := svc.cache.Get(username)
	if !ok {
		return ErrNotUnlocked
	}
	slotRef := slotRefForCredential(credentialID)
	wrapped, nonce, err := wrap(prfOutput, key, slotAAD(username, SlotTypeWebAuthn, slotRef))
	if err != nil {
		return err
	}
	return svc.store.PutSlot(ctx, Slot{
		Username: username, SlotType: SlotTypeWebAuthn, SlotRef: slotRef,
		WrappedKey: wrapped, Nonce: nonce,
	})
}

// Lock discards username's cached unlocked key — called on explicit logout.
func (svc *Service) Lock(username string) {
	svc.cache.Lock(username)
}

// Get returns username's unwrapped master key, if currently unlocked. Used
// by internal/httpapi's tool-call injection.
func (svc *Service) Get(username string) ([]byte, bool) {
	return svc.cache.Get(username)
}

// RemoveSlotForCredential drops credentialID's wrapped-key slot — called
// when the matching passkey itself is deleted, so it doesn't linger
// orphaned.
func (svc *Service) RemoveSlotForCredential(ctx context.Context, username string, credentialID []byte) error {
	return svc.store.DeleteSlot(ctx, username, SlotTypeWebAuthn, slotRefForCredential(credentialID))
}

func slotRefForCredential(credentialID []byte) string {
	return base64.RawURLEncoding.EncodeToString(credentialID)
}
