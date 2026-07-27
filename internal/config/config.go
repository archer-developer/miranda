// Package config loads Miranda's YAML configuration file and merges it over
// built-in defaults, so the agent runs with sane behavior even with an empty
// or partial config.yaml.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the root of the agent's configuration tree. Every field must have
// a value set in Default() so a missing or partial config.yaml still yields a
// fully runnable configuration.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Storage  StorageConfig  `yaml:"storage"`
	Logging  LoggingConfig  `yaml:"logging"`
	LLM      LLMConfig      `yaml:"llm"`
	Agent    AgentConfig    `yaml:"agent"`
	Memory   MemoryConfig   `yaml:"memory"`
	MCP      MCPConfig      `yaml:"mcp"`
	TTS      TTSConfig      `yaml:"tts"`
	WebUI    WebUIConfig    `yaml:"web_ui"`
	WebAuthn WebAuthnConfig `yaml:"webauthn"`
	Telegram TelegramConfig `yaml:"telegram"`
	Users    []UserConfig   `yaml:"users"`
}

// UserConfig is one login account for the web UI, and doubles as the
// canonical identity for history/memory: the same Username is used as the
// user_id key regardless of whether a turn arrives via an authenticated web
// UI session or via Home Assistant (in which case HAUserID, if set, is used
// to map HA's speaker-recognition user id back to this Username — see
// internal/users). Only Username and PasswordHash are required; the rest
// are optional profile fields.
type UserConfig struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"` // bcrypt, see internal/users.HashPassword
	FullName     string `yaml:"full_name,omitempty"`
	// Avatar is either an http(s) URL, or a bare filename that must exist in
	// StorageConfig.AvatarsDir (served at /static/avatars/<filename>).
	Avatar   string `yaml:"avatar,omitempty"`
	HAUserID string `yaml:"ha_user_id,omitempty"`
	// TelegramName is this user's Telegram @username (with or without the
	// leading "@" — normalized on load), used to map incoming webhook
	// messages to a Username the same way HAUserID maps HA speaker-
	// recognition ids. Only relevant when TelegramConfig.Enabled is true.
	TelegramName string `yaml:"telegram_name,omitempty"`
	// Language is the web UI's default locale for this user after login
	// ("ru", "be", or "en"); the header switcher can still override it per
	// session. Defaults to the server-wide web_ui.default_language.
	Language string `yaml:"language,omitempty"`
}

// ServerConfig controls the unified command interface / web UI HTTP server.
type ServerConfig struct {
	HTTPAddr  string `yaml:"http_addr"`
	AuthToken string `yaml:"auth_token"`
}

// StorageConfig points at the on-disk SQLite history DB, markdown memory
// dir, and local user avatar images.
type StorageConfig struct {
	SQLitePath string `yaml:"sqlite_path"`
	MemoryDir  string `yaml:"memory_dir"`
	// AvatarsDir is served at /static/avatars/ — see UserConfig.Avatar.
	AvatarsDir string `yaml:"avatars_dir"`
	// WebAuthnSQLitePath is a separate small SQLite file for passkey
	// credentials (internal/webauthn) — kept apart from SQLitePath so
	// changing/wiping the dialog history never touches registered passkeys
	// (and vice versa). Only used when WebAuthnConfig.Enabled is true.
	WebAuthnSQLitePath string `yaml:"webauthn_sqlite_path"`
	// TelegramChatsPath is a small JSON file mapping each Miranda username to
	// the Telegram chat id learned the first time they message the bot (see
	// internal/telegram.ChatStore) — the Bot API gives no way to look up or
	// message a user who has never started a chat with the bot, so this has
	// to be learned and persisted rather than derived from config. Only used
	// when TelegramConfig.Enabled is true.
	TelegramChatsPath string `yaml:"telegram_chats_path"`
	// TTSCacheDir is where the gemini_tts provider's content-addressed,
	// permanent (no TTL/expiry — see internal/tts/cache.go) rendered-audio
	// cache lives; it's also the directory tts.HTTPHandler serves
	// GET /tts-audio/{key}.{ext} out of. A cache hit skips calling Gemini's
	// API entirely, which is the main reason this exists — quota, not just
	// latency. Only relevant when gemini_tts is the configured primary or
	// fallback TTS provider.
	TTSCacheDir string `yaml:"tts_cache_dir"`
}

