// Package users is the web UI login/identity registry. A User doubles as
// the canonical identity for history/memory: the same Username is used as
// the user_id key in internal/history and internal/memory regardless of
// whether a turn arrives via an authenticated web UI session or via Home
// Assistant — see Registry.ResolveUserID.
package users

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"

	"github.com/archer-developer/miranda/internal/config"
)

// SourceHAAssist must match the InputRequest.Source value the HA thin
// client sends (see internal/httpapi), since only that source's raw user_id
// (a Home Assistant user id from speaker recognition) gets resolved via
// HAUserID — every other source is trusted to already carry the right id.
const SourceHAAssist = "ha_assist"

// User is one configured account, with config-file secrets already
// unwrapped (PasswordHash stays hashed; it's never handled in plaintext
// after config load).
type User struct {
	Username     string
	PasswordHash string
	FullName     string
	Avatar       string
	HAUserID     string
	TelegramName string
	Language     string
}

// DisplayName returns FullName if set, else the bare Username — for the web
// UI header/avatar area.
func (u User) DisplayName() string {
	if u.FullName != "" {
		return u.FullName
	}
	return u.Username
}

// Registry resolves login credentials and canonical user identity.
type Registry struct {
	byUsername map[string]User
	byHAUserID map[string]User
}

// NewRegistry builds a Registry from configured users. Usernames must be
// unique; a duplicate is a configuration error caught at startup rather than
// silently shadowing an account.
func NewRegistry(configs []config.UserConfig) (*Registry, error) {
	r := &Registry{
		byUsername: make(map[string]User, len(configs)),
		byHAUserID: make(map[string]User),
	}
	for _, c := range configs {
		if c.Username == "" {
			return nil, fmt.Errorf("users: a configured user has an empty username")
		}
		if _, exists := r.byUsername[c.Username]; exists {
			return nil, fmt.Errorf("users: duplicate username %q in config", c.Username)
		}
		u := User{
			Username:     c.Username,
			PasswordHash: c.PasswordHash,
			FullName:     c.FullName,
			Avatar:       c.Avatar,
			HAUserID:     c.HAUserID,
			TelegramName: c.TelegramName,
			Language:     c.Language,
		}
		r.byUsername[c.Username] = u
		if c.HAUserID != "" {
			r.byHAUserID[c.HAUserID] = u
		}
	}
	return r, nil
}

// Empty reports whether no users are configured — the web UI is
// unreachable in this state, since login is mandatory and there's nobody
// who could log in.
func (r *Registry) Empty() bool {
	return len(r.byUsername) == 0
}

// Get looks up a user by username.
func (r *Registry) Get(username string) (User, bool) {
	u, ok := r.byUsername[username]
	return u, ok
}

// Authenticate checks a login attempt against the configured password hash.
func (r *Registry) Authenticate(username, password string) (User, bool) {
	u, ok := r.byUsername[username]
	if !ok || u.PasswordHash == "" {
		return User{}, false
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return User{}, false
	}
	return u, true
}

// ResolveUserID maps a raw InputRequest user_id to the canonical username
// used as the history/memory key. For ha_assist, HA sends its own
// speaker-recognition user id (typically a HA-internal UUID, not one of our
// usernames); if it matches a configured HAUserID, we translate it to that
// user's Username so a person's memory is the same whether they talk via HA
// voice or the (now-authenticated) web UI. Any other source, or an
// unmatched HA id, passes through unchanged — this keeps ad-hoc
// curl/testing with arbitrary user_ids working exactly as before.
func (r *Registry) ResolveUserID(source, rawUserID string) string {
	if source == SourceHAAssist {
		if u, ok := r.byHAUserID[rawUserID]; ok {
			return u.Username
		}
	}
	return rawUserID
}

// HashPassword bcrypt-hashes password for storage in config.yaml's
// users[].password_hash. Not called at runtime — it's what you run once
// (e.g. via `go run` or a short script) to generate a hash to paste into
// config.yaml; passwords are never stored or logged in plaintext.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("users: hash password: %w", err)
	}
	return string(hash), nil
}
