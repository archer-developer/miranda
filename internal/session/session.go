// Package session is a minimal in-memory login session store for the web
// UI. It's intentionally simple — one process, no persistence across
// restarts, no sliding renewal — matching the project's "single small home
// appliance on a trusted LAN" trust model (see server.auth_token's similar
// stance in internal/config). Sessions are just an opaque random token
// mapped to a username; nothing about them is validated cryptographically,
// so treat the token like a password and only ever transmit it as an
// HttpOnly cookie.
package session

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

const tokenBytes = 32

// CookieName is the cookie both internal/webui (which sets it on login) and
// internal/httpapi (which reads it to authenticate browser-originated
// POST /api/v1/input and /ws/logs requests) use, so there's one name to
// keep in sync rather than two packages agreeing on a string by convention.
const CookieName = "miranda_session"

// Store maps session tokens to the username that created them, expiring
// them after TTL.
type Store struct {
	mu       sync.Mutex
	sessions map[string]entry
	ttl      time.Duration
}

type entry struct {
	username  string
	expiresAt time.Time
}

// NewStore creates a Store whose sessions expire ttl after creation.
func NewStore(ttl time.Duration) *Store {
	return &Store{sessions: make(map[string]entry), ttl: ttl}
}

// TTL returns the session lifetime, so callers setting the session cookie
// can give it a matching Max-Age.
func (s *Store) TTL() time.Duration {
	return s.ttl
}

// Create starts a new session for username and returns its token.
func (s *Store) Create(username string) (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("session: generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = entry{username: username, expiresAt: time.Now().Add(s.ttl)}
	return token, nil
}

// Validate returns the username associated with token, if it exists and
// hasn't expired. An expired session is evicted on lookup.
func (s *Store) Validate(token string) (string, bool) {
	if token == "" {
		return "", false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.sessions[token]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expiresAt) {
		delete(s.sessions, token)
		return "", false
	}
	return e.username, true
}

// Destroy invalidates token (logout).
func (s *Store) Destroy(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}
