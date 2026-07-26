// Package webauthn implements optional FIDO2/passkey ("biometric") login:
// persisting credentials (this file), bridging internal/users.User to the
// github.com/go-webauthn/webauthn library's User interface (adapter.go),
// holding transient ceremony state between a registration/login's begin and
// finish calls (ceremony.go), and orchestrating the actual ceremonies
// (service.go) for internal/webui's HTTP handlers to call.
//
// WebAuthn only works in a secure browser context (HTTPS, or
// http://localhost) — see config.WebAuthnConfig's doc comment. This package
// itself has no opinion on that; it just implements the relying-party
// protocol correctly once TLS is in place in front of Miranda.
package webauthn

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	webauthnlib "github.com/go-webauthn/webauthn/webauthn"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// userHandleBytes is the length of the random per-(rpid,username) User
// Handle stored in webauthn_users — the library recommends using the full
// 64 bytes it allows; we use 32, which is already far more entropy than
// needed to prevent guessing and matches internal/session's token size.
const userHandleBytes = 32

// Store is a SQLite-backed credential store, mirroring internal/history's
// Open/migrate pattern. Kept in its own database file (see
// config.StorageConfig.WebAuthnSQLitePath) so wiping/backing up dialog
// history never touches registered passkeys, and vice versa.
type Store struct {
	db *sql.DB
}

// Open creates (if needed) and opens the SQLite database at path, applying
// the schema.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("webauthn: create dir %s: %w", dir, err)
		}
	}

	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("webauthn: open %s: %w", path, err)
	}
	// SQLite only supports one writer at a time; a single connection avoids
	// SQLITE_BUSY errors from this process racing against itself.
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
		// One row per (RPID, username): the stable, random WebAuthn "user
		// handle" the library's User.WebAuthnID() must always return for
		// that person. Never derived from the username — see adapter.go.
		`CREATE TABLE IF NOT EXISTS webauthn_users (
			rpid       TEXT NOT NULL,
			username   TEXT NOT NULL,
			handle     BLOB NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (rpid, username)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_webauthn_users_handle ON webauthn_users(rpid, handle)`,
		// One row per registered credential. Every field of
		// webauthnlib.Credential (including its nested Authenticator and
		// CredentialAttestation) gets its own column — the library's own
		// "Storage" doc section recommends this explicit-fields shape over
		// an opaque blob so individual values stay queryable/auditable.
		`CREATE TABLE IF NOT EXISTS webauthn_credentials (
			id                   INTEGER PRIMARY KEY AUTOINCREMENT,
			rpid                 TEXT NOT NULL,
			username             TEXT NOT NULL,
			credential_id        BLOB NOT NULL,
			nickname             TEXT NOT NULL DEFAULT '',
			public_key           BLOB NOT NULL,
			attestation_type     TEXT NOT NULL DEFAULT '',
			attestation_format   TEXT NOT NULL DEFAULT '',
			transports           TEXT NOT NULL DEFAULT '',
			aaguid               BLOB,
			sign_count           INTEGER NOT NULL DEFAULT 0,
			clone_warning        INTEGER NOT NULL DEFAULT 0,
			attachment           TEXT NOT NULL DEFAULT '',
			flags                INTEGER NOT NULL DEFAULT 0,
			client_data_json     BLOB,
			client_data_hash     BLOB,
			authenticator_data   BLOB,
			public_key_algorithm INTEGER NOT NULL DEFAULT 0,
			attestation_object   BLOB,
			created_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_used_at         DATETIME
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_webauthn_credentials_kid ON webauthn_credentials(rpid, credential_id)`,
		`CREATE INDEX IF NOT EXISTS idx_webauthn_credentials_user ON webauthn_credentials(rpid, username)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("webauthn: migrate: %w", err)
		}
	}
	return nil
}

// EnsureUserHandle returns username's stable WebAuthn user handle under
// rpid, generating and persisting a fresh random one on first call. Safe to
// call every time a ceremony begins — it's a no-op after the first success.
func (s *Store) EnsureUserHandle(ctx context.Context, rpid, username string) ([]byte, error) {
	if handle, ok, err := s.UserHandle(ctx, rpid, username); err != nil {
		return nil, err
	} else if ok {
		return handle, nil
	}

	handle := make([]byte, userHandleBytes)
	if _, err := rand.Read(handle); err != nil {
		return nil, fmt.Errorf("webauthn: generate user handle: %w", err)
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO webauthn_users (rpid, username, handle) VALUES (?, ?, ?)
		 ON CONFLICT(rpid, username) DO NOTHING`,
		rpid, username, handle); err != nil {
		return nil, fmt.Errorf("webauthn: insert user handle: %w", err)
	}

	// Another goroutine may have won the insert race; re-read rather than
	// trust the handle we generated, so every caller agrees on one value.
	handle, ok, err := s.UserHandle(ctx, rpid, username)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("webauthn: user handle for %s missing immediately after insert", username)
	}
	return handle, nil
}

