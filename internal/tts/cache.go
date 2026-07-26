package tts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cacheKeyHexLen is the length of a cacheKey string: sha256's 32-byte
// digest, hex-encoded. httpserve.go validates an incoming request path's key
// against this exact length (and charset) before it's ever used to build a
// filesystem path.
const cacheKeyHexLen = sha256.Size * 2

// diskCache is a content-addressed, permanent (deliberately no TTL/expiry —
// see CLAUDE.md) store for synthesized TTS audio files, keyed by a hash of
// exactly what would produce identical audio (model + voice + format +
// text). A cache hit lets geminiProvider skip calling Gemini's API
// entirely — the main mechanism this feature has for not burning through
// Gemini's quota, more so than chunk size.
type diskCache struct {
	dir string
}

// newDiskCache ensures dir exists and returns a diskCache rooted there.
func newDiskCache(dir string) (*diskCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("tts: create cache dir %s: %w", dir, err)
	}
	return &diskCache{dir: dir}, nil
}

// cacheKey hashes exactly the inputs that determine the resulting audio, so
// the same text under a different model/voice/format never collides with
// (or serves stale audio for) a different combination.
func cacheKey(model, voice, format, text string) string {
	sum := sha256.Sum256([]byte(model + "|" + voice + "|" + format + "|" + text))
	return hex.EncodeToString(sum[:])
}

// path returns the on-disk location for key/ext, e.g.
// "<dir>/<64 hex chars>.wav".
func (c *diskCache) path(key, ext string) string {
	return filepath.Join(c.dir, key+"."+ext)
}

// durationPath returns the on-disk location of key's duration sidecar (see
// StoreDuration) — a plain-text millisecond count, next to the audio file
// itself.
func (c *diskCache) durationPath(key string) string {
	return filepath.Join(c.dir, key+".dur")
}

// Has reports whether key/ext's audio has already been rendered and cached.
func (c *diskCache) Has(key, ext string) bool {
	_, err := os.Stat(c.path(key, ext))
	return err == nil
}

// Store persists data under key/ext, overwriting any existing file — a
// content-addressed key never legitimately maps to two different sets of
// bytes, but a fresh render harmlessly replacing a possibly-truncated prior
// write (e.g. from a process killed mid-write) is fine.
func (c *diskCache) Store(key, ext string, data []byte) error {
	if err := os.WriteFile(c.path(key, ext), data, 0o644); err != nil {
		return fmt.Errorf("tts: write cache file for key %s: %w", key, err)
	}
	return nil
}

// StoreDuration persists ms (the exact playback duration of the raw PCM
// Gemini returned, computed once at synthesis time — see
// audio.go's pcmDurationMS) as a small sidecar next to key's audio file, so
// a later cache hit knows how long to let the station play it without
// re-deriving anything from the encoded container (which, for a
// variable-frame format, isn't exact from file size alone the way it would
// be for a fixed-rate WAV).
func (c *diskCache) StoreDuration(key string, ms int) error {
	if err := os.WriteFile(c.durationPath(key), []byte(strconv.Itoa(ms)), 0o644); err != nil {
		return fmt.Errorf("tts: write cache duration sidecar for key %s: %w", key, err)
	}
	return nil
}

// Duration reads back key's duration sidecar. ok is false if it's missing or
// unparseable — e.g. a cache directory populated before this sidecar existed
// — in which case the caller falls back to a cruder estimate rather than
// failing the whole Speak call.
func (c *diskCache) Duration(key string) (ms int, ok bool) {
	data, err := os.ReadFile(c.durationPath(key))
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return n, true
}
