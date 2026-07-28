package tts

import (
	"context"
	"fmt"
	"log/slog"

	"golang.org/x/sync/errgroup"

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

// Speak sends text, chunked to the station's character limit, to all
// configured entities simultaneously per chunk — each chunk broadcasts to
// every entity in parallel, and the next chunk only starts once every entity
// has finished playing the current one (see stationPlayer.waitIdle). This
// keeps multi-room setups in sync: all speakers say each sentence together
// rather than one room finishing before the next starts.
func (p *textProvider) Speak(ctx context.Context, text string) error {
	if len(p.cfg.Entities) == 0 {
		return fmt.Errorf("tts: yandex_station_text: no entities configured")
	}
	for _, chunk := range Chunk(text, p.cfg.ChunkMaxChars) {
		g, gctx := errgroup.WithContext(ctx)
		for _, entity := range p.cfg.Entities {
			g.Go(func() error {
				return p.station.playAndWait(gctx, entity, chunk, "text")
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
	}
	return nil
}