// WebAuthnConfig controls optional FIDO2/passkey ("biometric") login,
// implemented by internal/webauthn. Opt-in (Enabled defaults false)
// because RPID/RPOrigins are inherently deployment-specific — there's no
// safe auto-detected default, and getting them wrong doesn't just fail
// gracefully: a stale RPID orphans every previously-registered passkey.
//
// WebAuthn is a browser API that only works in a secure context (HTTPS, or
// http://localhost) — it does not work over plain HTTP on a LAN IP, even
// with this enabled. Miranda does not terminate TLS itself; front it with a
// reverse proxy (or similar) that does, and point RPID/RPOrigins at that
// proxy's hostname, not Miranda's own http_addr.
type WebAuthnConfig struct {
	Enabled bool `yaml:"enabled"`
	// RPID is the bare hostname passkeys are bound to — no scheme, no port
	// (e.g. "miranda.example.com"). Changing this later orphans every
	// credential registered under the old value.
	RPID string `yaml:"rp_id"`
	// RPDisplayName is shown to the user by their browser/OS during the
	// registration and login ceremonies (e.g. "Miranda").
	RPDisplayName string `yaml:"rp_display_name"`
	// RPOrigins lists every full origin (scheme+host+port) browsers may
	// legitimately reach Miranda through, e.g. ["https://miranda.example.com"].
	// Must be non-empty for WebAuthn to work at all if Enabled.
	RPOrigins []string `yaml:"rp_origins"`
}

// TelegramConfig controls the optional Telegram bot channel (see
// internal/telegram and internal/httpapi's webhook handler). Incoming
// messages arrive via a webhook Miranda registers with Telegram at startup,
// get mapped to a configured user via UserConfig.TelegramName, and are
// forwarded to the same agent loop every other channel uses; replies go
// back through the Bot API (Telegram never reads the webhook's HTTP
// response body), not the HTTP response. Opt-in (Enabled defaults false)
// for the same reason as WebAuthnConfig: PublicBaseURL is deployment-
// specific and there's no safe default to guess.
//
// Telegram requires an HTTPS webhook URL — Miranda does not terminate TLS
// itself, so front it with a reverse proxy (the same one WebAuthnConfig
// needs) and point PublicBaseURL at that proxy's hostname.
//
// The bot token (from @BotFather) is read from the TELEGRAM_BOT_TOKEN
// environment variable, never config.yaml — same convention as
// HA_MCP_TOKEN/HA_TOKEN. The webhook's authentication secret is generated
// fresh in memory on every startup (see internal/telegram.RandomSecret) and
// re-registered with Telegram each time, so there's nothing extra to
// configure or rotate by hand.
type TelegramConfig struct {
	Enabled bool `yaml:"enabled"`
	// WebhookPath is the path Telegram POSTs updates to, e.g.
	// "/telegram/webhook". Must match whatever your reverse proxy forwards
	// to Miranda's http_addr.
	WebhookPath string `yaml:"webhook_path"`
	// PublicBaseURL is the public HTTPS origin Telegram can reach Miranda
	// through (e.g. "https://miranda.example.com") — combined with
	// WebhookPath and registered with Telegram via setWebhook once at
	// startup. Must be non-empty (and HTTPS) for Enabled to work.
	PublicBaseURL string `yaml:"public_base_url"`
	// SendMessageTool controls whether the send_telegram tool (see
	// internal/httpapi) is offered to the model, letting it proactively push
	// a message to a household member's Telegram — e.g. "отправь мне на
	// телефон ...", "отправь Ане на телефон ...". Only works for a user who
	// has messaged the bot at least once (see StorageConfig.TelegramChatsPath).
	SendMessageTool bool `yaml:"send_message_tool"`
}

// LoggingConfig controls file logging: the general application log (a
// mirror of everything printed to the terminal) and the separate LLM
// request/response trace log (internal/llmtrace) used to debug why a given
// prompt did or didn't produce the expected tool calls/reply. Both rotate by
// size so they never grow unbounded.
type LoggingConfig struct {
	// Dir is where log files are written (miranda.log, llm.log). Created
	// automatically if missing.
	Dir string `yaml:"dir"`
	// MaxSizeMB is the size in megabytes a log file reaches before it's
	// rotated to a numbered backup.
	MaxSizeMB int `yaml:"max_size_mb"`
	// MaxBackups is how many rotated backups to keep before the oldest is
	// deleted. 0 means keep all of them (bounded only by MaxAgeDays, if set).
	MaxBackups int `yaml:"max_backups"`
	// MaxAgeDays is how long to keep a rotated backup regardless of
	// MaxBackups. 0 means no age-based cleanup.
	MaxAgeDays int `yaml:"max_age_days"`
}

