package tts

import (
	"fmt"
	"log/slog"

	"github.com/archer-developer/miranda/internal/config"
)

// NewProvider builds the Provider named name — "yandex_station_text" (the
// default) or "gemini_tts" — from cfg, the same config.TTSConfig a
// Dispatcher is built from. cacheDir is config.StorageConfig.TTSCacheDir,
// only actually used by gemini_tts. Called once for Primary and, if
// distinct, once more for Fallback (see cmd/miranda.buildTTSDispatcher).
func NewProvider(name string, cfg config.TTSConfig, cacheDir string, ha HAClient, logger *slog.Logger) (Provider, error) {
	switch name {
	case "", "yandex_station_text":
		return NewTextProvider(cfg.YandexStation, ha, logger), nil
	case "gemini_tts":
		if !cfg.GeminiTTS.Enabled {
			return nil, fmt.Errorf("tts: gemini_tts requested as a provider but tts.gemini_tts.enabled is false")
		}
		return NewGeminiProvider(cfg.GeminiTTS, cfg.YandexStation, cacheDir, ha, logger)
	default:
		return nil, fmt.Errorf("tts: unknown provider %q", name)
	}
}
