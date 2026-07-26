// Package tts turns assistant text into speech. Two providers exist:
// yandex_station_text (the default — Yandex Station's own built-in voice,
// dispatched through HA's media_player.play_media with media_content_type
// "text") and the opt-in gemini_tts (renders audio via Gemini's API and
// plays it back through the same station as a fetched file — see
// gemini.go). Dispatcher enqueues text onto a background Player so
// synthesis and physical playback never block the agent loop's turn (see
// player.go), trying a configured fallback provider if the primary reports
// its quota exhausted.
package tts

import (
	"context"
	"errors"
	"log/slog"

	"github.com/archer-developer/miranda/internal/hub"
)

// HAClient is the subset of *ha.Client the tts package needs, as an
// interface so tests can substitute a fake instead of a real Home Assistant
// instance.
type HAClient interface {
	CallService(ctx context.Context, domain, service string, data map[string]any) error
	// AliceState returns entityID's alice_state attribute ("IDLE" while not
	// speaking, "NONE" while an announcement plays — note the uppercase).
	// See internal/ha.Client.AliceState for why waitIdle needs this
	// specific attribute rather than the entity's top-level media_player
	// state.
	AliceState(ctx context.Context, entityID string) (string, error)
}

// Dispatcher is the agent loop's one entry point into TTS. Speak enqueues
// text onto a background Player and returns immediately; the Player's
// worker goroutine eventually synthesizes/plays it via primary, falling
// back to fallback if primary's quota is exhausted (see speakOne).
type Dispatcher struct {
	primary  Provider
	fallback Provider // nil if no tts.fallback is configured, or it resolves to the same provider as primary
	hub      *hub.Hub
	logger   *slog.Logger
	player   *Player
}

// NewDispatcher wires primary/fallback providers into a Dispatcher backed by
// a background Player (see player.go for the queue/interrupt mechanics, and
// speakOne for the primary->fallback selection logic). entities is the set
// of physical Yandex Station media_player entities Stop calls
// media_player.media_stop on, regardless of which provider queued the audio
// currently playing.
func NewDispatcher(primary, fallback Provider, ha HAClient, entities []string, h *hub.Hub, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	d := &Dispatcher{primary: primary, fallback: fallback, hub: h, logger: logger}
	d.player = newPlayer(ha, entities, d.speakOne, h, logger)
	return d
}

// Speak enqueues text to be spoken as soon as the background Player gets to
// it — it never blocks on synthesis or physical playback, so a slow or
// quota-throttled TTS provider can never stall the turn. Playback failures
// surface as hub events (Source: "tts"), not a returned error — see
// Player.run.
func (d *Dispatcher) Speak(ctx context.Context, text string) {
	d.player.Enqueue(text)
}

// Stop clears any not-yet-spoken queued text and stops whatever chunk is
// currently audible on the physical station(s) — see Player.Stop. Used both
// by the stop_speech tool and automatically at the start of every ha_assist
// turn (barge-in): a new voice turn always interrupts whatever a previous
// turn is still finishing instead of queuing after it.
func (d *Dispatcher) Stop(ctx context.Context) {
	d.player.Stop(ctx)
}

// speakOne is the Player's worker callback: try the primary provider, and —
// only if it reports its quota exhausted (ErrQuotaExceeded) and a fallback
// is configured — publish a hub warning and retry once via the fallback
// provider with the same text. Any other error is returned as-is: there's
// no reason to believe a different provider would succeed where the primary
// failed for an auth/network/malformed-request reason.
func (d *Dispatcher) speakOne(ctx context.Context, text string) error {
	err := d.primary.Speak(ctx, text)
	if err == nil || !errors.Is(err, ErrQuotaExceeded) || d.fallback == nil {
		return err
	}
	if d.hub != nil {
		d.hub.Publish(hub.Event{Source: "tts", Message: "primary TTS provider quota exceeded, falling back: " + err.Error()})
	}
	return d.fallback.Speak(ctx, text)
}
