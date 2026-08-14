package oauth2

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/archer-developer/miranda/internal/keyring"
)

// Token is one user's decrypted OAuth2 state for one provider — never
// persisted or logged in this shape; Store only ever accepts/returns it
// through PutToken/GetToken, which wrap/unwrap AccessToken/RefreshToken
// under the server's master key.
type Token struct {
	Username, Provider        string
	AccessToken, RefreshToken string // RefreshToken may be "" — some refresh responses omit it, see client.go
	Scope                     string
	Expiry                    time.Time
}

// Store is a SQLite-backed persistence layer for encrypted OAuth2 tokens,
// mirroring internal/keyring/store.go's exact Open/migrate/upsert pattern.
// Kept in its own database file (config.StorageConfig.OAuthSQLitePath) for
// the same isolation reasoning as keyring's own store: losing it is
// data-loss-equivalent to every household member having to re-authorize
// every OAuth-gated MCP server.
type Store struct {
	db *sql.DB
}

// Open creates (if needed) and opens the SQLite database at path, applying
// the schema.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("oauth2: create dir %s: %w", dir, err)
		}
	}

	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("oauth2: open %s: %w", path, err)
	}
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
		`CREATE TABLE IF NOT EXISTS oauth_tokens (
			id                  INTEGER PRIMARY KEY AUTOINCREMENT,
			username            TEXT NOT NULL,
			provider            TEXT NOT NULL,
			access_token_enc    BLOB NOT NULL,
			access_token_nonce  BLOB NOT NULL,
			refresh_token_enc   BLOB,
			refresh_token_nonce BLOB,
			scope               TEXT NOT NULL DEFAULT '',
			expiry              TEXT NOT NULL,
			updated_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_oauth_tokens_identity ON oauth_tokens(username, provider)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("oauth2: migrate: %w", err)
		}
	}
	return nil
}

// tokenAAD binds a wrapped token field to its own row identity (username,
// provider, field name) as AES-GCM associated data — same reasoning as
// internal/keyring's slotAAD: without this, a spliced ciphertext from a
// different row or field would decrypt undetected given the right key;
// with AAD bound to identity, Unwrap fails loudly instead.
func tokenAAD(username, provider, field string) []byte {
	return []byte(username + "|" + provider + "|" + field)
}

// PutToken upserts on (username, provider), wrapping AccessToken/
// RefreshToken independently under masterKey. A "" RefreshToken must NOT
// overwrite a previously stored refresh token — some refresh responses omit
// it (see client.go's RefreshAccessToken doc comment); callers merge the
// old value in before calling PutToken, which persists whatever it's given.
func (s *Store) PutToken(ctx context.Context, masterKey []byte, t Token) error {
	accessEnc, accessNonce, err := keyring.Wrap(masterKey, []byte(t.AccessToken), tokenAAD(t.Username, t.Provider, "access"))
	if err != nil {
		return fmt.Errorf("oauth2: wrap access token: %w", err)
	}

	var refreshEnc, refreshNonce any
	if t.RefreshToken != "" {
		enc, nonce, err := keyring.Wrap(masterKey, []byte(t.RefreshToken), tokenAAD(t.Username, t.Provider, "refresh"))
		if err != nil {
			return fmt.Errorf("oauth2: wrap refresh token: %w", err)
		}
		refreshEnc, refreshNonce = enc, nonce
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO oauth_tokens (username, provider, access_token_enc, access_token_nonce, refresh_token_enc, refresh_token_nonce, scope, expiry, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		ON CONFLICT(username, provider) DO UPDATE SET
			access_token_enc = excluded.access_token_enc,
			access_token_nonce = excluded.access_token_nonce,
			refresh_token_enc = excluded.refresh_token_enc,
			refresh_token_nonce = excluded.refresh_token_nonce,
			scope = excluded.scope,
			expiry = excluded.expiry,
			updated_at = excluded.updated_at`,
		t.Username, t.Provider, accessEnc, accessNonce, refreshEnc, refreshNonce, t.Scope, t.Expiry.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("oauth2: put token: %w", err)
	}
	return nil
}

