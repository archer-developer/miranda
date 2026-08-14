package oauth2

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// defaultPendingTTL is how long an in-flight authorization attempt stays
// valid before the sweeper evicts it — generous enough to cover a user who
// opens the link, gets distracted by Google's own consent screen, and comes
// back a few minutes later; short enough that an abandoned attempt doesn't
// linger as a stale, still-guessable-in-principle state value.
const defaultPendingTTL = 10 * time.Minute

// PendingAuth is one in-flight authorization attempt, correlated by an
// opaque state value the OAuth callback must present back. Modeled directly
// on internal/attachments.Store's TTL-bound, id-keyed record shape — used
// instead of a session cookie because the user clicks the authorization
// link from an external channel (Telegram, or a link surfaced in any chat
// reply), not necessarily from an authenticated browser tab carrying
// Miranda's own session cookie.
type PendingAuth struct {
	Username string
	Provider string
	// CodeVerifier is the PKCE verifier generated at StartAuthorization time,
	// replayed at token exchange.
	CodeVerifier string
	// RedirectURI is the exact redirect_uri sent to the provider — replayed
	// byte-for-byte at token exchange, since providers require an exact
	// match even if config could in principle change between the two calls.
	RedirectURI string
	CreatedAt   time.Time
}

// PendingAuthStore is a thread-safe, TTL-bounded, single-use correlation
// store for in-flight authorization attempts.
type PendingAuthStore struct {
	mu      sync.Mutex
	entries map[string]PendingAuth
	ttl     time.Duration
	done    chan struct{}
}

// NewPendingAuthStore creates a PendingAuthStore with the given TTL (<=0
// uses defaultPendingTTL) and starts its background sweeper goroutine,
// mirroring internal/attachments.Store.sweep. Call Close when done.
func NewPendingAuthStore(ttl time.Duration) *PendingAuthStore {
	if ttl <= 0 {
		ttl = defaultPendingTTL
	}
	s := &PendingAuthStore{
		entries: make(map[string]PendingAuth),
		ttl:     ttl,
		done:    make(chan struct{}),
	}
	go s.sweep()
	return s
}

// Put stores p under state, replacing any previous entry for that state.
func (s *PendingAuthStore) Put(state string, p PendingAuth) {
	p.CreatedAt = time.Now()
	s.mu.Lock()
	s.entries[state] = p
	s.mu.Unlock()
}

// Consume atomically gets-and-deletes the pending authorization for state —
// single-use, so a replayed callback (browser back button, retried GET)
// fails cleanly instead of re-exchanging an already-used authorization
// code. Returns ok=false for an unknown or already-expired state.
func (s *PendingAuthStore) Consume(state string) (PendingAuth, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.entries[state]
	if !ok {
		return PendingAuth{}, false
	}
	delete(s.entries, state)
	if time.Since(p.CreatedAt) > s.ttl {
		return PendingAuth{}, false
	}
	return p, true
}

// Close stops the background sweeper goroutine. The store must not be used
// after Close returns.
func (s *PendingAuthStore) Close() {
	close(s.done)
}

func (s *PendingAuthStore) sweep() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.evictExpired()
		}
	}
}

func (s *PendingAuthStore) evictExpired() {
	now := time.Now()
	s.mu.Lock()
	for state, p := range s.entries {
		if p.CreatedAt.Before(now.Add(-s.ttl)) {
			delete(s.entries, state)
		}
	}
	s.mu.Unlock()
}

// NewState generates a fresh, unguessable OAuth2 state value. Mirrors
// internal/attachments.NewFileID / internal/telegram.RandomSecret's exact
// pattern: 24 random bytes, hex-encoded.
func NewState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauth2: generate state: %w", err)
	}
	return hex.EncodeToString(b), nil
}
