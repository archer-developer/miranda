// Package keyring implements optional per-user data encryption: one random
// master key K per user, wrapped independently under every "authenticator"
// that should be able to unlock it (a WebAuthn passkey's PRF output, or a
// password-derived KDF output as a fallback) — the same key-wrapping model
// 1Password/FileVault/BitLocker use. Store persists the wrapped copies
// (this file), crypto.go implements the AES-GCM wrap/unwrap and Argon2id
// KDF primitives, Cache holds each user's unwrapped K in memory only for
// the life of the process, and Service (the only thing internal/webui and
// internal/httpapi should call) orchestrates the mint-vs-unwrap branching
// between them. See docs/encryption.md for the full design and threat
// model.
package keyring

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// Slot type constants — one row per (username, slot type, slot ref) in
// keyring_slots.
const (
	SlotTypeWebAuthn = "webauthn"
	SlotTypePassword = "password"
)

// Slot is one wrapped copy of a user's master key, keyed by which
// authenticator can unwrap it. SlotRef is a WebAuthn credential's
// base64url-encoded id for SlotTypeWebAuthn, or "" for the single
// SlotTypePassword slot. KDFSalt/KDFParams are only set for password slots.
type Slot struct {
	Username   string
	SlotType   string
	SlotRef    string
	WrappedKey []byte
	Nonce      []byte
	KDFSalt    []byte
	KDFParams  string
}

// Store is a SQLite-backed persistence layer for wrapped-key slots, mirroring
// internal/webauthn/store.go's Open/migrate pattern. Kept in its own
// database file (config.StorageConfig.KeyringSQLitePath) — unlike Miranda's
// other optional SQLite files, losing this one is data-loss-equivalent to
// losing every piece of data encrypted under it, so it deserves its own
// backup consideration, not just isolation from dialog history.
type Store struct {
	db *sql.DB
}

// Open creates (if needed) and opens the SQLite database at path, applying
// the schema.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("keyring: create dir %s: %w", dir, err)
		}
	}

	// _txlock=immediate makes every sql.Tx started via db.BeginTx acquire
	// SQLite's write lock immediately at BEGIN time (a real file-level lock,
	// enforced by SQLite itself) rather than the driver's default deferred
	// behavior, which only upgrades to a write lock on the transaction's
	// first write statement — see BootstrapSlotIfEmpty below, whose
	// check-then-insert sequence needs the lock held for the whole
	// transaction to be atomic, including against a second Miranda process
	// sharing this same database file (e.g. a MIRANDA_CONFIG_DIR-based
	// second local instance — see CLAUDE.md's Telegram section for that
	// documented pattern), not just other goroutines in this one process.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("keyring: open %s: %w", path, err)
	}
	// SQLite only supports one writer at a time; a single connection avoids
	// SQLITE_BUSY errors from this process racing against itself, and
	// (combined with BootstrapSlotIfEmpty's real transaction) is what makes
	// a read like GetSlot that lands mid-bootstrap see either the
	// pre-bootstrap or post-bootstrap state, never a torn one.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS keyring_slots (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			username    TEXT NOT NULL,
			slot_type   TEXT NOT NULL CHECK (slot_type IN ('webauthn','password')),
			slot_ref    TEXT NOT NULL DEFAULT '',
			wrapped_key BLOB NOT NULL,
			nonce       BLOB NOT NULL,
			kdf_salt    BLOB,
			kdf_params  TEXT,
			created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_keyring_slots_identity ON keyring_slots(username, slot_type, slot_ref)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("keyring: migrate: %w", err)
		}
	}
	return nil
}

