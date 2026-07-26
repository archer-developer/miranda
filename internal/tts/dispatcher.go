package tts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/archer-developer/miranda/internal/config"
)

// errPollTimeout is returned internally by pollUntil when its timeout
// elapses before match ever returns true; callers decide whether that's
// fatal.
var errPollTimeout = errors.New("tts: poll timed out")

// HAClient is the subset of *ha.Client the dispatcher needs, as an interface
// so tests can substitute a fake instead of a real Home Assistant instance.
type HAClient interface {
	CallService(ctx context.Context, domain, service string, data map[string]any) error
	State(ctx context.Context, entityID string) (string, error)
}

// Dispatcher sends assistant replies to the configured TTS channel(s).
type Dispatcher struct {
	cfg    config.TTSConfig
	ha     HAClient
	logger *slog.Logger
	// sem is a size-1 semaphore serializing Speak calls to the shared
	// speaker(s) — see Speak.
	sem chan struct{}
}

// NewDispatcher builds a Dispatcher from the agent's TTS config. logger may
// be nil (defaults to slog.Default()).
func NewDispatcher(cfg config.TTSConfig, haClient HAClient, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{cfg: cfg, ha: haClient, logger: logger, sem: make(chan struct{}, 1)}
}

// Speak sends a complete piece of text to the primary channel (Yandex
// Station, chunked to its character limit).
//
// Concurrent Speak calls are queued, not interleaved: two agent turns can
// legitimately run at once (e.g. a household member retries a request that
// was still in flight, or two people talk to Miranda around the same time),
// and each one's caller now runs on a context detached from its own inbound
// connection (see internal/httpapi's turnTimeout) specifically so a slow
// turn keeps completing even if its caller gave up — which makes overlap
// with a fresh retry more likely, not less. Without this lock, two Speak
// calls interleaving their play_media/wait-idle sequences on the same
// entity would each observe state transitions caused by the *other* call,
// which can hang either one's waitIdle indefinitely (the entity update
// either poller is waiting for never cleanly arrives) or reproduce the
// original chunk-cutoff bug between two different replies.
func (d *Dispatcher) Speak(ctx context.Context, text string) error {
	if d.cfg.Primary != "yandex_station" || len(d.cfg.YandexStation.Entities) == 0 {
		return fmt.Errorf("tts: no usable channel configured (primary=%s)", d.cfg.Primary)
	}
	select {
	case d.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-d.sem }()

	return d.speakYandexStation(ctx, text)
}

// speakYandexStation sends text, chunked to the station's character limit,
// to every configured entity, waiting between chunks so consecutive
// play_media calls don't interrupt each other.
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
			if err := d.waitIdle(ctx, entity, chunk); err != nil {
				return err
			}
		}
	}
	return nil
}

// waitIdle waits for entity to (likely) finish playing chunk before the
// caller sends the next one, so consecutive play_media calls don't
// interrupt each other.
//
// This is a two-phase wait. Phase 1 waits for the entity to actually leave
// "idle" (proof this chunk started playing) rather than trusting it
// immediately: right after play_media returns, the entity typically still
// reports the *previous* "idle" state for a beat, because synthesis and the
// station's own wake-up take real time, and polling for "idle" without this
// phase can land in that window and wrongly conclude the chunk we just sent
// is already done — firing the next chunk's play_media mid-utterance and
// cutting the current one off.
//
// Phase 2 is bounded by an estimated speech duration for chunk
// (SpeechCharsPerSecond/SpeechMarginMS), not an unconditional wait for
// "idle": direct observation on a real Yandex Station/AlexxIT integration
// setup showed the entity can get stuck reporting "playing" indefinitely
// after a media_content_type: text announcement and never go back to
// "idle" at all — polling for that would hang forever. If the entity does
// report idle before the estimate elapses, this still returns immediately;
// the estimate is only a fallback ceiling for setups where that signal
// never arrives.
func (d *Dispatcher) waitIdle(ctx context.Context, entity, chunk string) error {
	interval := time.Duration(d.cfg.YandexStation.IdlePollIntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = 300 * time.Millisecond
	}
	startTimeout := time.Duration(d.cfg.YandexStation.PlaybackStartTimeoutMS) * time.Millisecond
	if startTimeout <= 0 {
		startTimeout = 3 * time.Second
	}

	// Give up waiting for playback to start after startTimeout rather than
	// hang forever — the chunk may have been too short to observe leaving
	// idle, or playback may have failed silently upstream. Either way, fall
	// through to the idle-wait below, which is a harmless no-op if the
	// chunk already finished (or never started).
	err := d.pollUntil(ctx, entity, "leave idle", interval, startTimeout, func(s string) bool { return s != "idle" })
	if err != nil && !errors.Is(err, errPollTimeout) {
		return err
	}
	if errors.Is(err, errPollTimeout) {
		d.logger.Warn("tts: entity never left idle within playback_start_timeout_ms, proceeding anyway", "entity", entity, "timeout", startTimeout)
	}

	speechTimeout := estimateSpeechDuration(len([]rune(chunk)), d.cfg.YandexStation)
	err = d.pollUntil(ctx, entity, "return to idle", interval, speechTimeout, func(s string) bool { return s == "idle" })
	if errors.Is(err, errPollTimeout) {
		d.logger.Warn("tts: entity never returned to idle within the estimated speech duration, proceeding anyway",
			"entity", entity, "timeout", speechTimeout, "chunk_len", len([]rune(chunk)))
		return nil
	}
	return err
}

// estimateSpeechDuration estimates how long the station takes to speak
// chunkLen characters of text, as a fallback ceiling for waitIdle's second
// phase (see its doc comment) — deliberately conservative (a slow rate, plus
// a fixed margin) since underestimating reproduces the original chunk-cutoff
// bug, while overestimating only costs a bit of extra silence between
// chunks.
func estimateSpeechDuration(chunkLen int, cfg config.YandexStationConfig) time.Duration {
	rate := cfg.SpeechCharsPerSecond
	if rate <= 0 {
		rate = 12
	}
	margin := time.Duration(cfg.SpeechMarginMS) * time.Millisecond
	if margin <= 0 {
		margin = 2 * time.Second
	}
	return time.Duration(float64(chunkLen)/rate*float64(time.Second)) + margin
}

// pollUntil polls entity's state every interval until match(state) is true,
// ctx is cancelled, or (if timeout > 0) timeout elapses without a match —
// in which case it returns errPollTimeout. label identifies which of
// waitIdle's two phases this is, purely for the state-transition log lines
// below — the tool for seeing exactly what a real device reported when a
// wait doesn't resolve the way we expect.
func (d *Dispatcher) pollUntil(ctx context.Context, entity, label string, interval, timeout time.Duration, match func(state string) bool) error {
	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastState := ""
	for {
		state, err := d.ha.State(ctx, entity)
		if err != nil {
			return fmt.Errorf("tts: poll state of %s: %w", entity, err)
		}
		if state != lastState {
			d.logger.Info("tts: entity state", "entity", entity, "waiting_for", label, "state", state)
			lastState = state
		}
		if match(state) {
			return nil
		}
		select {
		case <-ticker.C:
		case <-deadline:
			return errPollTimeout
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