// LLMProvider describes one configured model backend, either an
// OpenAI-compatible endpoint (local or hosted) or native Anthropic.
type LLMProvider struct {
	Name      string `yaml:"name"`
	Type      string `yaml:"type"` // "openai_compat" | "anthropic"
	BaseURL   string `yaml:"base_url,omitempty"`
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
	// AnthropicTools enables Claude's own server-executed tools. Only
	// meaningful when Type == "anthropic"; ignored otherwise. These run
	// entirely on Anthropic's side (web fetch/search over the live internet,
	// code execution in Anthropic's sandbox) rather than through the
	// Orchestrator's own tool loop, so they're what lets Claude answer
	// something like "what's the bitcoin price right now" — none of
	// Miranda's own tools (MCP/HA, remember_this, etc.) reach the open web.
	AnthropicTools AnthropicToolsConfig `yaml:"anthropic_tools,omitempty"`
}

// AnthropicToolsConfig toggles which of Claude's native server-side tools
// (see internal/llm/anthropic) are sent on every request from this
// provider. All default to false (opt-in) since they let the model reach
// the open internet or run arbitrary code — leave disabled for providers
// that shouldn't have that reach.
type AnthropicToolsConfig struct {
	// WebSearch lets the model search the live web.
	WebSearch bool `yaml:"web_search"`
	// WebFetch lets the model retrieve a specific URL's content directly.
	WebFetch bool `yaml:"web_fetch"`
	// CodeExecution runs Python/bash in Anthropic's sandbox. When WebSearch
	// or WebFetch are also enabled, the sandbox is allowed to call them as
	// helpers (e.g. fetch a page, then parse/compute over it in code) — see
	// the AllowedCallers wiring in internal/llm/anthropic.
	CodeExecution bool `yaml:"code_execution"`
}

// EscalationConfig configures the explicit escalate_to_claude-style tool that
// lets a cheap/local model hand off a hard turn to a stronger provider.
type EscalationConfig struct {
	Enabled        bool   `yaml:"enabled"`
	ToolName       string `yaml:"tool_name"`
	TargetProvider string `yaml:"target_provider"`
}

// LLMConfig is the ordered provider fallback chain plus escalation settings.
type LLMConfig struct {
	// Providers is tried in order on error/timeout of the previous one.
	Providers       []LLMProvider    `yaml:"providers"`
	DefaultProvider string           `yaml:"default_provider"`
	Escalation      EscalationConfig `yaml:"escalation"`
}

// MemoryConfig controls how per-user markdown memory gets updated and how
// conversation sessions begin and end.
type MemoryConfig struct {
	AutoSummarize bool `yaml:"auto_summarize"`
	ExplicitTool  bool `yaml:"explicit_tool"`
	// SessionIdleTimeoutMinutes is how long a conversation must sit with no
	// new messages before the background sweeper (cmd/miranda) treats it as
	// over, distills it into memory, and marks it ended. Only consulted when
	// AutoSummarize is true. The server — not any conversation_id a caller
	// echoes back — is what decides session continuity (see
	// history.Store.OpenConversation), so this timeout is what actually
	// governs when a session boundary happens.
	SessionIdleTimeoutMinutes int `yaml:"session_idle_timeout_minutes"`
	// SearchHistoryTool exposes a search_history tool the model can call when
	// the user references an earlier conversation ("помнишь мы говорили о
	// ...") — a full-text lookup over past (ended) conversations in
	// internal/history, returning each match's stored summary rather than
	// raw messages, distinct from the distilled facts in the per-user memory
	// file.
	SearchHistoryTool bool `yaml:"search_history_tool"`
	// EndConversationTool exposes an end_conversation tool the model can call
	// when the user explicitly asks to start a new conversation ("давай
	// начнём новую беседу") — ends the current session immediately instead
	// of waiting for the idle timeout.
	EndConversationTool bool `yaml:"end_conversation_tool"`
	// ForgetConversationTool exposes a forget_conversation tool the model can
	// call when the user asks to erase the current conversation entirely
	// ("забудь этот диалог", "давай с начала") — deletes it from history with
	// no summarization, unlike a normal session end.
	ForgetConversationTool bool `yaml:"forget_conversation_tool"`
}

