package tts

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
)

// withGeminiTestServer points package-level geminiAPIBaseURL at a fresh
// httptest.Server running handler for the duration of the calling test,
// restoring the real Gemini host on cleanup — geminiAPIBaseURL is a var
// (not a const) precisely so tests can substitute a fake endpoint instead
// of hitting Google's real API.
func withGeminiTestServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	original := geminiAPIBaseURL
	geminiAPIBaseURL = srv.URL
	t.Cleanup(func() {
		srv.Close()
		geminiAPIBaseURL = original
	})
}

// successBody builds a minimal, but shape-accurate, generateContent success
// response: candidates[0].content.parts[0].inlineData.data is base64 PCM
// (here just a recognizable placeholder, not real audio — callGemini
// doesn't care what the bytes mean, only that they decode).
func successBody(pcmPlaceholder string) string {
	data := base64.StdEncoding.EncodeToString([]byte(pcmPlaceholder))
	return fmt.Sprintf(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/L16;rate=24000","data":%q}}]}}]}`, data)
}

const quotaExceededBody = `{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}`

// newTestGeminiProvider builds a *geminiProvider (unwrapping the Provider
// interface NewGeminiProvider returns, since this test file lives in
// package tts and can reach the concrete type directly) against a fake HA
// client — HA interaction isn't what these tests exercise, callGemini/
// synthesize are called directly instead of going through Speak.
func newTestGeminiProvider(t *testing.T, cfg config.GeminiTTSConfig) *geminiProvider {
	t.Helper()
	if cfg.Model == "" {
		cfg.Model = "gemini-test-tts"
	}
	if cfg.Voice == "" {
		cfg.Voice = "Kore"
	}
	p, err := NewGeminiProvider(cfg, config.YandexStationConfig{Entities: []string{"media_player.kitchen"}}, t.TempDir(), &fakeHA{}, nil)
	require.NoError(t, err)
	gp, ok := p.(*geminiProvider)
	require.True(t, ok)
	return gp
}

func TestGeminiProvider_CallGemini_Success(t *testing.T) {
	var requests []string
	withGeminiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Header.Get("x-goog-api-key"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(successBody("PCMDATA:hello")))
	})

	t.Setenv("GEMINI_TEST_KEY_1", "key-1")
	p := newTestGeminiProvider(t, config.GeminiTTSConfig{
		APIKeyEnvs:          []string{"GEMINI_TEST_KEY_1"},
		MaxQuotaRetryCycles: 1,
	})

	pcm, err := p.callGemini(context.Background(), "hello")
	require.NoError(t, err)
	require.Equal(t, "PCMDATA:hello", string(pcm))
	require.Equal(t, []string{"key-1"}, requests)
}

func TestGeminiProvider_CallGemini_RotatesKeyOn429(t *testing.T) {
	var requests []string
	withGeminiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-goog-api-key")
		requests = append(requests, key)
		if key == "key-1" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(quotaExceededBody))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(successBody("PCMDATA:hi")))
	})

	t.Setenv("GEMINI_TEST_KEY_1", "key-1")
	t.Setenv("GEMINI_TEST_KEY_2", "key-2")
	p := newTestGeminiProvider(t, config.GeminiTTSConfig{
		APIKeyEnvs:          []string{"GEMINI_TEST_KEY_1", "GEMINI_TEST_KEY_2"},
		MaxQuotaRetryCycles: 1,
	})

	pcm, err := p.callGemini(context.Background(), "hi")
	require.NoError(t, err)
	require.Equal(t, "PCMDATA:hi", string(pcm))
	require.Equal(t, []string{"key-1", "key-2"}, requests)
}

func TestGeminiProvider_CallGemini_AllKeys429ThenCooldownThenSucceeds(t *testing.T) {
	var mu sync.Mutex
	callCount := map[string]int{}
	withGeminiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-goog-api-key")
		mu.Lock()
		callCount[key]++
		n := callCount[key]
		mu.Unlock()

		if n == 1 { // every key's first call, in every cycle so far, hits quota
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(quotaExceededBody))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(successBody("PCMDATA:ok")))
	})

	t.Setenv("GEMINI_TEST_KEY_1", "key-1")
	t.Setenv("GEMINI_TEST_KEY_2", "key-2")
	p := newTestGeminiProvider(t, config.GeminiTTSConfig{
		APIKeyEnvs: []string{"GEMINI_TEST_KEY_1", "GEMINI_TEST_KEY_2"},
		// 0s keeps the test fast; still exercises the "sleep, then retry the
		// whole key list" cycle path, just without an observable delay.
		QuotaCooldownSeconds: 0,
		MaxQuotaRetryCycles:  2,
	})

	pcm, err := p.callGemini(context.Background(), "ok")
	require.NoError(t, err)
	require.Equal(t, "PCMDATA:ok", string(pcm))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 2, callCount["key-1"], "key-1 is tried first each cycle: fails cycle 1, succeeds cycle 2")
	require.Equal(t, 1, callCount["key-2"], "key-2 is only ever reached once, in cycle 1, before key-1 succeeds in cycle 2")
}

func TestGeminiProvider_CallGemini_AllKeysExhaustedAcrossAllCyclesReturnsErrQuotaExceeded(t *testing.T) {
	var mu sync.Mutex
	requestCount := 0
	withGeminiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(quotaExceededBody))
	})

	t.Setenv("GEMINI_TEST_KEY_1", "key-1")
	t.Setenv("GEMINI_TEST_KEY_2", "key-2")
	p := newTestGeminiProvider(t, config.GeminiTTSConfig{
		APIKeyEnvs:           []string{"GEMINI_TEST_KEY_1", "GEMINI_TEST_KEY_2"},
		QuotaCooldownSeconds: 0,
		MaxQuotaRetryCycles:  2,
	})

	_, err := p.callGemini(context.Background(), "text")
	require.ErrorIs(t, err, ErrQuotaExceeded)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 4, requestCount, "2 keys x 2 cycles")
}

