package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/archer-developer/miranda/internal/config"
)

// geminiAPIBaseURL is Gemini's public API host. It's a var, not a const, so
// tests can point requestOnce at an httptest.Server instead — see
// gemini_test.go.
var geminiAPIBaseURL = "https://generativelanguage.googleapis.com"

// geminiGenerateContentURLFormat is the generateContent REST endpoint,
// templated with geminiAPIBaseURL and the configured model name.
const geminiGenerateContentURLFormat = "%s/v1beta/models/%s:generateContent"

// errKeyQuotaExceeded marks a single API key, on a single request, as having
// hit its quota (HTTP 429, or a response body with error.status
// "RESOURCE_EXHAUSTED") — distinct from the exported ErrQuotaExceeded,
// which callGemini only returns once *every* configured key has failed this
// way across *every* retry cycle. Any other error from requestOnce is not
// wrapped in this and short-circuits callGemini immediately, without
// rotating keys or retrying — see its doc comment.
var errKeyQuotaExceeded = errors.New("tts: gemini: key quota exceeded")

// geminiProvider synthesizes speech via Gemini's generateContent API
// (generationConfig.responseModalities: ["AUDIO"]) and plays the result back
// through the same Yandex Station entities textProvider uses, by serving
// the rendered file over HTTP (see httpserve.go) and pointing
// media_player.play_media at that URL — Yandex Station's own TTS can't
// produce Gemini's voices, so the audio has to be pre-rendered and handed
// to the station as a file to fetch and play, rather than text for it to
// speak itself.
type geminiProvider struct {
	cfg        config.GeminiTTSConfig
	stationCfg config.YandexStationConfig
	station    *stationPlayer
	cache      *diskCache
	apiKeys    []string
	httpClient *http.Client
	logger     *slog.Logger
}

