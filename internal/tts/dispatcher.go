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
	"fmt"
	"log/slog"
	"time"

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
	// ResolveMediaPlayer maps a media_player entity's friendly name (as
	// shown in HA's UI) to its entity_id (e.g. "Станция Мини 3 Про" →
	// "media_player.alice_mini_pro"). Used by the Dispatcher to resolve
	// TTSConfig.DefaultDevice and the speak_reply tool's optional device
	// argument. See ha.Client.ResolveMediaPlayer for the lazy-cache details.
	ResolveMediaPlayer(ctx context.Context, friendlyName string) (string, error)
}

// Dispatcher is the agent loop's one entry point into TTS. Speak enqueues
// text onto a background Player and returns immediately; the Player's
// worker goroutine eventually synthesizes/plays it via primary, falling
// back to fallback if primary's quota is exhausted (see speakOne).
type Dispatcher struct {
	primary       Provider
	fallback      Provider // nil if no tts.fallback is configured, or it resolves to the same provider as primary
	ha            HAClient
	defaultDevice string // friendly name, resolved to entity_id at speak time via ha.ResolveMediaPlayer
	hub           *hub.Hub
	logger        *slog.Logger
	player        *Player
}

// NewDispatcher wires primary/fallback providers into a Dispatcher backed by
// a background Player (see player.go for the queue/interrupt mechanics, and
// speakOne for the primary->fallback selection logic). defaultDevice is the
// friendly name of the media_player entity used when no device is specified
// per-call (see TTSConfig.DefaultDevice); empty means speech is skipped when
// no explicit device is given.
func NewDispatcher(primary, fallback Provider, ha HAClient, defaultDevice string, h *hub.Hub, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	d := &Dispatcher{primary: primary, fallback: fallback, ha: ha, defaultDevice: defaultDevice, hub: h, logger: logger}
	d.player = newPlayer(d.speakOne, h, logger)
	return d
}

// Speak enqueues text to be spoken on the configured default device — it
// never blocks on synthesis or physical playback, so a slow or
// quota-throttled TTS provider can never stall the turn. Playback failures
// surface as hub events (Source: "tts"), not a returned error — see
// Player.run. When no default device is configured, the call is silently
// skipped and a warning is published to the hub.
func (d *Dispatcher) Speak(ctx context.Context, text string) {
	d.speakToResolved(ctx, text, "")
}

// SpeakTo enqueues text to be spoken on device (a friendly name resolved via
// ha.ResolveMediaPlayer). Falls back to the configured default device when
// device is empty, same as Speak.
func (d *Dispatcher) SpeakTo(ctx context.Context, text, device string) {
	d.speakToResolved(ctx, text, device)
}

// speakToResolved resolves device to an entity_id and enqueues the item on
// the Player. When device is empty, d.defaultDevice is used; when neither
// resolves, a hub warning is published and the call is a no-op.
func (d *Dispatcher) speakToResolved(ctx context.Context, text, device string) {
	entityID := d.resolveEntity(ctx, device)
	if entityID == "" {
		if d.hub != nil {
			d.hub.Publish(hub.Event{Source: "tts", Message: "no TTS device configured or could not be resolved — skipping speech"})
		}
		return
	}
	d.player.Enqueue(text, entityID)
}

// resolveEntity maps device (a friendly name) to a media_player entity_id via
// ha.ResolveMediaPlayer. When device is empty, d.defaultDevice is tried instead.
// Returns "" if neither is set or resolution fails, publishing a hub warning in
// the latter case.
func (d *Dispatcher) resolveEntity(ctx context.Context, device string) string {
	target := device
	if target == "" {
		target = d.defaultDevice
	}
	if target == "" {
		return ""
	}
	entityID, err := d.ha.ResolveMediaPlayer(ctx, target)
	if err != nil {
		if d.hub != nil {
			d.hub.Publish(hub.Event{Source: "tts", Message: fmt.Sprintf("tts: could not resolve device %q: %v", target, err)})
		}
		return ""
	}
	return entityID
}

// Stop clears any not-yet-spoken queued text and interrupts the chunk
// currently being synthesized — see Player.Stop. Used both by the
// stop_speech tool and automatically at the start of every ha_assist turn
// (barge-in): a new voice turn always interrupts whatever a previous turn
// is still finishing instead of queuing after it. The station will finish
// its current short chunk on its own without an explicit media_stop call.
func (d *Dispatcher) Stop() {
	d.player.Stop()
}

// WaitIdle blocks until the Player has drained its queue and finished playing
// the current chunk — delegates to Player.WaitIdle. Called by executeTool
// before alice MCP tool calls to ensure any queued TTS finishes before a new
// speaker command is sent.
func (d *Dispatcher) WaitIdle(ctx context.Context) {
	d.player.WaitIdle(ctx)
}

// WaitEntityIdle polls entityID's alice_state attribute until it equals "IDLE",
// so an alice tool call targeting a speaker waits for any current playback to
// finish before firing. Used by executeTool after WaitIdle to catch the
// HA-native-command → HA-native-command case, where no Miranda-owned TTS is
// queued but the station is still busy from a previous MCP call.
func (d *Dispatcher) WaitEntityIdle(ctx context.Context, entityID string) error {
	const pollInterval = 300 * time.Millisecond
	for {
		state, err := d.ha.AliceState(ctx, entityID)
		if err != nil {
			return fmt.Errorf("tts: poll alice_state of %s: %w", entityID, err)
		}
		if state == "IDLE" {
			return nil
		}
		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// speakOne is the Player's worker callback: try the primary provider, and —
// only if it reports its quota exhausted (ErrQuotaExceeded) and a fallback
// is configured — publish a hub warning and retry once via the fallback
// provider with the same text and entityID. Any other error is returned as-is:
// there's no reason to believe a different provider would succeed where the
// primary failed for an auth/network/malformed-request reason.
func (d *Dispatcher) speakOne(ctx context.Context, text, entityID string) error {
	err := d.primary.Speak(ctx, text, entityID)
	if err == nil || !errors.Is(err, ErrQuotaExceeded) || d.fallback == nil {
		return err
	}
	if d.hub != nil {
		d.hub.Publish(hub.Event{Source: "tts", Message: "primary TTS provider quota exceeded, falling back: " + err.Error()})
	}
	return d.fallback.Speak(ctx, text, entityID)
}
