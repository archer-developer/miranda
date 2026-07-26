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
	// SpeechCharsPerSecond estimates the station's spoken reading speed, and
	// SpeechMarginMS is a fixed cushion added on top (synthesis/network
	// latency before audio actually starts). Together they bound how long
	// waitIdle will wait for a chunk to finish playing: some real Yandex
	// Station setups never report the entity back to "idle" after a
	// media_content_type: text announcement (confirmed by direct
	// observation — the entity got stuck reporting "playing" indefinitely),
	// so polling for that indefinitely can hang forever. If the entity does
	// report idle before this estimate elapses, waitIdle still returns as
	// soon as that happens — this is only the fallback ceiling.
	SpeechCharsPerSecond float64 `yaml:"speech_chars_per_second"`
	SpeechMarginMS       int     `yaml:"speech_margin_ms"`
}

// TTSConfig selects and configures the TTS channel.
type TTSConfig struct {
	Primary       string              `yaml:"primary"`
	YandexStation YandexStationConfig `yaml:"yandex_station"`
	// SpeakReplyTool controls whether the speak_reply tool (see
	// internal/httpapi) is offered to the model — it lets the model dispatch
	// this turn's reply to the primary TTS channel even on a source other
	// than ha_assist, when the user explicitly asks to hear the answer.
	SpeakReplyTool bool `yaml:"speak_reply_tool"`
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
			Primary: "yandex_station",
			YandexStation: YandexStationConfig{
				Entities:               nil,
				ChunkMaxChars:          100,
				IdlePollIntervalMS:     300,
				PlaybackStartTimeoutMS: 3000,
				SpeechCharsPerSecond:   12, // conservative Russian TTS reading speed — err on the side of a longer wait, not a cut-off chunk
				SpeechMarginMS:         2000,
			},
			SpeakReplyTool: true,
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