// UserHandle returns username's stored WebAuthn user handle under rpid, if
// one has been created yet.
func (s *Store) UserHandle(ctx context.Context, rpid, username string) ([]byte, bool, error) {
	var handle []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT handle FROM webauthn_users WHERE rpid = ? AND username = ?`, rpid, username).Scan(&handle)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("webauthn: query user handle: %w", err)
	}
	return handle, true, nil
}

// SaveCredential persists a newly registered credential for username under
// rpid, with an admin/user-supplied nickname (e.g. "iPhone") for display.
func (s *Store) SaveCredential(ctx context.Context, rpid, username, nickname string, cred *webauthnlib.Credential) error {
	transports := make([]string, len(cred.Transport))
	for i, t := range cred.Transport {
		transports[i] = string(t)
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO webauthn_credentials (
			rpid, username, credential_id, nickname, public_key,
			attestation_type, attestation_format, transports, aaguid,
			sign_count, clone_warning, attachment, flags,
			client_data_json, client_data_hash, authenticator_data,
			public_key_algorithm, attestation_object
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rpid, username, cred.ID, nickname, cred.PublicKey,
		cred.AttestationType, cred.AttestationFormat, strings.Join(transports, ","), cred.Authenticator.AAGUID,
		cred.Authenticator.SignCount, cred.Authenticator.CloneWarning, string(cred.Authenticator.Attachment), cred.Flags.MsgpByte(),
		cred.Attestation.ClientDataJSON, cred.Attestation.ClientDataHash, cred.Attestation.AuthenticatorData,
		cred.Attestation.PublicKeyAlgorithm, cred.Attestation.Object,
	)
	if err != nil {
		return fmt.Errorf("webauthn: save credential: %w", err)
	}
	return nil
}

// CredentialsForUser returns every credential registered for username under
// rpid, reconstructed as the library's own Credential type — this is what
// backs the User.WebAuthnCredentials() adapter method.
func (s *Store) CredentialsForUser(ctx context.Context, rpid, username string) ([]webauthnlib.Credential, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT credential_id, public_key, attestation_type, attestation_format, transports, aaguid,
		       sign_count, clone_warning, attachment, flags,
		       client_data_json, client_data_hash, authenticator_data, public_key_algorithm, attestation_object
		FROM webauthn_credentials WHERE rpid = ? AND username = ?`, rpid, username)
	if err != nil {
		return nil, fmt.Errorf("webauthn: query credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []webauthnlib.Credential
	for rows.Next() {
		cred, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cred)
	}
	return out, rows.Err()
}

