package tts

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
)

func TestNewProvider_DefaultsAndExplicitYandexStationText(t *testing.T) {
	cfg := config.TTSConfig{YandexStation: config.YandexStationConfig{Entities: []string{"media_player.kitchen"}}}

	for _, name := range []string{"", "yandex_station_text"} {
		p, err := NewProvider(name, cfg, t.TempDir(), &fakeHA{}, nil)
		require.NoError(t, err)
		_, ok := p.(*textProvider)
		require.True(t, ok, "NewProvider(%q, ...) must return a *textProvider", name)
	}
}

func TestNewProvider_GeminiTTSRequiresEnabledFlag(t *testing.T) {
	cfg := config.TTSConfig{GeminiTTS: config.GeminiTTSConfig{Enabled: false}}

	_, err := NewProvider("gemini_tts", cfg, t.TempDir(), &fakeHA{}, nil)
	require.Error(t, err, "requesting gemini_tts while tts.gemini_tts.enabled is false must fail clearly")
}

func TestNewProvider_GeminiTTSBuildsWhenEnabledAndKeyed(t *testing.T) {
	t.Setenv("MIRANDA_TEST_FACTORY_GEMINI_KEY", "a-key")
	cfg := config.TTSConfig{
		GeminiTTS: config.GeminiTTSConfig{
			Enabled:    true,
			APIKeyEnvs: []string{"MIRANDA_TEST_FACTORY_GEMINI_KEY"},
		},
	}

	p, err := NewProvider("gemini_tts", cfg, t.TempDir(), &fakeHA{}, nil)
	require.NoError(t, err)
	_, ok := p.(*geminiProvider)
	require.True(t, ok)
}

func TestNewProvider_UnknownNameReturnsError(t *testing.T) {
	_, err := NewProvider("something_else", config.TTSConfig{}, t.TempDir(), &fakeHA{}, nil)
	require.Error(t, err)
}
