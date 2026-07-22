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

func TestDispatcher_FallsBackToHATTSWhenYandexFails(t *testing.T) {
	ha := &fakeHA{failDomain: "media_player"}
	cfg := yandexConfig("media_player.kitchen")
	cfg.HATTS = config.HATTSConfig{Enabled: true, EntityID: "tts.piper", TargetPlayer: "media_player.satellite1"}
	d := NewDispatcher(cfg, ha)

	err := d.Speak(context.Background(), "привет")
	require.NoError(t, err)

	require.Len(t, ha.calls, 1)
	require.Equal(t, "tts", ha.calls[0].domain)
	require.Equal(t, "speak", ha.calls[0].service)
	require.Equal(t, "привет", ha.calls[0].data["message"])
}

func TestDispatcher_ReturnsErrorWhenYandexFailsAndNoFallback(t *testing.T) {
	ha := &fakeHA{failDomain: "media_player"}
	d := NewDispatcher(yandexConfig("media_player.kitchen"), ha)

	err := d.Speak(context.Background(), "привет")
	require.Error(t, err)
}

func TestDispatcher_ErrorsWhenNoChannelConfigured(t *testing.T) {
	cfg := config.TTSConfig{Primary: "yandex_station"} // no entities, no fallback
	d := NewDispatcher(cfg, &fakeHA{})

	err := d.Speak(context.Background(), "привет")
	require.Error(t, err)
}