// CredentialInfo is a display-safe view of a registered credential for the
// profile screen's "manage passkeys" list — deliberately excludes the
// public key and attestation blobs. ID is base64url (not sensitive — it's a
// public identifier, not key material) so the frontend can round-trip it
// straight back on a delete request with no extra decoding step.
type CredentialInfo struct {
	ID         string `json:"id"`
	Nickname   string `json:"nickname"`
	Transports string `json:"transports"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
}

// ListForUser returns display-safe info for every credential username has
// registered under rpid, most recently created first.
func (s *Store) ListForUser(ctx context.Context, rpid, username string) ([]CredentialInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT credential_id, nickname, transports, created_at, COALESCE(last_used_at, '')
		FROM webauthn_credentials WHERE rpid = ? AND username = ?
		ORDER BY created_at DESC, id DESC`, rpid, username)
	if err != nil {
		return nil, fmt.Errorf("webauthn: list credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CredentialInfo
	for rows.Next() {
		var info CredentialInfo
		var id []byte
		if err := rows.Scan(&id, &info.Nickname, &info.Transports, &info.CreatedAt, &info.LastUsedAt); err != nil {
			return nil, fmt.Errorf("webauthn: scan credential info: %w", err)
		}
		info.ID = base64.RawURLEncoding.EncodeToString(id)
		out = append(out, info)
	}
	return out, rows.Err()
}

// DeleteCredential removes one credential, scoped by username so a user can
// never delete another account's credential even if they guess/forge an id.
func (s *Store) DeleteCredential(ctx context.Context, rpid, username string, credentialID []byte) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM webauthn_credentials WHERE rpid = ? AND username = ? AND credential_id = ?`,
		rpid, username, credentialID)
	if err != nil {
		return fmt.Errorf("webauthn: delete credential: %w", err)
	}
	return nil
}

// LookupByCredentialID returns which username owns rawID under rpid — the
// lookup a discoverable/passkey login needs, since the assertion identifies
// itself by credential id before the Relying Party knows who's logging in.
func (s *Store) LookupByCredentialID(ctx context.Context, rpid string, rawID []byte) (username string, found bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT username FROM webauthn_credentials WHERE rpid = ? AND credential_id = ?`, rpid, rawID).Scan(&username)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("webauthn: lookup credential owner: %w", err)
	}
	return username, true, nil
}

// UpdateSignCount writes back the fields the library mutates on every
// successful login (sign_count, clone_warning, flags — notably BackupState
// can change over the life of a credential) but never persists itself. This
// must be called after every login or clone detection silently stops
// working.
func (s *Store) UpdateSignCount(ctx context.Context, rpid string, credentialID []byte, signCount uint32, cloneWarning bool, flags webauthnlib.CredentialFlags) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE webauthn_credentials
		SET sign_count = ?, clone_warning = ?, flags = ?, last_used_at = CURRENT_TIMESTAMP
		WHERE rpid = ? AND credential_id = ?`,
		signCount, cloneWarning, flags.MsgpByte(), rpid, credentialID)
	if err != nil {
		return fmt.Errorf("webauthn: update sign count: %w", err)
	}
	return nil
}

func scanCredential(rows *sql.Rows) (webauthnlib.Credential, error) {
	var cred webauthnlib.Credential
	var transports string
	var attachment string
	var flagsByte byte

	if err := rows.Scan(
		&cred.ID, &cred.PublicKey, &cred.AttestationType, &cred.AttestationFormat, &transports, &cred.Authenticator.AAGUID,
		&cred.Authenticator.SignCount, &cred.Authenticator.CloneWarning, &attachment, &flagsByte,
		&cred.Attestation.ClientDataJSON, &cred.Attestation.ClientDataHash, &cred.Attestation.AuthenticatorData,
		&cred.Attestation.PublicKeyAlgorithm, &cred.Attestation.Object,
	); err != nil {
		return webauthnlib.Credential{}, fmt.Errorf("webauthn: scan credential: %w", err)
	}

	cred.Authenticator.Attachment = protocol.AuthenticatorAttachment(attachment)
	cred.Flags = webauthnlib.CredentialFlagsFromMsgpByte(flagsByte)
	if transports != "" {
		for _, t := range strings.Split(transports, ",") {
			cred.Transport = append(cred.Transport, protocol.AuthenticatorTransport(t))
		}
	}
	return cred, nil
}
