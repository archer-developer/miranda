package tts

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/archer-developer/miranda/internal/config"
)

// textProvider dispatches text directly to Yandex Station's own built-in
// text-to-speech via media_content_type "text" (never "dialog" — that
// reopens the station's own mic and conflicts with the voice pipeline, see
// CLAUDE.md). This is the original (and still default) TTS channel: its
// behavior is unchanged from before Provider existed as an interface — it
// was Dispatcher.speakYandexStation until Gemini TTS became a second
// option, at which point it moved here as its own type so Dispatcher could
// become a thin primary/fallback selector (see dispatcher.go).
type textProvider struct {
	cfg     config.YandexStationConfig
	station *stationPlayer
}

// NewTextProvider builds the yandex_station_text Provider.
func NewTextProvider(cfg config.YandexStationConfig, ha HAClient, logger *slog.Logger) Provider {
	return &textProvider{
		cfg:     cfg,
		station: newStationPlayer(ha, logger, cfg.IdlePollIntervalMS, cfg.PlaybackStartTimeoutMS),
	}
}

// Speak sends text, chunked to the station's character limit, to every
// configured entity in turn, waiting for each chunk to finish playing
// before sending the next (see stationPlayer.waitIdle).
func (p *textProvider) Speak(ctx context.Context, text string) error {
	if len(p.cfg.Entities) == 0 {
		return fmt.Errorf("tts: yandex_station_text: no entities configured")
	}
	chunks := Chunk(text, p.cfg.ChunkMaxChars)
	for _, entity := range p.cfg.Entities {
		for _, chunk := range chunks {
			if err := p.station.playAndWait(ctx, entity, chunk, "text"); err != nil {
				return err
			}
		}
	}
	return nil
}
