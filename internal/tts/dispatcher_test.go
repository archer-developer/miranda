package tts

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
)

// fakeHA is an in-memory HAClient: play_media calls are recorded and State
// immediately reports "idle" so tests run without waiting on real timers
// beyond the dispatcher's configured (tiny, in tests) poll interval.
type fakeHA struct {
	mu         sync.Mutex
	calls      []call
	failDomain string // if set, CallService for this domain returns an error
}

type call struct {
	domain, service string
	data            map[string]any
}

func (f *fakeHA) CallService(ctx context.Context, domain, service string, data map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDomain != "" && domain == f.failDomain {
		return fmt.Errorf("%s unreachable", domain)
	}
	f.calls = append(f.calls, call{domain, service, data})
	return nil
}

func (f *fakeHA) State(ctx context.Context, entityID string) (string, error) {
	return "idle", nil
}

func yandexConfig(entities ...string) config.TTSConfig {
	return config.TTSConfig{
		Primary: "yandex_station",
		YandexStation: config.YandexStationConfig{
			Entities:           entities,
			ChunkMaxChars:      100,
			IdlePollIntervalMS: 1, // keep tests fast
			// fakeHA.State always reports "idle", so the "wait for it to
			// leave idle" phase always times out; keep that timeout tiny too.
			PlaybackStartTimeoutMS: 1,
		},
	}
}

func TestDispatcher_SpeaksChunkedTextToEachEntity(t *testing.T) {
	ha := &fakeHA{}
	d := NewDispatcher(yandexConfig("media_player.kitchen", "media_player.bedroom"), ha)

	err := d.Speak(context.Background(), "Первое предложение. Второе предложение.")
	require.NoError(t, err)

	require.Len(t, ha.calls, 4) // 2 chunks x 2 entities
	require.Equal(t, "media_player", ha.calls[0].domain)
	require.Equal(t, "play_media", ha.calls[0].service)
	require.Equal(t, "text", ha.calls[0].data["media_content_type"])
	require.Equal(t, "Первое предложение.", ha.calls[0].data["media_content_id"])
}

func TestDispatcher_ReturnsErrorWhenYandexFails(t *testing.T) {
	ha := &fakeHA{failDomain: "media_player"}
	d := NewDispatcher(yandexConfig("media_player.kitchen"), ha)

	err := d.Speak(context.Background(), "привет")
	require.Error(t, err)
}

// scriptedHA lets a test script exactly which state media_player.State
// returns on each successive poll, and records how many State polls had
// happened by the time each play_media call fired — the tool for proving
// waitIdle's two-phase ordering without depending on real-time flakiness.
type scriptedHA struct {
	mu     sync.Mutex
	calls  []call
	states []string // consumed one per State() call; last element repeats forever

	stateCalls        int
	callsAtStateCount []int
}

func (f *scriptedHA) CallService(ctx context.Context, domain, service string, data map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{domain, service, data})
	f.callsAtStateCount = append(f.callsAtStateCount, f.stateCalls)
	return nil
}

func (f *scriptedHA) State(ctx context.Context, entityID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.stateCalls
	f.stateCalls++
	if i < len(f.states) {
		return f.states[i], nil
	}
	return f.states[len(f.states)-1], nil
}

// TestDispatcher_WaitsForPlaybackToStartBeforePollingForIdle guards against
// the regression this file's waitIdle comment describes: right after
// play_media, the entity briefly still reports the *previous* "idle" state
// before the station actually starts playing. If the dispatcher polled for
// "idle" without first confirming playback started, it would see that stale
// "idle" and immediately fire the next chunk mid-utterance, cutting the
// current one off.
func TestDispatcher_WaitsForPlaybackToStartBeforePollingForIdle(t *testing.T) {
	ha := &scriptedHA{states: []string{"idle", "playing", "playing", "idle"}}
	cfg := config.TTSConfig{
		Primary: "yandex_station",
		YandexStation: config.YandexStationConfig{
			Entities:               []string{"media_player.kitchen"},
			ChunkMaxChars:          100,
			IdlePollIntervalMS:     1,
			PlaybackStartTimeoutMS: 50,
		},
	}
	d := NewDispatcher(cfg, ha)

	err := d.Speak(context.Background(), "Первое предложение. Второе предложение.")
	require.NoError(t, err)

	require.Len(t, ha.calls, 2)
	require.Equal(t, 0, ha.callsAtStateCount[0])
	// The second chunk must only be sent after the dispatcher observed the
	// entity leave "idle" (index 1: "playing") and return to "idle" again
	// (index 3) -- i.e. after all 4 scripted states were consumed, not
	// after the single stale "idle" reading at index 0.
	require.Equal(t, 4, ha.callsAtStateCount[1])
}

func TestDispatcher_ErrorsWhenNoChannelConfigured(t *testing.T) {
	cfg := config.TTSConfig{Primary: "yandex_station"} // no entities configured
	d := NewDispatcher(cfg, &fakeHA{})

	err := d.Speak(context.Background(), "привет")
	require.Error(t, err)
}