// PutSlot inserts or replaces the wrapped key for (username, slot type, slot
// ref) — an upsert on the identity unique index, so re-wrapping (e.g. a
// future "change password" flow re-wrapping the password slot) never
// accumulates duplicate rows.
func (s *Store) PutSlot(ctx context.Context, slot Slot) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO keyring_slots (username, slot_type, slot_ref, wrapped_key, nonce, kdf_salt, kdf_params, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		ON CONFLICT(username, slot_type, slot_ref) DO UPDATE SET
			wrapped_key = excluded.wrapped_key,
			nonce = excluded.nonce,
			kdf_salt = excluded.kdf_salt,
			kdf_params = excluded.kdf_params,
			updated_at = excluded.updated_at`,
		slot.Username, slot.SlotType, slot.SlotRef, slot.WrappedKey, slot.Nonce, slot.KDFSalt, slot.KDFParams,
	)
	if err != nil {
		return fmt.Errorf("keyring: put slot: %w", err)
	}
	return nil
}

// BootstrapSlotIfEmpty atomically checks whether username has any slot at
// all (of any type) and, only if not, inserts slot as the very first one —
// enforcing the single-mint invariant (see internal/keyring/service.go's
// Service doc comment) as a real SQLite transaction rather than a
// check-then-act pair of separate calls. Because Open sets _txlock=immediate,
// this transaction takes SQLite's write lock at BEGIN time, so it's atomic
// even against a second Miranda process sharing this same database file —
// not just other goroutines in this one process (contrast with a purely
// in-process lock, which can't see a concurrent process at all). Returns
// bootstrapped=false, without inserting anything, if the user already has
// at least one slot — the caller (Service.unlockOrBootstrap) should treat
// that exactly like GetSlot finding nothing: this particular unlock method
// still has no slot of its own, but no error occurred.
func (s *Store) BootstrapSlotIfEmpty(ctx context.Context, username string, slot Slot) (bootstrapped bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("keyring: begin bootstrap transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit has succeeded

	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM keyring_slots WHERE username = ?`, username).Scan(&n); err != nil {
		return false, fmt.Errorf("keyring: count slots: %w", err)
	}
	if n > 0 {
		return false, nil
	}

	// Insert under the username parameter just checked above, not
	// slot.Username — the two are redundant by construction (Slot also
	// carries a Username field, used by PutSlot/GetSlot's simpler
	// single-statement callers), and a caller that only fills in slot.Type/
	// Ref/WrappedKey/Nonce here (the common case: BootstrapSlotIfEmpty is
	// the one Store method whose identity is already fully pinned down by
	// its own username argument) would otherwise silently insert an
	// empty/mismatched username, defeating the very COUNT(*) check above.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO keyring_slots (username, slot_type, slot_ref, wrapped_key, nonce, kdf_salt, kdf_params, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		username, slot.SlotType, slot.SlotRef, slot.WrappedKey, slot.Nonce, slot.KDFSalt, slot.KDFParams,
	); err != nil {
		return false, fmt.Errorf("keyring: insert bootstrap slot: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("keyring: commit bootstrap transaction: %w", err)
	}
	return true, nil
}

// GetSlot returns the wrapped-key slot for (username, slotType, slotRef), if
// one exists.
func (s *Store) GetSlot(ctx context.Context, username, slotType, slotRef string) (Slot, bool, error) {
	var slot Slot
	var kdfParams sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT wrapped_key, nonce, kdf_salt, kdf_params
		FROM keyring_slots WHERE username = ? AND slot_type = ? AND slot_ref = ?`,
		username, slotType, slotRef,
	).Scan(&slot.WrappedKey, &slot.Nonce, &slot.KDFSalt, &kdfParams)
	if err == sql.ErrNoRows {
		return Slot{}, false, nil
	}
	if err != nil {
		return Slot{}, false, fmt.Errorf("keyring: get slot: %w", err)
	}
	slot.Username, slot.SlotType, slot.SlotRef = username, slotType, slotRef
	slot.KDFParams = kdfParams.String
	return slot, true, nil
}

// CountSlots returns how many wrapped-key slots (of any type) username has.
// Service uses this to decide whether an unlock attempt should mint a fresh
// master key (zero slots — first-ever unlock) or leave things locked
// (nonzero slots, but none matching the method being tried) — see
// docs/encryption.md's single-mint invariant.
func (s *Store) CountSlots(ctx context.Context, username string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM keyring_slots WHERE username = ?`, username).Scan(&n); err != nil {
		return 0, fmt.Errorf("keyring: count slots: %w", err)
	}
	return n, nil
}

// DeleteSlot removes one slot — used when a passkey is deleted, so its
// now-orphaned wrapped-key slot doesn't linger.
func (s *Store) DeleteSlot(ctx context.Context, username, slotType, slotRef string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM keyring_slots WHERE username = ? AND slot_type = ? AND slot_ref = ?`,
		username, slotType, slotRef); err != nil {
		return fmt.Errorf("keyring: delete slot: %w", err)
	}
	return nil
}