// MCPServer is one MCP server the agent connects to as a tool source.
type MCPServer struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url"`
	TokenEnv string `yaml:"token_env,omitempty"`
	Enabled  bool   `yaml:"enabled"`
}

// MCPConfig lists the MCP servers (HA + others) available as tool sources.
type MCPConfig struct {
	Servers []MCPServer `yaml:"servers"`
}

// YandexStationConfig configures the primary TTS channel.
type YandexStationConfig struct {
	Entities           []string `yaml:"entities"`
	ChunkMaxChars      int      `yaml:"chunk_max_chars"`
	IdlePollIntervalMS int      `yaml:"idle_poll_interval_ms"`
	// PlaybackStartTimeoutMS bounds how long the dispatcher waits, after a
	// play_media call, for the entity to actually leave "idle" before giving
	// up and moving on to waiting for it to return to idle. See waitIdle in
	// internal/tts/dispatcher.go for why this two-phase wait exists.
	PlaybackStartTimeoutMS int `yaml:"playback_start_timeout_ms"`
}

// GeminiTTSConfig configures the optional gemini_tts provider — an
// alternative to Yandex Station's own built-in voice, selected via
// TTSConfig.Primary/Fallback ("gemini_tts"). See internal/tts/gemini.go for
// the request/response shape this drives. Disabled by default: it needs
// real API keys and has its own quota/cost tradeoffs a deployment has to
// opt into deliberately, unlike the always-available yandex_station_text
// provider.
type GeminiTTSConfig struct {
	Enabled bool `yaml:"enabled"`
	// APIKeyEnvs lists environment variable names, each expected to hold one
	// Gemini API key (never the keys themselves — same *_env convention as
	// every other secret in this codebase). See
	// internal/tts.geminiProvider's key-rotation doc comment for why more
	// than one is useful: free-tier Gemini keys have a low per-key quota,
	// and rotating across several multiplies the effective quota for a
	// purely personal/household deployment.
	APIKeyEnvs []string `yaml:"api_key_envs"`
	// Model is the Gemini model id to call generateContent on, e.g.
	// "gemini-2.5-flash-preview-tts".
	Model string `yaml:"model"`
	// Voice is one of Gemini's prebuilt voice names (the request's
	// generationConfig.speechConfig.voiceConfig.prebuiltVoiceConfig.voiceName).
	Voice string `yaml:"voice"`
	// AudioFormat selects the container the raw PCM Gemini returns is
	// wrapped in before being served to the Yandex Station: "wav" (a bare
	// 44-byte RIFF/WAVE header, no new dependency) or "mp3" (encoded via
	// github.com/braheezy/shine-mp3, pure Go, no cgo). Whether a real
	// station's play_media URL playback actually accepts WAV isn't verified
	// against real hardware yet — see README — so this is a one-line config
	// change to try the other format, not a code change.
	AudioFormat string `yaml:"audio_format"`
	// PublicBaseURL is the base URL the Yandex Station can reach Miranda's
	// own GET /tts-audio/ route through (e.g. "http://192.168.1.50:8787", or
	// a reverse-proxied hostname) — combined with the cached file's name to
	// build the URL handed to media_player.play_media.
	PublicBaseURL string `yaml:"public_base_url"`
	// ChunkMaxChars is this provider's sentence-boundary chunk size (see
	// tts.Accumulator/Chunk) — larger than Yandex Station's own text limit,
	// since Gemini renders a real audio file rather than being limited by
	// whatever the station's own on-device TTS can swallow in one call.
	ChunkMaxChars int `yaml:"chunk_max_chars"`
	// RequestTimeoutSeconds bounds one HTTP call to Gemini's generateContent
	// endpoint.
	RequestTimeoutSeconds int `yaml:"request_timeout_seconds"`
	// QuotaCooldownSeconds is how long to sleep before retrying the whole
	// API key list again if every key hit a quota error in one pass — most
	// free-tier Gemini quota windows are per-minute, so a short cooldown
	// often recovers a key that failed moments ago.
	QuotaCooldownSeconds int `yaml:"quota_cooldown_seconds"`
	// MaxQuotaRetryCycles bounds how many times the whole key list is
	// retried (with QuotaCooldownSeconds between passes) before giving up
	// and returning tts.ErrQuotaExceeded.
	MaxQuotaRetryCycles int `yaml:"max_quota_retry_cycles"`
}

