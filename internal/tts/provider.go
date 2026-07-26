package tts

import (
	"context"
	"errors"
)

// Provider synthesizes (if needed) and plays one already-chunked-by-the-
// caller piece of text through some TTS channel. Speak blocks until
// playback is confirmed complete on the physical device (or ctx is
// cancelled) — Dispatcher's background Player is what keeps that blocking
// off the agent loop's own turn, not Provider itself.
//
// Two implementations exist: textProvider (yandex_text.go) hands text
// straight to Yandex Station's own built-in voice, and geminiProvider
// (gemini.go) renders it via Gemini's API first and hands the station a
// URL to fetch and play instead.
type Provider interface {
	// Speak synthesizes (if needed) and plays text. Returns ErrQuotaExceeded
	// specifically when this provider's own quota (e.g. every configured
	// Gemini API key, across every retry cycle) is exhausted, so
	// Dispatcher.speakOne knows a fallback provider is worth trying instead
	// of just failing the turn's speech outright.
	Speak(ctx context.Context, text string) error
}

// ErrQuotaExceeded is the sentinel a Provider's Speak returns when every
// avenue it has to synthesize speech has been exhausted by quota errors.
// Dispatcher checks for this specific error (via errors.Is) before falling
// back to a secondary provider — a quota error is the one failure mode
// where a *different* provider might succeed where the primary just
// failed; any other error (auth, network, malformed request) is not, so
// falling back on those would just fail identically (or worse) on a
// provider that was never the issue.
var ErrQuotaExceeded = errors.New("tts: provider quota exceeded")
