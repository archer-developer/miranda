package tts

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
)

// fakeHA is an in-memory HAClient: play_media calls are recorded and
// AliceState immediately reports "IDLE" so tests run without waiting on real
// timers beyond the configured (tiny, in tests) poll interval.
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

func (f *fakeHA) AliceState(ctx context.Context, entityID string) (string, error) {
	return "IDLE", nil
}

func (f *fakeHA) Calls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]call, len(f.calls))
	copy(out, f.calls)
	return out
}

func yandexConfig(entities ...string) config.YandexStationConfig {
	return config.YandexStationConfig{
		Entities:           entities,
		ChunkMaxChars:      100,
		IdlePollIntervalMS: 1, // keep tests fast
		// fakeHA.AliceState always reports "IDLE", so the "wait for it to
		// leave idle" phase always times out; keep that timeout tiny too.
		PlaybackStartTimeoutMS: 1,
	}
}

func TestTextProvider_SpeaksChunkedTextToEachEntity(t *testing.T) {
	ha := &fakeHA{}
	cfg := yandexConfig("media_player.kitchen", "media_player.bedroom")
	// Small enough that the two sentences don't both fit in one chunk —
	// chunking is always maximally greedy (see Accumulator's doc comment),
	// so a limit generous enough for both would merge them into a single
	// chunk instead of exercising the "one call per chunk per entity" path
	// this test is for.
	cfg.ChunkMaxChars = 25
	p := NewTextProvider(cfg, ha, nil)

	err := p.Speak(context.Background(), "Первое предложение. Второе предложение.")
	require.NoError(t, err)

	calls := ha.Calls()
	require.Len(t, calls, 4) // 2 chunks x 2 entities
	require.Equal(t, "media_player", calls[0].domain)
	require.Equal(t, "play_media", calls[0].service)
	require.Equal(t, "text", calls[0].data["media_content_type"])
	require.Equal(t, "Первое предложение.", calls[0].data["media_content_id"])
}

func TestTextProvider_ReturnsErrorWhenYandexFails(t *testing.T) {
	ha := &fakeHA{failDomain: "media_player"}
	p := NewTextProvider(yandexConfig("media_player.kitchen"), ha, nil)

	err := p.Speak(context.Background(), "привет")
	require.Error(t, err)
}

func TestTextProvider_ErrorsWhenNoEntitiesConfigured(t *testing.T) {
	p := NewTextProvider(yandexConfig(), &fakeHA{}, nil) // no entities configured

	err := p.Speak(context.Background(), "привет")
	require.Error(t, err)
}

// scriptedHA lets a test script exactly which value AliceState returns on
// each successive poll, and records how many AliceState polls had happened
// by the time each play_media call fired — the tool for proving waitIdle's
// two-phase ordering without depending on real-time flakiness.
type scriptedHA struct {
	mu     sync.Mutex
	calls  []call
	states []string // consumed one per AliceState() call; last element repeats forever

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

func (f *scriptedHA) AliceState(ctx context.Context, entityID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.stateCalls
	f.stateCalls++
	if i < len(f.states) {
		return f.states[i], nil
	}
	return f.states[len(f.states)-1], nil
}

// TestTextProvider_WaitsForPlaybackToStartBeforePollingForIdle guards against
// the regression stationPlayer.waitIdle's doc comment describes: right after
// play_media, the entity briefly still reports the *previous* "IDLE"
// alice_state before the station actually starts speaking. If Speak polled
// for "IDLE" without first confirming playback started, it would see that
// stale "IDLE" and immediately fire the next chunk mid-utterance, cutting
// the current one off.
func TestTextProvider_WaitsForPlaybackToStartBeforePollingForIdle(t *testing.T) {
	ha := &scriptedHA{states: []string{"IDLE", "NONE", "NONE", "IDLE"}}
	cfg := config.YandexStationConfig{
		Entities: []string{"media_player.kitchen"},
		// Small enough to force two chunks — see the comment in
		// TestTextProvider_SpeaksChunkedTextToEachEntity for why.
		ChunkMaxChars:          25,
		IdlePollIntervalMS:     1,
		PlaybackStartTimeoutMS: 50,
	}
	p := NewTextProvider(cfg, ha, nil)

	err := p.Speak(context.Background(), "Первое предложение. Второе предложение.")
	require.NoError(t, err)

	require.Len(t, ha.calls, 2)
	require.Equal(t, 0, ha.callsAtStateCount[0])
	// The second chunk must only be sent after playback observed the entity
	// leave "IDLE" (index 1: "NONE") and return to "IDLE" again (index 3) --
	// i.e. after all 4 scripted states were consumed, not after the single
	// stale "IDLE" reading at index 0.
	require.Equal(t, 4, ha.callsAtStateCount[1])
}