// TTSConfig selects and configures the TTS channel(s).
type TTSConfig struct {
	// Primary is the TTS provider tried first for every Speak call:
	// "yandex_station_text" (Yandex Station's own built-in voice, played via
	// media_content_type "text" — the default) or "gemini_tts" (an external
	// voice rendered by Gemini and played back through the same station as
	// a fetched audio file — see GeminiTTSConfig).
	Primary       string              `yaml:"primary"`
	YandexStation YandexStationConfig `yaml:"yandex_station"`
	// Fallback is the provider retried, with the same text, if Primary's
	// Speak returns tts.ErrQuotaExceeded — e.g. gemini_tts exhausting every
	// configured API key falls back to yandex_station_text so the user
	// still hears *something* instead of silence. Equal to Primary (the
	// default) or empty both disable the fallback path.
	Fallback string `yaml:"fallback"`
	// GeminiTTS configures the optional gemini_tts provider — only consulted
	// when Primary or Fallback names it.
	GeminiTTS GeminiTTSConfig `yaml:"gemini_tts"`
	// SpeakReplyTool controls whether the speak_reply tool (see
	// internal/httpapi) is offered to the model — it lets the model dispatch
	// this turn's reply to the primary TTS channel even on a source other
	// than ha_assist, when the user explicitly asks to hear the answer.
	SpeakReplyTool bool `yaml:"speak_reply_tool"`
	// StopSpeechTool controls whether the stop_speech tool (see
	// internal/httpapi) is offered to the model — it lets the model
	// interrupt whatever tts is currently speaking or has queued, e.g. when
	// the user explicitly asks Miranda to stop talking mid-reply.
	StopSpeechTool bool `yaml:"stop_speech_tool"`
}

// WebUIConfig controls the embedded monitoring dashboard.
type WebUIConfig struct {
	Enabled bool `yaml:"enabled"`
	// LogBufferSize is how many recent hub.Event entries are replayed to a
	// newly connected /ws/logs subscriber. Shared by chat/tool events and
	// the log-viewer screen's mirrored app/LLM-trace log lines (see
	// hub.Hub.Writer), so it needs more headroom than a chat-only buffer.
	LogBufferSize int `yaml:"log_buffer_size"`
	// DefaultLanguage is used for the login page (before we know which user
	// is signing in) and as the fallback for any user without their own
	// UserConfig.Language. One of "ru", "be", "en".
	DefaultLanguage string `yaml:"default_language"`
}

// AgentConfig controls high-level agent behaviour that is separate from
// the model-provider choice (LLMConfig) or memory mechanics (MemoryConfig).
type AgentConfig struct {
	// SystemPrompt is injected as the system message at the start of every
	// LLM conversation. Override this in config.yaml to change the assistant
	// persona, add standing instructions, or restrict topic scope.
	SystemPrompt string `yaml:"system_prompt"`
}

