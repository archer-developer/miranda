package tts

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// newTestMux mounts h at the same GET /tts-audio/{filename} pattern
// cmd/miranda/internal/httpapi's Server.SetTTSAudioHandler registers it at,
// so these tests exercise PathValue extraction the same way the real route
// does, not just a bare handler call.
func newTestMux(h http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /tts-audio/{filename}", h)
	return mux
}

func TestHTTPHandler_ServesACachedFile(t *testing.T) {
	dir := t.TempDir()
	key := cacheKey("model", "voice", "wav", "привет")
	require.NoError(t, os.WriteFile(filepath.Join(dir, key+".wav"), []byte("fake wav bytes"), 0o644))

	h, err := NewHTTPHandler(dir)
	require.NoError(t, err)
	mux := newTestMux(h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tts-audio/"+key+".wav", nil)
	mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "fake wav bytes", rec.Body.String())
}

func TestHTTPHandler_RejectsNonHexKey(t *testing.T) {
	dir := t.TempDir()
	h, err := NewHTTPHandler(dir)
	require.NoError(t, err)
	mux := newTestMux(h)

	for _, filename := range []string{
		"not-hex-at-all.wav",
		strings.Repeat("g", cacheKeyHexLen) + ".wav",   // right length, non-hex characters
		strings.Repeat("a", cacheKeyHexLen-1) + ".wav", // one char short
		strings.Repeat("a", cacheKeyHexLen+1) + ".wav", // one char too long
	} {
		t.Run(filename, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/tts-audio/"+filename, nil)
			mux.ServeHTTP(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

// TestHTTPHandler_RejectsPathTraversalAttemptDirectly bypasses the mux (which
// would otherwise redirect a literal "../" in the URL path before ServeHTTP
// ever runs) to confirm ServeHTTP's own hex-key regexp is what actually
// stands between an untrusted filename and http.ServeFile, not just
// net/http's incidental path-cleaning behavior.
func TestHTTPHandler_RejectsPathTraversalAttemptDirectly(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("do not serve me"), 0o644))

	h, err := NewHTTPHandler(dir)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tts-audio/x", nil)
	req.SetPathValue("filename", "../"+secret)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHTTPHandler_RejectsUnknownExtension(t *testing.T) {
	dir := t.TempDir()
	h, err := NewHTTPHandler(dir)
	require.NoError(t, err)
	mux := newTestMux(h)

	key := strings.Repeat("a", cacheKeyHexLen)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tts-audio/"+key+".exe", nil)
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHTTPHandler_ValidKeyButMissingFileReturns404(t *testing.T) {
	dir := t.TempDir()
	h, err := NewHTTPHandler(dir)
	require.NoError(t, err)
	mux := newTestMux(h)

	key := strings.Repeat("a", cacheKeyHexLen)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tts-audio/"+key+".wav", nil)
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}
