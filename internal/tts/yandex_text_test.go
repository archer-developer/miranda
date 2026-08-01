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

func (f *fakeHA) ResolveMediaPlayer(ctx context.Context, friendlyName string) (string, error) {
	return "media_player." + friendlyName, nil
}

func (f *fakeHA) Calls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]call, len(f.calls))
	copy(out, f.calls)
	return out
}

func yandexConfig() config.YandexStationConfig {
	return config.YandexStationConfig{
		ChunkMaxChars:      100,
		IdlePollIntervalMS: 1, // keep tests fast
		// fakeHA.AliceState always reports "IDLE", so the "wait for it to
		// leave idle" phase always times out; keep that timeout tiny too.
		PlaybackStartTimeoutMS: 1,
	}
}

func TestTextProvider_SpeaksChunkedTextInOrderToEntity(t *testing.T) {
	ha := &fakeHA{}
	cfg := yandexConfig()
	// Small enough to force two chunks — see Accumulator's doc comment for
	// why chunking is maximally greedy and a generous limit would merge them.
	cfg.ChunkMaxChars = 25
	p := NewTextProvider(cfg, ha, nil)

	err := p.Speak(context.Background(), "Первое предложение. Второе предложение.", "media_player.kitchen")
	require.NoError(t, err)

	calls := ha.Calls()
	require.Len(t, calls, 2) // 2 chunks, 1 entity
	for _, c := range calls {
		require.Equal(t, "media_player", c.domain)
		require.Equal(t, "play_media", c.service)
		require.Equal(t, "text", c.data["media_content_type"])
		require.Equal(t, "media_player.kitchen", c.data["entity_id"])
	}

	chunkOf := func(c call) string { return c.data["media_content_id"].(string) }
	require.Equal(t, "Первое предложение.", chunkOf(calls[0]))
	require.Equal(t, "Второе предложение.", chunkOf(calls[1]))
}

func TestTextProvider_ReturnsErrorWhenYandexFails(t *testing.T) {
	ha := &fakeHA{failDomain: "media_player"}
	p := NewTextProvider(yandexConfig(), ha, nil)

	err := p.Speak(context.Background(), "привет", "media_player.kitchen")
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

func (f *scriptedHA) ResolveMediaPlayer(ctx context.Context, friendlyName string) (string, error) {
	return "media_player." + friendlyName, nil
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
		// Small enough to force two chunks — see the comment in
		// TestTextProvider_SpeaksChunkedTextInOrderToEntity for why.
		ChunkMaxChars:          25,
		IdlePollIntervalMS:     1,
		PlaybackStartTimeoutMS: 50,
	}
	p := NewTextProvider(cfg, ha, nil)

	err := p.Speak(context.Background(), "Первое предложение. Второе предложение.", "media_player.kitchen")
	require.NoError(t, err)

	require.Len(t, ha.calls, 2)
	require.Equal(t, 0, ha.callsAtStateCount[0])
	// The second chunk must only be sent after playback observed the entity
	// leave "IDLE" (index 1: "NONE") and return to "IDLE" again (index 3) --
	// i.e. after all 4 scripted states were consumed, not after the single
	// stale "IDLE" reading at index 0.
	require.Equal(t, 4, ha.callsAtStateCount[1])
}
