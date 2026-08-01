// Package users is the web UI login/identity registry. A User doubles as
// the canonical identity for history/memory: the same Username is used as
// the user_id key in internal/history and internal/memory regardless of
// whether a turn arrives via an authenticated web UI session or via Home
// Assistant — see Registry.ResolveUserID.
package users

import (
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/archer-developer/miranda/internal/config"
)

// SourceHAAssist must match the InputRequest.Source value the HA thin
// client sends (see internal/httpapi), since only that source's raw user_id
// (a Home Assistant user id from speaker recognition) gets resolved via
// HAUserID — every other source is trusted to already carry the right id.
const SourceHAAssist = "ha_assist"

// SourceTelegram is the InputRequest.Source value the Telegram webhook
// handler (internal/httpapi) uses for turns that arrive via the bot.
const SourceTelegram = "telegram"

// SourceScheduled is the InputRequest.Source value
// Orchestrator.RunScheduledTasks uses when replaying a due scheduled task's
// prompt through Handle — never live-spoken the way ha_assist is (see
// streamOneTurn's speakLive check), since a scheduled turn's own prompt is
// expected to call speak_reply/send_telegram/etc. explicitly for whatever
// output it needs, the same as every other non-ha_assist source.
const SourceScheduled = "scheduled"

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
	// Timezone is the IANA timezone name from config (e.g. "Europe/Moscow").
	// Call Location() to get a parsed *time.Location.
	Timezone string
}

// Location returns the parsed IANA timezone for this user, or time.Local when
// Timezone is empty or names an unknown zone. Used by the scheduler to
// interpret cron expressions and display times in the user's local time.
func (u User) Location() *time.Location {
	if u.Timezone == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(u.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
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
	byUsername     map[string]User
	byHAUserID     map[string]User
	byTelegramName map[string]User // keyed by normalizeTelegramName(TelegramName)
}

// NewRegistry builds a Registry from configured users. Usernames must be
// unique; a duplicate is a configuration error caught at startup rather than
// silently shadowing an account.
func NewRegistry(configs []config.UserConfig) (*Registry, error) {
	r := &Registry{
		byUsername:     make(map[string]User, len(configs)),
		byHAUserID:     make(map[string]User),
		byTelegramName: make(map[string]User),
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
			Timezone:     c.Timezone,
		}
		r.byUsername[c.Username] = u
		if c.HAUserID != "" {
			r.byHAUserID[c.HAUserID] = u
		}
		if c.TelegramName != "" {
			r.byTelegramName[normalizeTelegramName(c.TelegramName)] = u
		}
	}
	return r, nil
}

// normalizeTelegramName strips an optional leading "@" and lowercases, so
// config.yaml's telegram_name and the @username Telegram sends on every
// webhook update compare equal regardless of how either was written —
// Telegram usernames are themselves case-insensitive.
func normalizeTelegramName(name string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "@"))
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

// ResolveByTelegramName looks up the configured user whose TelegramName
// matches telegramUsername (the @username Telegram sends as
// message.from.username on every webhook update), ignoring a leading "@"
// and case. Used by the webhook handler (internal/httpapi) to map an
// incoming message to a Miranda account before it ever reaches the agent
// loop — an unmatched name must not be forwarded (see that handler's doc
// comment for why: an unrecognized Telegram account has no memory/history
// identity to act as).
func (r *Registry) ResolveByTelegramName(telegramUsername string) (User, bool) {
	if telegramUsername == "" {
		return User{}, false
	}
	u, ok := r.byTelegramName[normalizeTelegramName(telegramUsername)]
	return u, ok
}

// ResolveByDisplayName finds the configured user whose Username or
// FullName case-insensitively matches name — used by the send_telegram
// tool to resolve a spoken/typed recipient (e.g. "Аня") to an account. The
// household's member names are already known to the model via the system
// prompt, so this only needs to match what the model is told, not do fuzzy
// natural-language matching.
func (r *Registry) ResolveByDisplayName(name string) (User, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return User{}, false
	}
	for _, u := range r.byUsername {
		if strings.EqualFold(u.Username, name) || strings.EqualFold(u.FullName, name) {
			return u, true
		}
	}
	return User{}, false
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