func TestGeminiProvider_CallGemini_NonQuotaErrorShortCircuitsWithoutTryingOtherKeys(t *testing.T) {
	var requests []string
	withGeminiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Header.Get("x-goog-api-key"))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"message":"boom","status":"INTERNAL"}}`))
	})

	t.Setenv("GEMINI_TEST_KEY_1", "key-1")
	t.Setenv("GEMINI_TEST_KEY_2", "key-2")
	p := newTestGeminiProvider(t, config.GeminiTTSConfig{
		APIKeyEnvs:          []string{"GEMINI_TEST_KEY_1", "GEMINI_TEST_KEY_2"},
		MaxQuotaRetryCycles: 3,
	})

	_, err := p.callGemini(context.Background(), "text")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrQuotaExceeded)
	require.Equal(t, []string{"key-1"}, requests, "a non-quota error must not rotate keys or retry")
}

func TestGeminiProvider_Synthesize_CacheHitSkipsGeminiEntirely(t *testing.T) {
	called := false
	withGeminiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(successBody("PCMDATA:x")))
	})

	t.Setenv("GEMINI_TEST_KEY_1", "key-1")
	cacheDir := t.TempDir()
	gp := newTestGeminiProvider(t, config.GeminiTTSConfig{
		APIKeyEnvs:          []string{"GEMINI_TEST_KEY_1"},
		AudioFormat:         "wav",
		PublicBaseURL:       "http://station.local",
		ChunkMaxChars:       200,
		MaxQuotaRetryCycles: 1,
	})
	// newTestGeminiProvider makes its own t.TempDir() cache dir; rebuild
	// against cacheDir explicitly so this test can pre-seed it.
	gp.cache, _ = newDiskCache(cacheDir)

	key := cacheKey(gp.cfg.Model, gp.cfg.Voice, "wav", "привет")
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, key+".wav"), []byte("cached"), 0o644))

	url, duration, err := gp.synthesize(context.Background(), "привет")
	require.NoError(t, err)
	require.Equal(t, "http://station.local/tts-audio/"+key+".wav", url)
	require.False(t, called, "a cache hit must skip calling Gemini entirely")
	// No .dur sidecar was pre-seeded, so this must fall back to the
	// chars-based estimate rather than zero (which would reproduce the
	// overlap bug this mechanism exists to prevent).
	require.Greater(t, duration, time.Duration(0))
}

func TestGeminiProvider_Synthesize_FreshRenderStoresExactDurationSidecar(t *testing.T) {
	// 24kHz/16-bit/mono PCM: 24000 samples * 2 bytes/sample = 48000 bytes
	// for exactly one second of audio.
	onePCMSecond := string(make([]byte, 24000*2))
	withGeminiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(successBody(onePCMSecond)))
	})

	t.Setenv("GEMINI_TEST_KEY_1", "key-1")
	gp := newTestGeminiProvider(t, config.GeminiTTSConfig{
		APIKeyEnvs:          []string{"GEMINI_TEST_KEY_1"},
		AudioFormat:         "wav",
		PublicBaseURL:       "http://station.local",
		ChunkMaxChars:       200,
		MaxQuotaRetryCycles: 1,
	})

	_, duration, err := gp.synthesize(context.Background(), "привет")
	require.NoError(t, err)
	require.Equal(t, time.Second, duration)

	// A second synthesize call for the same text must be a cache hit that
	// reads the exact sidecar back, not the crude chars-based estimate.
	_, duration2, err := gp.synthesize(context.Background(), "привет")
	require.NoError(t, err)
	require.Equal(t, time.Second, duration2)
}

func TestNewGeminiProvider_FailsWhenNoConfiguredEnvVarIsSet(t *testing.T) {
	_, err := NewGeminiProvider(
		config.GeminiTTSConfig{APIKeyEnvs: []string{"MIRANDA_TEST_UNSET_GEMINI_KEY"}},
		config.YandexStationConfig{}, t.TempDir(), &fakeHA{}, nil,
	)
	require.Error(t, err)
}

func TestGeminiProvider_Speak_ErrorsWithoutEntitiesOrPublicBaseURL(t *testing.T) {
	t.Setenv("GEMINI_TEST_KEY_1", "key-1")

	// No entities configured.
	p1, err := NewGeminiProvider(
		config.GeminiTTSConfig{APIKeyEnvs: []string{"GEMINI_TEST_KEY_1"}, PublicBaseURL: "http://x", ChunkMaxChars: 200},
		config.YandexStationConfig{}, t.TempDir(), &fakeHA{}, nil,
	)
	require.NoError(t, err)
	require.Error(t, p1.Speak(context.Background(), "hi"))

	// No public_base_url configured.
	p2, err := NewGeminiProvider(
		config.GeminiTTSConfig{APIKeyEnvs: []string{"GEMINI_TEST_KEY_1"}, ChunkMaxChars: 200},
		config.YandexStationConfig{Entities: []string{"media_player.kitchen"}}, t.TempDir(), &fakeHA{}, nil,
	)
	require.NoError(t, err)
	require.Error(t, p2.Speak(context.Background(), "hi"))
}
