package tts

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

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
	d := NewDispatcher(yandexConfig("media_player.kitchen", "media_player.bedroom"), ha, nil)

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
	d := NewDispatcher(yandexConfig("media_player.kitchen"), ha, nil)

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
	d := NewDispatcher(cfg, ha, nil)

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

// stuckPlayingHA simulates a real, observed Yandex Station/AlexxIT
// integration failure mode: the entity correctly leaves "idle" once
// play_media is called, but then never reports "idle" again for the rest of
// the test, no matter how long we wait.
type stuckPlayingHA struct {
	mu    sync.Mutex
	calls []string
}

func (f *stuckPlayingHA) CallService(ctx context.Context, domain, service string, data map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, data["media_content_id"].(string))
	return nil
}

func (f *stuckPlayingHA) State(ctx context.Context, entityID string) (string, error) {
	return "playing", nil
}

// TestDispatcher_ProceedsAfterEstimatedSpeechDurationWhenEntityNeverReportsIdleAgain
// guards against a real production hang: direct observation of a live
// Yandex Station showed its media_player entity get stuck reporting
// "playing" indefinitely after a media_content_type: text announcement,
// never returning to "idle" at all. Before waitIdle's second phase had a
// fallback timeout, this hung Speak (and the whole HTTP request) forever —
// only the first chunk ever got spoken.
func TestDispatcher_ProceedsAfterEstimatedSpeechDurationWhenEntityNeverReportsIdleAgain(t *testing.T) {
	ha := &stuckPlayingHA{}
	cfg := config.TTSConfig{
		Primary: "yandex_station",
		YandexStation: config.YandexStationConfig{
			Entities:               []string{"media_player.kitchen"},
			ChunkMaxChars:          100,
			IdlePollIntervalMS:     1,
			PlaybackStartTimeoutMS: 1,
			SpeechCharsPerSecond:   1000, // keep the test fast
			SpeechMarginMS:         5,
		},
	}
	d := NewDispatcher(cfg, ha, nil)

	err := d.Speak(context.Background(), "Первое предложение. Второе предложение.")
	require.NoError(t, err)

	// Both chunks must have gone out, in order — the dispatcher must not
	// have hung waiting for "idle" after the first one.
	require.Equal(t, []string{"Первое предложение.", "Второе предложение."}, ha.calls)
}

// TestDispatcher_LogsStatusHeartbeatWhileWaiting proves waitIdle's
// once-a-second status heartbeat actually logs — a plain, fixed-cadence
// timeline of what the entity reports, independent of the functional
// (change-triggered) polling logged elsewhere, requested specifically to
// make a real device's behavior unambiguous when a wait doesn't resolve the
// way we expect.
func TestDispatcher_LogsStatusHeartbeatWhileWaiting(t *testing.T) {
	ha := &stuckPlayingHA{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := config.TTSConfig{
		Primary: "yandex_station",
		YandexStation: config.YandexStationConfig{
			Entities:               []string{"media_player.kitchen"},
			ChunkMaxChars:          100,
			IdlePollIntervalMS:     1,
			PlaybackStartTimeoutMS: 1,
			SpeechCharsPerSecond:   100000, // keep the estimated-speech-duration wait short
			SpeechMarginMS:         50,
		},
	}
	d := NewDispatcher(cfg, ha, logger)
	d.statusLogInterval = 5 * time.Millisecond // real 1s cadence would make this test slow

	err := d.Speak(context.Background(), "привет")
	require.NoError(t, err)

	require.Contains(t, buf.String(), "tts: status check")
	require.Contains(t, buf.String(), "state=playing")
}

// gatedHA lets a test hold the first CallService call open (simulating a
// slow/in-flight turn) while a second Speak call is started concurrently,
// to prove the two don't interleave their play_media calls on the shared
// entity.
type gatedHA struct {
	mu      sync.Mutex
	calls   []string // media_content_id in the order CallService actually ran
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (f *gatedHA) CallService(ctx context.Context, domain, service string, data map[string]any) error {
	f.once.Do(func() {
		close(f.entered)
		<-f.gate
	})
	f.mu.Lock()
	f.calls = append(f.calls, data["media_content_id"].(string))
	f.mu.Unlock()
	return nil
}

func (f *gatedHA) State(ctx context.Context, entityID string) (string, error) { return "idle", nil }

// TestDispatcher_SerializesConcurrentSpeakCalls guards against the failure
// mode two overlapping turns hit in production: nothing prevented two Speak
// calls (e.g. an in-flight turn and a retry of the same request) from
// interleaving their play_media/wait-idle sequences on the same shared
// Yandex Station entity, which made each dispatcher's waitIdle observe state
// transitions caused by the *other* call — hanging one or both indefinitely.
func TestDispatcher_SerializesConcurrentSpeakCalls(t *testing.T) {
	ha := &gatedHA{gate: make(chan struct{}), entered: make(chan struct{})}
	d := NewDispatcher(yandexConfig("media_player.kitchen"), ha, nil)

	firstDone := make(chan struct{})
	go func() {
		require.NoError(t, d.Speak(context.Background(), "первый"))
		close(firstDone)
	}()
	<-ha.entered // first call now holds the shared-speaker semaphore

	secondDone := make(chan struct{})
	go func() {
		require.NoError(t, d.Speak(context.Background(), "второй"))
		close(secondDone)
	}()

	select {
	case <-secondDone:
		t.Fatal("second Speak call returned before the first released the shared speaker")
	case <-time.After(50 * time.Millisecond):
	}

	close(ha.gate)
	<-firstDone
	<-secondDone

	require.Equal(t, []string{"первый", "второй"}, ha.calls)
}

func TestDispatcher_ErrorsWhenNoChannelConfigured(t *testing.T) {
	cfg := config.TTSConfig{Primary: "yandex_station"} // no entities configured
	d := NewDispatcher(cfg, &fakeHA{}, nil)

	err := d.Speak(context.Background(), "привет")
	require.Error(t, err)
}
