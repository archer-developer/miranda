package tts

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// hexKeyPattern matches exactly a sha256 hex digest — cacheKeyHexLen
// lowercase hex characters — the only shape ServeHTTP accepts before it
// ever touches the filesystem, so a crafted key (e.g. "../../etc/passwd")
// is rejected on pattern alone rather than relying on path-cleaning to
// catch it.
var hexKeyPattern = regexp.MustCompile(fmt.Sprintf("^[0-9a-f]{%d}$", cacheKeyHexLen))

// HTTPHandler serves previously-rendered TTS audio files straight from a
// diskCache directory, so Home Assistant/Yandex Station can fetch one by
// URL (media_player.play_media given a URL, rather than raw text for the
// station to speak itself) — see geminiProvider.synthesize for how that URL
// is constructed.
type HTTPHandler struct {
	cache *diskCache
}

// NewHTTPHandler builds a handler serving files out of cacheDir — the same
// directory (config.StorageConfig.TTSCacheDir) a geminiProvider's cache
// writes to. Wire it in at GET /tts-audio/{filename} (see
// internal/httpapi/server.go's SetTTSAudioHandler: net/http's ServeMux only
// supports one wildcard per path segment, so the route captures the whole
// "<key>.<ext>" filename and ServeHTTP splits it itself, rather than
// registering "{key}.{ext}" directly, which the mux pattern syntax can't
// express).
func NewHTTPHandler(cacheDir string) (*HTTPHandler, error) {
	cache, err := newDiskCache(cacheDir)
	if err != nil {
		return nil, err
	}
	return &HTTPHandler{cache: cache}, nil
}

// ServeHTTP validates the requested filename is a bare "<sha256-hex>.<wav|mp3>"
// before ever building a filesystem path from it — the only untrusted input
// here is the URL path, and this check is what stands between it and a
// path-traversal read of an arbitrary file.
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	filename := r.PathValue("filename")
	key, ext, ok := strings.Cut(filename, ".")
	if !ok || !hexKeyPattern.MatchString(key) || (ext != "wav" && ext != "mp3") {
		http.Error(w, "invalid tts-audio key", http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, h.cache.path(key, ext))
}
