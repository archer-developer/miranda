package tts

import (
	"context"
	"fmt"
	"time"

	"github.com/archer-developer/miranda/internal/config"
)

// HAClient is the subset of *ha.Client the dispatcher needs, as an interface
// so tests can substitute a fake instead of a real Home Assistant instance.
type HAClient interface {
	CallService(ctx context.Context, domain, service string, data map[string]any) error
	State(ctx context.Context, entityID string) (string, error)
}

// Dispatcher sends assistant replies to the configured TTS channel(s).
type Dispatcher struct {
	cfg config.TTSConfig
	ha  HAClient
}

// NewDispatcher builds a Dispatcher from the agent's TTS config.
func NewDispatcher(cfg config.TTSConfig, haClient HAClient) *Dispatcher {
	return &Dispatcher{cfg: cfg, ha: haClient}
}

// Speak sends a complete piece of text to the primary channel (Yandex
// Station, chunked to its character limit).
func (d *Dispatcher) Speak(ctx context.Context, text string) error {
	if d.cfg.Primary != "yandex_station" || len(d.cfg.YandexStation.Entities) == 0 {
		return fmt.Errorf("tts: no usable channel configured (primary=%s)", d.cfg.Primary)
	}
	return d.speakYandexStation(ctx, text)
}

// speakYandexStation sends text, chunked to the station's character limit,
// to every configured entity, waiting for playback to reach "idle" between
// chunks so consecutive play_media calls don't interrupt each other.
func (d *Dispatcher) speakYandexStation(ctx context.Context, text string) error {
	chunks := Chunk(text, d.cfg.YandexStation.ChunkMaxChars)
	for _, entity := range d.cfg.YandexStation.Entities {
		for _, chunk := range chunks {
			if err := d.ha.CallService(ctx, "media_player", "play_media", map[string]any{
				"entity_id":          entity,
				"media_content_id":   chunk,
				"media_content_type": "text", // NOT "dialog" — that reopens the station's own mic
			}); err != nil {
				return fmt.Errorf("tts: yandex station %s: %w", entity, err)
			}
			if err := d.waitIdle(ctx, entity); err != nil {
				return err
			}
		}
	}
	return nil
}

// waitIdle polls entity's state until it returns to "idle", so the next
// chunk isn't sent while the station is still speaking the previous one.
func (d *Dispatcher) waitIdle(ctx context.Context, entity string) error {
	interval := time.Duration(d.cfg.YandexStation.IdlePollIntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = 300 * time.Millisecond
	}

	// Wait one interval before the first poll: right after play_media the
	// station is still "idle" from before playback started, so polling
	// immediately would return early.
	select {
	case <-time.After(interval):
	case <-ctx.Done():
		return ctx.Err()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		state, err := d.ha.State(ctx, entity)
		if err != nil {
			return fmt.Errorf("tts: poll state of %s: %w", entity, err)
		}
		if state == "idle" {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
