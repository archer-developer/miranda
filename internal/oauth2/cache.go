package oauth2

import (
	"sync"
	"time"
)

// Cache holds each user's currently-valid access token in memory only, for
// the current process's lifetime — mirrors internal/keyring/cache.go's
// in-memory-only shape. Unlike keyring's Cache (which has no expiry
// concept at all, since a master key doesn't expire), Get here treats an
// expired entry the same as a missing one: callers must never be handed a
// token the provider will reject.
type Cache struct {
	mu      sync.RWMutex
	entries map[cacheKey]cachedToken
}

type cacheKey struct {
	username, provider string
}

type cachedToken struct {
	accessToken string
	expiry      time.Time
}

// NewCache returns an empty, ready-to-use Cache.
func NewCache() *Cache {
	return &Cache{entries: make(map[cacheKey]cachedToken)}
}

// Set stores username's currently-valid access token for provider, replacing
// any previous entry.
func (c *Cache) Set(username, provider, accessToken string, expiry time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[cacheKey{username, provider}] = cachedToken{accessToken: accessToken, expiry: expiry}
}

// Get returns username's cached access token for provider. ok is false if
// no entry exists OR the cached token has already expired — there is no
// "expired but might still work" leniency here.
func (c *Cache) Get(username, provider string) (accessToken string, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, found := c.entries[cacheKey{username, provider}]
	if !found || !time.Now().Before(entry.expiry) {
		return "", false
	}
	return entry.accessToken, true
}

// Delete discards username's cached access token for provider, if any.
func (c *Cache) Delete(username, provider string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, cacheKey{username, provider})
}
