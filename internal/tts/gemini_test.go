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
	p, err := NewGeminiProvider(cfg, config.YandexStationConfig{}, t.TempDir(), &fakeHA{}, nil)
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

// TestGeminiProvider_CallGemini_AllKeysBannedAfterFirstCycleFailsFastWithoutASecondCycle
// mirrors miranda-llm/gemini's own TestGemini_AllKeysBannedAfterFirstCycleFailsFastWithoutASecondCycle
// — once both keys hit quota in cycle 1, both get banned (30min default,
// far longer than any QuotaCooldownSeconds), so cycle 2 finds every key
// already banned and keyrotation.Run fails immediately rather than
// re-requesting either key. Recovery now comes from the ban's own expiry
// (or Dispatcher's fallback provider), not from waiting out a cooldown
// inside this one call.
func TestGeminiProvider_CallGemini_AllKeysBannedAfterFirstCycleFailsFastWithoutASecondCycle(t *testing.T) {
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

	_, err := p.callGemini(context.Background(), "ok")
	require.ErrorIs(t, err, ErrQuotaExceeded)
	require.Contains(t, err.Error(), "banned")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 2, requestCount, "cycle 2 must find every key already banned and skip straight to failing, not re-request either key")
}

// TestGeminiProvider_CallGemini_QuotaErrorBansKeyForFutureCalls mirrors
// miranda-llm/gemini's own TestGemini_QuotaErrorBansTheKeyForFutureCalls: a
// 429 bans the key so a later, separate callGemini call skips it outright —
// the mechanism behind ordering a free-tier key before a paid one and
// having Miranda fail over automatically once the free key is exhausted.
func TestGeminiProvider_CallGemini_QuotaErrorBansKeyForFutureCalls(t *testing.T) {
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
		_, _ = w.Write([]byte(successBody("PCMDATA:ok")))
	})

	t.Setenv("GEMINI_TEST_KEY_1", "key-1")
	t.Setenv("GEMINI_TEST_KEY_2", "key-2")
	p := newTestGeminiProvider(t, config.GeminiTTSConfig{
		APIKeyEnvs: []string{"GEMINI_TEST_KEY_1", "GEMINI_TEST_KEY_2"},
	})

	_, err := p.callGemini(context.Background(), "first")
	require.NoError(t, err)
	require.True(t, p.bans.Banned(0), "the quota-exceeded key must be banned")

	_, err = p.callGemini(context.Background(), "second")
	require.NoError(t, err)
	require.Equal(t, []string{"key-1", "key-2", "key-2"}, requests, "the banned key must be skipped outright on the second call")
}

// TestGeminiProvider_CallGemini_OverloadedRotatesAndBansTheKey mirrors
// miranda-llm/gemini's own TestGemini_OverloadedRotatesAndBansTheKey: an
// explicit 503 UNAVAILABLE now rotates to the next key and bans the
// overloaded one — the mechanism behind switching to a paid key when the
// free tier is overloaded.
func TestGeminiProvider_CallGemini_OverloadedRotatesAndBansTheKey(t *testing.T) {
	var requests []string
	withGeminiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-goog-api-key")
		requests = append(requests, key)
		if key == "key-1" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":503,"message":"backend overloaded","status":"UNAVAILABLE"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(successBody("PCMDATA:ok")))
	})

	t.Setenv("GEMINI_TEST_KEY_1", "key-1")
	t.Setenv("GEMINI_TEST_KEY_2", "key-2")
	p := newTestGeminiProvider(t, config.GeminiTTSConfig{
		APIKeyEnvs: []string{"GEMINI_TEST_KEY_1", "GEMINI_TEST_KEY_2"},
	})

	pcm, err := p.callGemini(context.Background(), "ok")
	require.NoError(t, err)
	require.Equal(t, "PCMDATA:ok", string(pcm))
	require.Equal(t, []string{"key-1", "key-2"}, requests)
	require.True(t, p.bans.Banned(0), "the overloaded key must be banned")
	require.False(t, p.bans.Banned(1))
}

// TestGeminiProvider_CallGemini_FirstResponseTimeoutRotatesAndBans mirrors
// miranda-llm/gemini's own TestGemini_FirstResponseTimeoutRotatesAndBans: a
// backend that never answers within OverloadResponseTimeoutSeconds must be
// treated as overloaded (ban + rotate) rather than waited out.
func TestGeminiProvider_CallGemini_FirstResponseTimeoutRotatesAndBans(t *testing.T) {
	var mu sync.Mutex
	requestCount := 0
	withGeminiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := requestCount
		requestCount++
		mu.Unlock()

		if idx == 0 {
			// Sleep past the 1s overload timeout so the client gives up
			// first; still responds eventually (rather than blocking on
			// r.Context().Done(), which isn't guaranteed to fire promptly
			// just because the client stopped waiting) so the test
			// server's Close() during cleanup doesn't hang.
			time.Sleep(3 * time.Second)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(successBody("PCMDATA:too-late")))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(successBody("PCMDATA:ok")))
	})

	t.Setenv("GEMINI_TEST_KEY_1", "key-1")
	t.Setenv("GEMINI_TEST_KEY_2", "key-2")
	p := newTestGeminiProvider(t, config.GeminiTTSConfig{
		APIKeyEnvs:                     []string{"GEMINI_TEST_KEY_1", "GEMINI_TEST_KEY_2"},
		OverloadResponseTimeoutSeconds: 1,
	})

	pcm, err := p.callGemini(context.Background(), "ok")
	require.NoError(t, err)
	require.Equal(t, "PCMDATA:ok", string(pcm))
	require.True(t, p.bans.Banned(0), "the slow-to-respond key must be banned")
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

func TestGeminiProvider_Speak_ErrorsWithoutPublicBaseURL(t *testing.T) {
	t.Setenv("GEMINI_TEST_KEY_1", "key-1")

	p, err := NewGeminiProvider(
		config.GeminiTTSConfig{APIKeyEnvs: []string{"GEMINI_TEST_KEY_1"}, ChunkMaxChars: 200},
		config.YandexStationConfig{}, t.TempDir(), &fakeHA{}, nil,
	)
	require.NoError(t, err)
	require.Error(t, p.Speak(context.Background(), "hi", "media_player.kitchen"))
}
