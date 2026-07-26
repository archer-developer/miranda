package tts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCacheKey_SameInputsProduceSameKey(t *testing.T) {
	k1 := cacheKey("model-a", "voice-a", "wav", "привет")
	k2 := cacheKey("model-a", "voice-a", "wav", "привет")
	require.Equal(t, k1, k2)
	require.Len(t, k1, cacheKeyHexLen)
}

func TestCacheKey_DifferentInputsProduceDifferentKeys(t *testing.T) {
	base := cacheKey("model-a", "voice-a", "wav", "привет")

	require.NotEqual(t, base, cacheKey("model-b", "voice-a", "wav", "привет"), "model must be part of the key")
	require.NotEqual(t, base, cacheKey("model-a", "voice-b", "wav", "привет"), "voice must be part of the key")
	require.NotEqual(t, base, cacheKey("model-a", "voice-a", "mp3", "привет"), "format must be part of the key")
	require.NotEqual(t, base, cacheKey("model-a", "voice-a", "wav", "пока"), "text must be part of the key")
}

func TestDiskCache_StoreThenHasThenPathRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c, err := newDiskCache(dir)
	require.NoError(t, err)

	key := cacheKey("m", "v", "wav", "text")
	require.False(t, c.Has(key, "wav"), "must not report a hit before anything is stored")

	require.NoError(t, c.Store(key, "wav", []byte("fake audio bytes")))
	require.True(t, c.Has(key, "wav"))

	data, err := os.ReadFile(filepath.Join(dir, key+".wav"))
	require.NoError(t, err)
	require.Equal(t, "fake audio bytes", string(data))
}

func TestDiskCache_HasIsFalseForAWrongExtension(t *testing.T) {
	dir := t.TempDir()
	c, err := newDiskCache(dir)
	require.NoError(t, err)

	key := cacheKey("m", "v", "wav", "text")
	require.NoError(t, c.Store(key, "wav", []byte("data")))

	require.False(t, c.Has(key, "mp3"), "a cache entry for one format must not report a hit for another")
}

func TestNewDiskCache_CreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "cache", "dir")
	_, err := newDiskCache(dir)
	require.NoError(t, err)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.True(t, info.IsDir())
}
