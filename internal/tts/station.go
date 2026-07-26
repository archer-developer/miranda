package tts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// errPollTimeout is returned internally by pollUntil when its timeout
// elapses before match ever returns true; callers decide whether that's
// fatal.
var errPollTimeout = errors.New("tts: poll timed out")

// stationPlayer drives Home Assistant's media_player.play_media service for
// a Yandex Station entity and waits for on-device playback to actually
// finish before returning, regardless of what produced the audio — Yandex's
// own built-in TTS via media_content_type "text" (textProvider, via
// playAndWait, which polls the entity's alice_state attribute), or a
// pre-rendered file served over HTTP (geminiProvider, via playAndSleep,
// which just waits out the file's known duration). These two playback
// mechanisms are not interchangeable: confirmed by direct observation
// against a real device, alice_state cleanly tracks a "text"/"dialog"
// announcement's lifecycle but never changes at all for the
// "radio_play"/externalCommandBypass mechanism URL playback goes through —
// there is no HA-exposed signal waitIdle could poll for geminiProvider's
// case, which is why it doesn't try to.
type stationPlayer struct {
	ha     HAClient
	logger *slog.Logger

	idlePollInterval     time.Duration
	playbackStartTimeout time.Duration
}

// newStationPlayer builds a stationPlayer from the raw millisecond config
// values every YandexStationConfig carries, applying the same fallback
// defaults the original dispatcher's waitIdle used.
func newStationPlayer(ha HAClient, logger *slog.Logger, idlePollIntervalMS, playbackStartTimeoutMS int) *stationPlayer {
	if logger == nil {
		logger = slog.Default()
	}
	interval := time.Duration(idlePollIntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = 300 * time.Millisecond
	}
	startTimeout := time.Duration(playbackStartTimeoutMS) * time.Millisecond
	if startTimeout <= 0 {
		startTimeout = 3 * time.Second
	}
	return &stationPlayer{ha: ha, logger: logger, idlePollInterval: interval, playbackStartTimeout: startTimeout}
}

// playAndWait sends one play_media call to entity (contentID/contentType are
// media_content_id/media_content_type — a chunk of text for textProvider)
// and waits for it to finish before returning, so a caller looping over
// multiple chunks/entities never fires the next play_media while this one
// is still audible. Only textProvider uses this: it polls the entity's
// alice_state attribute, which reliably tracks a "text"/"dialog"
// announcement's lifecycle but not the "radio_play" mechanism URL playback
// goes through — see playAndSleep, and pcmDurationMS's doc comment in
// audio.go, for why geminiProvider can't use this method.
func (s *stationPlayer) playAndWait(ctx context.Context, entity, contentID, contentType string) error {
	if err := s.callPlayMedia(ctx, entity, contentID, contentType); err != nil {
		return err
	}
	return s.waitIdle(ctx, entity)
}

// playAndSleep sends one play_media call to entity and then simply waits
// duration (plus geminiPlaybackSafetyMargin) before returning, rather than
// polling any HA-exposed state — confirmed by direct observation against a
// real device that neither the entity's top-level state nor its alice_state
// attribute ever changes for the "radio_play" externalCommandBypass
// mechanism a URL played via media_player.play_media goes through, unlike a
// "text"/"dialog" announcement (see playAndWait). geminiProvider always
// knows duration exactly, up front, since it controls the raw PCM it
// rendered (or cached) the file from — see audio.go's pcmDurationMS — so
// there's no state left to poll for in the first place.
func (s *stationPlayer) playAndSleep(ctx context.Context, entity, contentID, contentType string, duration time.Duration) error {
	if err := s.callPlayMedia(ctx, entity, contentID, contentType); err != nil {
		return err
	}
	select {
	case <-time.After(duration + geminiPlaybackSafetyMargin):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// callPlayMedia is the media_player.play_media service call shared by
// playAndWait and playAndSleep.
func (s *stationPlayer) callPlayMedia(ctx context.Context, entity, contentID, contentType string) error {
	if err := s.ha.CallService(ctx, "media_player", "play_media", map[string]any{
		"entity_id":          entity,
		"media_content_id":   contentID,
		"media_content_type": contentType,
	}); err != nil {
		return fmt.Errorf("tts: yandex station %s: %w", entity, err)
	}
	return nil
}

// waitIdle waits for entity to finish playing whatever was just sent to it,
// so the next play_media call isn't fired while the station is still
// speaking/playing this one. It polls entity's alice_state attribute, not
// its top-level media_player state — see internal/ha.Client.AliceState for
// why: the top-level state tracks whatever the underlying media session
// (e.g. background music) is doing and doesn't reliably reflect a TTS
// announcement at all, while alice_state cleanly tracks it regardless.
//
// This is a two-phase wait, not a single poll-for-idle loop: right after
// play_media returns, the entity typically still reports the *previous*
// "IDLE" alice_state for a beat, because synthesis and the station's own
// wake-up take real time. A single poll-for-idle loop can land in that
// window, wrongly conclude playback of the chunk we just sent is already
// done, and fire the next chunk's play_media immediately — which interrupts
// the station mid-utterance. That's exactly what produced the originally
// reported bug: long replies got cut mid-sentence, with the tail end only
// surfacing once the *next* chunk started playing. So we first wait for the
// entity to actually leave "IDLE" (proof this chunk started), then wait for
// it to return to "IDLE" (proof it finished).
func (s *stationPlayer) waitIdle(ctx context.Context, entity string) error {
	// Give up waiting for playback to start after playbackStartTimeout
	// rather than hang forever — the chunk may have been too short to
	// observe leaving idle, or playback may have failed silently upstream.
	// Either way, fall through to the idle-wait below, which is a harmless
	// no-op if the chunk already finished (or never started).
	err := s.pollUntil(ctx, entity, "leave idle", s.idlePollInterval, s.playbackStartTimeout, func(state string) bool { return state != "IDLE" })
	if err != nil && !errors.Is(err, errPollTimeout) {
		return err
	}
	if errors.Is(err, errPollTimeout) {
		s.logger.Warn("tts: entity never left idle within playback_start_timeout_ms, proceeding anyway", "entity", entity, "timeout", s.playbackStartTimeout)
	}

	return s.pollUntil(ctx, entity, "return to idle", s.idlePollInterval, 0, func(state string) bool { return state == "IDLE" })
}

// pollUntil polls entity's alice_state every interval until match(state) is
// true, ctx is cancelled, or (if timeout > 0) timeout elapses without a
// match — in which case it returns errPollTimeout. label identifies which of
// waitIdle's two phases this is, purely for the state-transition log lines
// below — the tool for seeing exactly what a real device reported when a
// wait doesn't resolve the way we expect.
func (s *stationPlayer) pollUntil(ctx context.Context, entity, label string, interval, timeout time.Duration, match func(state string) bool) error {
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
		state, err := s.ha.AliceState(ctx, entity)
		if err != nil {
			return fmt.Errorf("tts: poll state of %s: %w", entity, err)
		}
		if state != lastState {
			s.logger.Info("tts: entity alice_state", "entity", entity, "waiting_for", label, "alice_state", state)
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