// NewGeminiProvider resolves cfg.APIKeyEnvs to actual key values from the
// process environment (never stored in config.yaml itself — same
// convention as every other secret in this codebase) and opens/creates the
// disk cache directory. Fails fast if none of the configured env vars
// actually hold a key, since every subsequent Speak call would otherwise
// fail identically anyway.
func NewGeminiProvider(cfg config.GeminiTTSConfig, stationCfg config.YandexStationConfig, cacheDir string, ha HAClient, logger *slog.Logger) (Provider, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var keys []string
	for _, env := range cfg.APIKeyEnvs {
		if v := os.Getenv(env); v != "" {
			keys = append(keys, v)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("tts: gemini_tts: none of the configured api_key_envs %v are set", cfg.APIKeyEnvs)
	}

	cache, err := newDiskCache(cacheDir)
	if err != nil {
		return nil, err
	}

	timeout := time.Duration(cfg.RequestTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	return &geminiProvider{
		cfg:        cfg,
		stationCfg: stationCfg,
		station:    newStationPlayer(ha, logger, stationCfg.IdlePollIntervalMS, stationCfg.PlaybackStartTimeoutMS),
		cache:      cache,
		apiKeys:    keys,
		httpClient: &http.Client{Timeout: timeout},
		logger:     logger,
	}, nil
}

// Speak chunks text greedily (see Accumulator's doc comment — several short
// sentences are grouped into one ~ChunkMaxChars chunk rather than one
// request per sentence, since each chunk here is a real, quota-limited
// Gemini API call), synthesizing (or reusing a cached rendering of) each
// chunk and playing it on every configured Yandex Station entity in turn.
func (p *geminiProvider) Speak(ctx context.Context, text string) error {
	if len(p.stationCfg.Entities) == 0 {
		return fmt.Errorf("tts: gemini_tts: no yandex_station entities configured to play audio through")
	}
	if p.cfg.PublicBaseURL == "" {
		return fmt.Errorf("tts: gemini_tts: public_base_url is not configured, the station has no way to fetch synthesized audio")
	}

	for _, chunk := range Chunk(text, p.cfg.ChunkMaxChars) {
		url, duration, err := p.synthesize(ctx, chunk)
		if err != nil {
			return err
		}
		for _, entity := range p.stationCfg.Entities {
			// media_content_type's value doesn't matter to AlexxIT/YandexStation
			// for URL playback — confirmed both by reading its source
			// (async_play_media dispatches on "http(s):// in media_id", not on
			// media_content_type at all) and by a live device: debug logging
			// showed it correctly built a "radio_play" externalCommandBypass
			// with our .wav streamUrl. "music" is kept only because it reads
			// clearly in logs — playAndSleep, not this value, is what makes
			// playback timing correct (see its doc comment).
			if err := p.station.playAndSleep(ctx, entity, url, "music", duration); err != nil {
				return err
			}
		}
	}
	return nil
}

// synthesize returns the public URL of chunk's rendered audio and its exact
// playback duration, generating and caching both first if this exact
// (model, voice, format, text) combination has never been rendered before —
// a cache hit skips the Gemini API call entirely, which matters more for
// quota than chunk size (see cache.go).
func (p *geminiProvider) synthesize(ctx context.Context, chunk string) (string, time.Duration, error) {
	key := cacheKey(p.cfg.Model, p.cfg.Voice, p.cfg.AudioFormat, chunk)
	ext := audioFileExt(p.cfg.AudioFormat)

	var duration time.Duration
	if p.cache.Has(key, ext) {
		if ms, ok := p.cache.Duration(key); ok {
			duration = time.Duration(ms) * time.Millisecond
		} else {
			// A cache entry from before the duration sidecar existed: fall
			// back to a crude estimate rather than failing, or assuming
			// zero and reproducing the very overlap this mechanism exists
			// to prevent.
			duration = estimatedDurationFromChars(len([]rune(chunk)))
		}
	} else {
		pcm, err := p.callGemini(ctx, chunk)
		if err != nil {
			return "", 0, err
		}
		durationMS := pcmDurationMS(pcm)
		duration = time.Duration(durationMS) * time.Millisecond

		encoded, err := encodeAudio(pcm, p.cfg.AudioFormat)
		if err != nil {
			return "", 0, err
		}
		if err := p.cache.Store(key, ext, encoded); err != nil {
			return "", 0, err
		}
		if err := p.cache.StoreDuration(key, durationMS); err != nil {
			return "", 0, err
		}
	}

	url := strings.TrimRight(p.cfg.PublicBaseURL, "/") + "/tts-audio/" + key + "." + ext
	return url, duration, nil
}

// estimatedDurationFromChars is the fallback used only when a cache hit has
// no duration sidecar (e.g. a cache directory populated before one
// existed) — a conservative average speaking rate, so old entries don't
// regress to "assume instant" and reproduce the very overlap this whole
// mechanism exists to prevent.
func estimatedDurationFromChars(chars int) time.Duration {
	const charsPerSecond = 12
	return time.Duration(chars) * time.Second / charsPerSecond
}

// callGemini requests chunk's audio from Gemini, rotating across every
// configured API key on a quota error (HTTP 429 or a response body with
// error.status "RESOURCE_EXHAUSTED") before giving up on that pass. If
// every key hits quota in one pass, it sleeps QuotaCooldownSeconds and
// retries the whole key list again, up to MaxQuotaRetryCycles total passes —
// most free-tier Gemini quota windows are per-minute, so a short cooldown
// often recovers a key that failed moments ago. Once every cycle is
// exhausted, it returns ErrQuotaExceeded.
//
// Any non-quota error (auth, network, malformed request) returns
// immediately without rotating keys or retrying: cycling keys can't fix a
// request that's simply wrong, and there's no reason to burn the cooldown
// delay on an error a retry will just reproduce.
func (p *geminiProvider) callGemini(ctx context.Context, text string) ([]byte, error) {
	cycles := p.cfg.MaxQuotaRetryCycles
	if cycles <= 0 {
		cycles = 1
	}
	cooldown := time.Duration(p.cfg.QuotaCooldownSeconds) * time.Second

	var lastErr error
	for cycle := 0; cycle < cycles; cycle++ {
		for i, key := range p.apiKeys {
			pcm, err := p.requestOnce(ctx, i, key, text)
			if err == nil {
				return pcm, nil
			}
			if !errors.Is(err, errKeyQuotaExceeded) {
				return nil, err
			}
			lastErr = err
		}
		if cycle < cycles-1 {
			p.logger.Info("tts: gemini: all keys exhausted this pass, cooling down before retry",
				"cycle", cycle+1, "of_cycles", cycles, "cooldown", cooldown)
			select {
			case <-time.After(cooldown):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, fmt.Errorf("%w: all %d configured key(s) exhausted across %d cycle(s): %v", ErrQuotaExceeded, len(p.apiKeys), cycles, lastErr)
}

// requestOnce makes exactly one generateContent call with one API key and
// returns the raw PCM audio decoded from the response's
// candidates[0].content.parts[0].inlineData.data (base64). Every attempt —
// success or failure — is logged with its wall-clock duration and the
// chunk's length (never the chunk text itself, and never the key value) so
// a local test run shows exactly how long each Gemini round trip took and
// which key/cycle produced it.
func (p *geminiProvider) requestOnce(ctx context.Context, keyIndex int, apiKey, text string) (pcm []byte, err error) {
	start := time.Now()
	defer func() {
		fields := []any{
			"model", p.cfg.Model, "voice", p.cfg.Voice,
			"key_index", keyIndex, "chunk_chars", len([]rune(text)),
			"duration_ms", time.Since(start).Milliseconds(),
		}
		switch {
		case err == nil:
			p.logger.Info("tts: gemini: request succeeded", fields...)
		case errors.Is(err, errKeyQuotaExceeded):
			p.logger.Warn("tts: gemini: key quota exceeded, trying next key", append(fields, "error", err)...)
		default:
			p.logger.Error("tts: gemini: request failed", append(fields, "error", err)...)
		}
	}()

	reqBody := geminiRequest{
		Contents: []geminiContent{{Parts: []geminiPart{{Text: text}}}},
		GenerationConfig: geminiGenerationConfig{
			ResponseModalities: []string{"AUDIO"},
			SpeechConfig: geminiSpeechConfig{
				VoiceConfig: geminiVoiceConfig{
					PrebuiltVoiceConfig: geminiPrebuiltVoiceConfig{VoiceName: p.cfg.Voice},
				},
			},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("tts: gemini: marshal request: %w", err)
	}

	url := fmt.Sprintf(geminiGenerateContentURLFormat, geminiAPIBaseURL, p.cfg.Model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tts: gemini: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", apiKey)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("tts: gemini: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tts: gemini: read response: %w", err)
	}

	var parsed geminiResponse
	// Best-effort: a non-JSON error body (e.g. an upstream proxy's plain-text
	// 429 page) still falls through to the status-code check below rather
	// than failing to decode.
	_ = json.Unmarshal(respBody, &parsed)

	if resp.StatusCode == http.StatusTooManyRequests || (parsed.Error != nil && parsed.Error.Status == "RESOURCE_EXHAUSTED") {
		return nil, fmt.Errorf("%w (http %d): %s", errKeyQuotaExceeded, resp.StatusCode, string(respBody))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tts: gemini: unexpected status %d: %s", resp.StatusCode, string(respBody))
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("tts: gemini: response had no audio content: %s", string(respBody))
	}

	inline := parsed.Candidates[0].Content.Parts[0].InlineData
	pcm, err = base64.StdEncoding.DecodeString(inline.Data)
	if err != nil {
		return nil, fmt.Errorf("tts: gemini: decode inline audio data: %w", err)
	}
	return pcm, nil
}

// The types below mirror exactly the documented generateContent request and
// response shape for speech generation
// (https://ai.google.dev/gemini-api/docs/speech-generation):
//
//	POST /v1beta/models/{model}:generateContent
//	header: x-goog-api-key: <key>
//	body: {"contents":[{"parts":[{"text":"..."}]}],
//	       "generationConfig":{"responseModalities":["AUDIO"],
//	                            "speechConfig":{"voiceConfig":{"prebuiltVoiceConfig":{"voiceName":"..."}}}}}
//
// A successful response's candidates[0].content.parts[0].inlineData.data is
// base64 PCM (24kHz/16-bit/mono — see audio.go's geminiPCM* constants), and
// inlineData.mimeType describes it (e.g. "audio/L16;rate=24000") but isn't
// otherwise parsed, since that shape doesn't vary for this API.

type geminiRequest struct {
	Contents         []geminiContent        `json:"contents"`
	GenerationConfig geminiGenerationConfig `json:"generationConfig"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenerationConfig struct {
	ResponseModalities []string           `json:"responseModalities"`
	SpeechConfig       geminiSpeechConfig `json:"speechConfig"`
}

type geminiSpeechConfig struct {
	VoiceConfig geminiVoiceConfig `json:"voiceConfig"`
}

type geminiVoiceConfig struct {
	PrebuiltVoiceConfig geminiPrebuiltVoiceConfig `json:"prebuiltVoiceConfig"`
}

type geminiPrebuiltVoiceConfig struct {
	VoiceName string `json:"voiceName"`
}

type geminiResponse struct {
	Candidates []geminiCandidate `json:"candidates"`
	Error      *geminiError      `json:"error,omitempty"`
}

type geminiCandidate struct {
	Content geminiContentResp `json:"content"`
}

type geminiContentResp struct {
	Parts []geminiPartResp `json:"parts"`
}

type geminiPartResp struct {
	InlineData geminiInlineData `json:"inlineData"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// geminiError is the shape of Gemini's error responses, e.g.
// {"error":{"code":429,"message":"...","status":"RESOURCE_EXHAUSTED"}}.
type geminiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}