// Default returns the built-in configuration used when config.yaml is absent
// or only overrides a subset of fields.
func Default() Config {
	return Config{
		Server: ServerConfig{
			HTTPAddr:  ":8787",
			AuthToken: "",
		},
		Storage: StorageConfig{
			SQLitePath:         "./data/miranda.db",
			MemoryDir:          "./data/memory",
			AvatarsDir:         "./data/avatars",
			WebAuthnSQLitePath: "./data/webauthn.db",
			TelegramChatsPath:  "./data/telegram_chats.json",
			TTSCacheDir:        "./data/storage",
		},
		Logging: LoggingConfig{
			Dir:        "./logs",
			MaxSizeMB:  10,
			MaxBackups: 5,
			MaxAgeDays: 30,
		},
		LLM: LLMConfig{
			Providers:       nil,
			DefaultProvider: "",
			Escalation: EscalationConfig{
				Enabled:        true,
				ToolName:       "escalate_to_claude",
				TargetProvider: "claude",
			},
		},
		Memory: MemoryConfig{
			AutoSummarize:             true,
			ExplicitTool:              true,
			SessionIdleTimeoutMinutes: 25,
			SearchHistoryTool:         true,
			EndConversationTool:       true,
			ForgetConversationTool:    true,
		},
		MCP: MCPConfig{
			Servers: nil,
		},
		TTS: TTSConfig{
			// Opt-in gemini_tts is a new channel, not a default swap — this
			// stays the always-available, zero-external-dependency provider.
			Primary: "yandex_station_text",
			YandexStation: YandexStationConfig{
				Entities:               nil,
				ChunkMaxChars:          100,
				IdlePollIntervalMS:     300,
				PlaybackStartTimeoutMS: 3000,
			},
			// Equal to Primary by default, i.e. no active fallback until a
			// deployment actually configures gemini_tts as Primary.
			Fallback: "yandex_station_text",
			GeminiTTS: GeminiTTSConfig{
				Enabled: false,
				// A bare 44-byte RIFF/WAVE header needs zero new
				// dependencies; try "mp3" (via shine-mp3) if the real
				// Yandex Station rejects WAV over play_media's URL
				// playback.
				AudioFormat:           "wav",
				ChunkMaxChars:         200,
				RequestTimeoutSeconds: 30,
				QuotaCooldownSeconds:  5,
				MaxQuotaRetryCycles:   3,
			},
			SpeakReplyTool: true,
			StopSpeechTool: true,
		},
		Agent: AgentConfig{
			SystemPrompt: `Тебя зовут Miranda. Ты домашний голосовой ассистент.
Твоя задача — помогать Саше и Ане управлять умным домом и отвечать на любые вопросы, которые ты можешь решить.

## Стиль общения
- По умолчанию отвечай кратко, естественно и по существу.
- Большинство ответов будет озвучено голосом, поэтому не используй длинные вступления и лишние пояснения.
- Если пользователь явно просит рассказать подробнее, дай развернутый ответ.

## Ограничения длины
- Обычный ответ — не более 100 символов.
- Если пользователь явно просит рассказать подробно — не более 1000 символов.

## Работа с умным домом
Используй GetLiveContext в следующих случаях:

- перед управлением устройствами;
- если необходимо узнать текущее состояние устройства;
- если существует хотя бы малейшая неоднозначность в названии устройства, комнаты или сцены;
- если пользователь использует неполное или разговорное название.

Не пытайся угадывать названия устройств, комнат или сцен.
Не проси пользователя уточнить название, пока сначала не проверишь доступные устройства и сцены через GetLiveContext.
Не вызывай GetLiveContext для обычных вопросов, не связанных с умным домом.

## Источник истины
Никогда не используй память разговора как источник информации о текущем состоянии дома.
Единственным источником истины о состоянии устройств, сцен, яркости, громкости, температуре и других параметрах является GetLiveContext.
Не утверждай, что устройство включено, выключено, имеет определенную яркость, громкость или другое состояние, пока не получишь актуальные данные через GetLiveContext.
Если пользователь сообщает, что названное тобой состояние неверно:

- считай информацию пользователя более достоверной;
- повторно вызови GetLiveContext;
- не повторяй прежний ответ без проверки.

## Выполнение действий
После управления устройствами сообщай только тот результат, который подтвержден инструментом.
Не говори "Готово", "Включила" или "Выключила", если инструмент сообщил об ошибке или не подтвердил успешное выполнение.
После успешного выполнения действия отвечай максимально кратко.

## Общие правила
Если для ответа необходимы актуальные данные о доме — сначала используй GetLiveContext.
Если вопрос не связан с управлением домом или его состоянием, отвечай без вызова инструментов.
Если информации недостаточно даже после использования GetLiveContext, задай пользователю уточняющий вопрос вместо того, чтобы делать предположения.`,
		},
		WebUI: WebUIConfig{
			Enabled:         true,
			LogBufferSize:   2000,
			DefaultLanguage: "ru",
		},
		// Disabled by default — see WebAuthnConfig's doc comment for why
		// there's no safe auto-detected RPID/RPOrigins.
		WebAuthn: WebAuthnConfig{
			Enabled: false,
		},
		// Disabled by default — see TelegramConfig's doc comment for why
		// there's no safe auto-detected PublicBaseURL.
		Telegram: TelegramConfig{
			Enabled:         false,
			WebhookPath:     "/telegram/webhook",
			SendMessageTool: true,
		},
		// No default Users: web UI login is mandatory and fails closed until
		// config.yaml lists at least one account (see internal/users).
		Users: nil,
	}
}

// Load reads the YAML file at path and merges it over Default(). A missing
// file is not an error: it just means the defaults are used as-is, so the
// agent has a working config out of the box.
//
// yaml.Unmarshal only overwrites fields present in the document, so starting
// from a fully-populated default struct gives us a cheap partial merge
// without a separate deep-merge step.
func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", path, err)
	}

	return cfg, nil
}