// GetToken unwraps and returns username's stored token for provider, if any.
func (s *Store) GetToken(ctx context.Context, masterKey []byte, username, provider string) (Token, bool, error) {
	var accessEnc, accessNonce []byte
	var refreshEnc, refreshNonce sql.NullString
	var scope, expiry string
	err := s.db.QueryRowContext(ctx, `
		SELECT access_token_enc, access_token_nonce, refresh_token_enc, refresh_token_nonce, scope, expiry
		FROM oauth_tokens WHERE username = ? AND provider = ?`,
		username, provider,
	).Scan(&accessEnc, &accessNonce, &refreshEnc, &refreshNonce, &scope, &expiry)
	if err == sql.ErrNoRows {
		return Token{}, false, nil
	}
	if err != nil {
		return Token{}, false, fmt.Errorf("oauth2: get token: %w", err)
	}

	t, err := s.decodeToken(username, provider, accessEnc, accessNonce, []byte(refreshEnc.String), []byte(refreshNonce.String), refreshEnc.Valid, scope, expiry, masterKey)
	if err != nil {
		return Token{}, false, err
	}
	return t, true, nil
}

// ListDueForRefresh returns every stored token whose Expiry is within margin
// of now and that has a non-empty refresh token — the query the background
// refresher polls on each tick.
func (s *Store) ListDueForRefresh(ctx context.Context, masterKey []byte, margin time.Duration) ([]Token, error) {
	cutoff := time.Now().Add(margin).UTC().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `
		SELECT username, provider, access_token_enc, access_token_nonce, refresh_token_enc, refresh_token_nonce, scope, expiry
		FROM oauth_tokens WHERE expiry <= ? AND refresh_token_enc IS NOT NULL`,
		cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("oauth2: list due for refresh: %w", err)
	}
	defer rows.Close()

	var out []Token
	for rows.Next() {
		var username, provider, scope, expiry string
		var accessEnc, accessNonce []byte
		var refreshEnc, refreshNonce sql.NullString
		if err := rows.Scan(&username, &provider, &accessEnc, &accessNonce, &refreshEnc, &refreshNonce, &scope, &expiry); err != nil {
			return nil, fmt.Errorf("oauth2: scan due-for-refresh row: %w", err)
		}
		t, err := s.decodeToken(username, provider, accessEnc, accessNonce, []byte(refreshEnc.String), []byte(refreshNonce.String), refreshEnc.Valid, scope, expiry, masterKey)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("oauth2: iterate due-for-refresh rows: %w", err)
	}
	return out, nil
}

// decodeToken unwraps a row's encrypted columns into a Token. refreshValid
// distinguishes a NULL refresh_token_enc (never granted) from a genuinely
// empty one, which never happens by construction but is handled the same
// way for safety.
func (s *Store) decodeToken(username, provider string, accessEnc, accessNonce, refreshEnc, refreshNonce []byte, refreshValid bool, scope, expiry string, masterKey []byte) (Token, error) {
	accessPlain, err := keyring.Unwrap(masterKey, accessEnc, accessNonce, tokenAAD(username, provider, "access"))
	if err != nil {
		return Token{}, fmt.Errorf("oauth2: unwrap access token for %s/%s: %w", username, provider, err)
	}

	var refreshPlain []byte
	if refreshValid {
		refreshPlain, err = keyring.Unwrap(masterKey, refreshEnc, refreshNonce, tokenAAD(username, provider, "refresh"))
		if err != nil {
			return Token{}, fmt.Errorf("oauth2: unwrap refresh token for %s/%s: %w", username, provider, err)
		}
	}

	expiryTime, err := time.Parse(time.RFC3339, expiry)
	if err != nil {
		return Token{}, fmt.Errorf("oauth2: parse expiry for %s/%s: %w", username, provider, err)
	}

	return Token{
		Username:     username,
		Provider:     provider,
		AccessToken:  string(accessPlain),
		RefreshToken: string(refreshPlain),
		Scope:        scope,
		Expiry:       expiryTime,
	}, nil
}

// DeleteToken removes username's stored token for provider, if any.
func (s *Store) DeleteToken(ctx context.Context, username, provider string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE username = ? AND provider = ?`, username, provider); err != nil {
		return fmt.Errorf("oauth2: delete token: %w", err)
	}
	return nil
}

// HasToken reports whether username has ever completed authorization for
// provider.
func (s *Store) HasToken(ctx context.Context, username, provider string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_tokens WHERE username = ? AND provider = ?`, username, provider).Scan(&n); err != nil {
		return false, fmt.Errorf("oauth2: has token: %w", err)
	}
	return n > 0, nil
}
