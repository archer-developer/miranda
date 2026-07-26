package webauthn

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	webauthnlib "github.com/go-webauthn/webauthn/webauthn"
)

const ceremonyIDBytes = 32

// CeremonyStore holds a webauthnlib.SessionData between a WebAuthn
// ceremony's begin and finish calls. Mirrors internal/session.Store's
// shape (in-memory, mutex-guarded, TTL-expiring) and the same "single
// process, no persistence across restarts" trust model — a ceremony spans
// seconds, not something that needs to survive a Miranda restart.
//
// Unlike a login session token, Take is *consuming*: a WebAuthn challenge
// must never be usable for a second Finish attempt, so a replayed request
// hard-fails instead of the (intentionally) repeatable session-cookie
// validation internal/session.Store does. This is a deliberate divergence
// from that package's shape, not an oversight.
type CeremonyStore struct {
	mu      sync.Mutex
	entries map[string]ceremonyEntry
	ttl     time.Duration
}

type ceremonyEntry struct {
	data      webauthnlib.SessionData
	expiresAt time.Time
}

// NewCeremonyStore creates a CeremonyStore whose entries expire ttl after
// being put. ttl should comfortably exceed the library's own ceremony
// timeout (tens of seconds) — a couple of minutes is plenty.
func NewCeremonyStore(ttl time.Duration) *CeremonyStore {
	return &CeremonyStore{entries: make(map[string]ceremonyEntry), ttl: ttl}
}

// Put stores data under key, overwriting any previous entry for that key.
// Used for registration, keyed by the caller's existing session token —
// "last begin wins" is fine there, since it's the same logged-in person.
func (c *CeremonyStore) Put(key string, data webauthnlib.SessionData) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = ceremonyEntry{data: data, expiresAt: time.Now().Add(c.ttl)}
}

// PutNew stores data under a freshly generated random key and returns it.
// Used for login: there's no session cookie yet to key by (that's the
// point of the ceremony), so the client must echo this id back on finish.
func (c *CeremonyStore) PutNew(data webauthnlib.SessionData) (string, error) {
	buf := make([]byte, ceremonyIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("webauthn: generate ceremony id: %w", err)
	}
	key := base64.RawURLEncoding.EncodeToString(buf)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = ceremonyEntry{data: data, expiresAt: time.Now().Add(c.ttl)}
	return key, nil
}

// Take returns and deletes the entry for key, if present and unexpired.
// Consuming (delete-on-read) by design — see the type doc comment.
func (c *CeremonyStore) Take(key string) (webauthnlib.SessionData, bool) {
	if key == "" {
		return webauthnlib.SessionData{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[key]
	delete(c.entries, key)
	if !ok {
		return webauthnlib.SessionData{}, false
	}
	if time.Now().After(e.expiresAt) {
		return webauthnlib.SessionData{}, false
	}
	return e.data, true
}
