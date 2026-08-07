package keyring

import "sync"

// Cache holds each user's unwrapped master key in memory, for the current
// process's lifetime only — never persisted, never expired on a timer.
// Per the accepted threat model (disk access, not a live memory-dump
// attacker — see docs/encryption.md), a key staying resident here for as
// long as a user is logged in anywhere (web UI, and therefore usable by HA
// voice/Telegram/scheduled tasks too) is an accepted risk, not something to
// engineer around. Only Lock (explicit logout) or a process restart ever
// clears an entry.
type Cache struct {
	mu   sync.RWMutex
	keys map[string][]byte
}

// NewCache returns an empty, ready-to-use Cache.
func NewCache() *Cache {
	return &Cache{keys: make(map[string][]byte)}
}

// Unlock stores username's unwrapped master key, replacing any previous
// entry.
func (c *Cache) Unlock(username string, key []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys[username] = key
}

// Get returns a copy of username's unwrapped master key, if currently
// unlocked. Deliberately a copy, not the cache's own backing array: Lock
// (e.g. from a concurrent logout) zeroes that array in place, and a caller
// holding a live reference to it (base64-encoding it for a tool call, say)
// could read a torn or fully-zeroed key if that races with a concurrent
// Lock — a real data race, not just a theoretical one. Returning a fresh
// slice here means every caller owns independent memory, so Lock can never
// affect anything a Get has already handed out.
func (c *Cache) Get(username string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	key, ok := c.keys[username]
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(key))
	copy(cp, key)
	return cp, true
}

// Lock discards username's cached key, if any — called on explicit logout.
// Best-effort zeroes the byte slice before dropping the reference; cheap
// defense-in-depth, not a hard guarantee (Go may have copied the bytes
// elsewhere, e.g. into a JSON-marshaled tool-call argument, before this
// runs — acceptable given the stated threat model).
func (c *Cache) Lock(username string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if key, ok := c.keys[username]; ok {
		for i := range key {
			key[i] = 0
		}
		delete(c.keys, username)
	}
}
