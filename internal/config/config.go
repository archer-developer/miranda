// Package config loads Miranda's YAML configuration file and merges it over
// built-in defaults, so the agent runs with sane behavior even with an empty
// or partial config.yaml.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the root of the agent's configuration tree. Every field must have
// a value set in Default() so a missing or partial config.yaml still yields a
// fully runnable configuration.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Storage    StorageConfig    `yaml:"storage"`
	Logging    LoggingConfig    `yaml:"logging"`
	LLM        LLMConfig        `yaml:"llm"`
	Agent      AgentConfig      `yaml:"agent"`
	Memory     MemoryConfig     `yaml:"memory"`
	MCP        MCPConfig        `yaml:"mcp"`
	Tavily     TavilyConfig     `yaml:"tavily"`
	TTS        TTSConfig        `yaml:"tts"`
	WebUI      WebUIConfig      `yaml:"web_ui"`
	WebAuthn   WebAuthnConfig   `yaml:"webauthn"`
	Telegram   TelegramConfig   `yaml:"telegram"`
	Schedule   ScheduleConfig   `yaml:"schedule"`
	FileUpload FileUploadConfig `yaml:"file_upload"`
	Users      []UserConfig     `yaml:"users"`
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
	// Timezone is the IANA timezone name for this user (e.g. "Europe/Moscow",
	// "America/New_York"). Used to display scheduled task times to the model
	// and to interpret cron expressions relative to the user's local time.
	// Defaults to the server's local timezone when empty or invalid.
	Timezone string `yaml:"timezone,omitempty"`
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
	// ScheduleSQLitePath is a separate SQLite file for scheduled tasks (see
	// internal/schedule) — kept apart from SQLitePath for the same reason
	// WebAuthnSQLitePath is: wiping/backing up dialog history shouldn't touch
	// a household's pending reminders (and vice versa). Only used when
	// ScheduleConfig.Enabled is true.
	ScheduleSQLitePath string `yaml:"schedule_sqlite_path"`
	// KeyringSQLitePath is a separate SQLite file holding every user's
	// wrapped master-key slots (see internal/keyring). Unlike the other
	// optional SQLite files above, losing this one is data-loss-equivalent
	// to losing every piece of data encrypted under it — back it up at
	// least as carefully as SQLitePath, not as an afterthought like
	// ScheduleSQLitePath/WebAuthnSQLitePath. The keyring itself is always
	// on (no config toggle — see internal/keyring), so this path is always
	// in use.
	KeyringSQLitePath string `yaml:"keyring_sqlite_path"`
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
	// RegisterWebhook controls whether setupTelegram (cmd/miranda) actually
	// calls Telegram's setWebhook API at startup. Defaults to true — the
	// normal case where this is the one process that should own the bot's
	// webhook registration. Set to false for a second, non-production
	// instance that shares the same TELEGRAM_BOT_TOKEN/PublicBaseURL as a
	// real deployment (e.g. a developer running `go run ./cmd/miranda`
	// locally against otherwise-real config — see MIRANDA_CONFIG_DIR in
	// README.md — while debugging something unrelated to Telegram): with
	// Enabled true and this false, the outbound send_telegram tool and
	// ChatStore still work, but startup skips setWebhook, so it never
	// clobbers the real deployment's webhook secret and breaks its inbound
	// delivery until that one restarts.
	RegisterWebhook bool `yaml:"register_webhook"`
}

// ScheduleConfig controls the create_scheduled_task/list_scheduled_tasks/
// delete_scheduled_task tools (see internal/schedule, internal/httpapi) that
// let the model schedule a free-text prompt to be replayed through the
// agent loop later — a one-off reminder or a recurring routine. Unlike
// Telegram/WebAuthn, this needs no deployment-specific secret or URL to
// turn on, so Enabled defaults to true (opt-out, not opt-in).
type ScheduleConfig struct {
	Enabled bool `yaml:"enabled"`
}

// FileUploadConfig controls the optional POST /api/upload, GET /files/{id},
// and GET /api/files/{file_id} endpoints and client-side file
// attachment/download support in the web UI. Upload and download are two
// independent paths sharing this one config block for historical reasons,
// not because they still share a mechanism: an upload is staged locally
// (internal/attachments.Store) and served back out over GET /files/{id}
// under PublicBaseURL (see docs/file-staging-refactor.md); a download still
// proxies to the sandbox's own GET /files/{id} via SandboxMCPServerName,
// unchanged. Opt-in (Enabled defaults false) since there's no safe
// auto-detected default for either.
type FileUploadConfig struct {
	// Enabled activates POST /api/upload, GET /files/{id}, GET
	// /api/files/{file_id}, and attachment processing in the agent loop.
	// Requires PublicBaseURL to be set (checked at startup); SandboxMCPServerName
	// is only required if downloads are to work.
	Enabled bool `yaml:"enabled"`
	// SandboxMCPServerName is the name of an MCP server entry in
	// MCPConfig.Servers whose URL hosts the sandbox's file transfer API —
	// used only for the download direction (GET /api/files/{file_id}
	// proxying a download_file result back to the browser). The files
	// endpoint URL is derived by replacing the "/mcp" suffix of that
	// server's URL with "/files" — e.g. "http://192.168.1.50:8788/mcp" →
	// "http://192.168.1.50:8788/files". The same server's TokenEnv
	// authenticates the download request, and this same name is what
	// SetSandboxDownload namespaces the download_file MCP tool name
	// against.
	SandboxMCPServerName string `yaml:"sandbox_mcp_server_name"`
	// MaxFileSizeBytes is the maximum size of a single uploaded file.
	// Defaults to 50 MB. Applies to the /api/upload read limit. Downloads
	// are bounded separately, by the sandbox's own max_download_size_bytes
	// — a download_file call already rejects an oversized file before
	// Miranda ever proxies a GET /api/files/{file_id} for it.
	MaxFileSizeBytes int64 `yaml:"max_file_size_bytes"`
	// DownloadRecordTTLHours is how long a download_file ownership record
	// (internal/attachments.Store, Record.TTL) stays valid before
	// GET /api/files/{file_id} starts 404ing it. Unlike an upload-staging
	// record — which only needs to outlive the turn still composing a
	// prompt — a download's <download>...</download> marker is embedded
	// durably in persisted conversation history and can be clicked again
	// whenever that conversation is reopened, so it needs a much longer
	// window than the store's own short upload-oriented default TTL.
	// Defaults to 720 hours (30 days).
	DownloadRecordTTLHours int `yaml:"download_record_ttl_hours"`
	// PublicBaseURL is the base URL other backend services (the sandbox,
	// miranda-medical-card, ...) can reach Miranda's own GET /files/{id}
	// route through — e.g. "http://192.168.1.50:8787". This is what a
	// tool's own fileUri argument (see docs/file-staging-refactor.md) is
	// built from: Miranda stages an uploaded attachment's bytes locally
	// (internal/attachments.Store) and hands the model a URI under this
	// base, rather than pushing bytes anywhere itself — whichever tool the
	// model calls with that URI is responsible for fetching it. Mirrors
	// TTSConfig's gemini_tts.PublicBaseURL, which solves the identical
	// problem (a LAN device fetching a Miranda-hosted resource by URL) for
	// synthesized audio — required (checked at startup) whenever Enabled is
	// true, the same way gemini_tts checks it at the point of use.
	PublicBaseURL string `yaml:"public_base_url"`
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
	// Level is the minimum log level to emit: "debug", "info", "warn", or
	// "error". Defaults to "info". Set to "debug" to see tool-call timings,
	// web search queries, and other verbose diagnostic output.
	Level string `yaml:"level"`
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

// LLMProvider describes one configured model backend: OpenAI-compatible
// (local or hosted), native Anthropic, or native Gemini.
type LLMProvider struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"` // "openai_compat" | "anthropic" | "gemini"
	BaseURL string `yaml:"base_url,omitempty"`
	Model   string `yaml:"model"`

	// APIKeyEnvs lists environment variable names, each expected to hold
	// one API key (never the key itself — same *_env convention as every
	// other secret in this codebase). All three provider types share this
	// one field for schema consistency, but only "gemini" actually rotates
	// across more than one entry (internal/llm/gemini cycles through every
	// resolved key on a quota/server error — see GeminiRotationConfig).
	// "anthropic" and "openai_compat" use only APIKeyEnvs[0]: those SDKs
	// take a single credential per client and aren't being changed to
	// rotate — extra entries beyond the first are silently ignored for
	// those two types (see cmd/miranda/main.go's firstAPIKey).
	APIKeyEnvs []string `yaml:"api_key_envs,omitempty"`

	// AnthropicTools enables Claude's own server-executed tools. Only
	// meaningful when Type == "anthropic"; ignored otherwise. These run
	// entirely on Anthropic's side (web fetch/search over the live internet,
	// code execution in Anthropic's sandbox) rather than through the
	// Orchestrator's own tool loop, so they're what lets Claude answer
	// something like "what's the bitcoin price right now" — none of
	// Miranda's own tools (MCP/HA, remember_this, etc.) reach the open web.
	AnthropicTools AnthropicToolsConfig `yaml:"anthropic_tools,omitempty"`
	// GeminiTools enables Gemini's own server-executed tools. Only
	// meaningful when Type == "gemini"; ignored otherwise — see
	// GeminiToolsConfig.
	GeminiTools GeminiToolsConfig `yaml:"gemini_tools,omitempty"`
	// GeminiRotation tunes this provider's key-rotation behavior. Only
	// meaningful when Type == "gemini"; the zero value falls back to
	// internal/llm/gemini's built-in defaults (1 cycle, no cooldown).
	GeminiRotation GeminiRotationConfig `yaml:"gemini_rotation,omitempty"`

	// Escalation lets this specific provider hand a hard turn off to
	// another configured provider by calling a tool. Lives on each provider
	// (rather than once globally) so a chain — e.g. a cheap model escalates
	// to a stronger one, which escalates to Claude — can have each hop pick
	// its own target/tool name independently. See internal/llm/router for
	// how a chain of these gets walked hop by hop.
	Escalation EscalationConfig `yaml:"escalation,omitempty"`
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

// GeminiToolsConfig toggles Gemini's own native server-side tools sent on
// every request from this provider (see internal/llm/gemini) — mirrors
// AnthropicToolsConfig's opt-in-only shape (all default to false).
type GeminiToolsConfig struct {
	// GoogleSearch enables Grounding with Google Search — analogous to
	// AnthropicToolsConfig.WebSearch.
	//
	// There is deliberately no CodeExecution field here, unlike
	// AnthropicToolsConfig: Gemini's code-execution tool
	// (genai.ToolCodeExecution) only works on Vertex AI, not the plain
	// Gemini Developer API key this provider type uses — confirmed via the
	// SDK's own source comments, consistently applied across ~98 other
	// genuinely-Vertex-only fields. See internal/llm/gemini.nativeTools's
	// doc comment for the full reasoning, including why a working-looking
	// public example doesn't actually contradict this.
	GoogleSearch bool `yaml:"google_search"`
	// ContextCaching is not implemented yet — internal/llm/gemini.New
	// rejects startup if this is true. Gemini's CachedContent is an
	// explicit, separately-managed resource (create/reference/invalidate),
	// structurally unlike Anthropic's per-request cache_control breakpoint,
	// and needs its own design pass (cache-key strategy, invalidation
	// trigger, TTL policy) that doesn't belong bolted onto this field.
	ContextCaching bool `yaml:"context_caching"`
}

// GeminiRotationConfig tunes internal/llm/gemini's key-rotation — same
// shape/reasoning as GeminiTTSConfig's QuotaCooldownSeconds/
// MaxQuotaRetryCycles, but this adapter's rotation trigger is broader
// (quota AND server errors — see internal/llm/gemini.isRetryable) than
// TTS's quota-only rotation.
type GeminiRotationConfig struct {
	CooldownSeconds int `yaml:"cooldown_seconds"`
	MaxRetryCycles  int `yaml:"max_retry_cycles"`
}

// EscalationConfig configures the explicit escalation tool that lets one
// provider hand a hard turn off to another. Lives on each LLMProvider (see
// its doc comment) rather than as a single global setting, so a chain of
// providers can each pick their own target/tool name.
type EscalationConfig struct {
	Enabled  bool   `yaml:"enabled"`
	ToolName string `yaml:"tool_name"`
	// Description overrides the escalation tool's description shown to the
	// model — the default is a generic "too complex/ambiguous/high-stakes"
	// prompt. Set this when a provider has a concrete, known-missing
	// capability worth calling out explicitly, e.g. a Gemini provider (no
	// working code-execution tool on the plain Developer API — see
	// internal/llm/gemini.nativeTools) naming that specifically, so the
	// model reliably escalates for it instead of attempting an answer it
	// has no way to actually compute.
	Description    string `yaml:"description,omitempty"`
	TargetProvider string `yaml:"target_provider"`
}

// LLMConfig is the ordered provider fallback chain.
type LLMConfig struct {
	// Providers is tried in order on error/timeout of the previous one,
	// unless DefaultProvider names one to try first instead.
	Providers []LLMProvider `yaml:"providers"`
	// DefaultProvider names which configured provider is tried first in the
	// fallback chain, independent of Providers' list order — router.New
	// moves this name to the front of its internal order. Empty means "use
	// Providers' list order as-is" (first entry wins).
	DefaultProvider string `yaml:"default_provider"`
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
	// EncryptionKeyAllowed opts this server into receiving the calling
	// user's unwrapped master key (see internal/keyring) as
	// a tool-call argument — e.g. a diary MCP server that encrypts entries
	// at rest. Only takes effect when URL is "https://" (see
	// EncryptionKeyPermitted) — validated at config-load time
	// (validateEncryptionKeyServers) and re-checked at startup when
	// cmd/miranda builds the runtime whitelist it hands to
	// httpapi.Orchestrator, since sending a real key over plaintext HTTP
	// would defeat the whole point. Defaults false: an MCP server must opt
	// in explicitly, so a newly-added or untrusted server never silently
	// receives key material.
	EncryptionKeyAllowed bool `yaml:"encryption_key_allowed,omitempty"`
	// EncryptionKeyArgName overrides the tool-call argument name this
	// server's schema expects the unwrapped master key under (see
	// EncryptionKeyArg). Only meaningful when EncryptionKeyAllowed is true.
	// Left unset, defaultEncryptionKeyArgName ("encryption_key") is used —
	// set this explicitly whenever a server's actual schema names the field
	// differently (e.g. the external miranda-diary repo's tools use
	// "record_encryption_key"; a mismatch here isn't caught at config-load
	// time, it surfaces as the MCP server rejecting every call from that
	// tool with a schema-validation error at call time, since the injected
	// field shows up as an unrecognized additional property).
	EncryptionKeyArgName string `yaml:"encryption_key_arg,omitempty"`
}

// defaultEncryptionKeyArgName is the tool-call argument name used for a
// server that opts into EncryptionKeyAllowed without overriding
// EncryptionKeyArgName.
const defaultEncryptionKeyArgName = "encryption_key"

// EncryptionKeyArg returns the tool-call argument name this server's schema
// expects the caller's unwrapped master key under: EncryptionKeyArgName if
// set, else defaultEncryptionKeyArgName.
func (s MCPServer) EncryptionKeyArg() string {
	if s.EncryptionKeyArgName != "" {
		return s.EncryptionKeyArgName
	}
	return defaultEncryptionKeyArgName
}

// EncryptionKeyPermitted reports whether this server may actually receive
// the caller's unwrapped master key: EncryptionKeyAllowed must be true AND
// the URL must be "https://" — sending real key material over plaintext
// HTTP would defeat the whole point of gating it. The single source of
// truth for that combined check, shared by validateEncryptionKeyServers
// (config-load time) and cmd/miranda's runtime whitelist computation
// (startup time), so the two can never drift apart on what "permitted"
// means.
func (s MCPServer) EncryptionKeyPermitted() bool {
	return s.EncryptionKeyAllowed && strings.HasPrefix(s.URL, "https://")
}

// FilesEndpoint returns the HTTP endpoint URL and bearer token for this
// server's file-transfer API: the server's own URL with its "/mcp" suffix
// replaced by "/files", authenticated with the same TokenEnv-resolved token
// as its MCP connection. Used by Config.SandboxFilesURL to build the
// sandbox's own download proxy URL (see internal/httpapi's handleDownload)
// — the sandbox protects its GET /files/{id} with the same bearer token as
// /mcp, not a separate one.
func (s MCPServer) FilesEndpoint() (filesURL, token string, err error) {
	if !strings.HasSuffix(s.URL, "/mcp") {
		return "", "", fmt.Errorf("config: mcp server %q url %q does not end with /mcp — cannot derive files endpoint", s.Name, s.URL)
	}
	filesURL = strings.TrimSuffix(s.URL, "/mcp") + "/files"
	if s.TokenEnv != "" {
		token = os.Getenv(s.TokenEnv)
	}
	return filesURL, token, nil
}

// MCPConfig lists the MCP servers (HA + others) available as tool sources.
type MCPConfig struct {
	Servers []MCPServer `yaml:"servers"`
}

// TavilyConfig configures Miranda's own web_search/web_fetch tools
// (internal/tools, backed by internal/tavily) — unlike LLMProvider's
// AnthropicTools/GeminiTools, these run through the Orchestrator's own tool
// loop (executeTool) exactly like remember_this or an MCP tool, so they're
// offered identically to every provider in the fallback/escalation chain
// rather than being tied to one specific backend's native tool support.
// This is what replaced Gemini's gemini_tools.google_search as this
// project's way of giving a cheap/free-tier model live web access: Grounding
// with Google Search turned out to have a zero quota on the free-tier
// Gemini Developer API, failing every single call with RESOURCE_EXHAUSTED
// rather than merely being rate-limited — see internal/llm/gemini's doc
// comment and config/llm.yaml.
type TavilyConfig struct {
	// APIKeyEnv names the environment variable holding the Tavily API key
	// (from https://app.tavily.com) — never stored in config.yaml directly,
	// same *_env convention as every other secret in this codebase.
	APIKeyEnv string              `yaml:"api_key_env"`
	WebSearch WebSearchToolConfig `yaml:"web_search"`
	WebFetch  WebFetchToolConfig  `yaml:"web_fetch"`
}

// WebSearchToolConfig controls the web_search tool (internal/tools). Opt-in
// (Enabled defaults false) since it needs a real Tavily API key, same
// posture as TelegramConfig/WebAuthnConfig.
type WebSearchToolConfig struct {
	Enabled bool `yaml:"enabled"`
	// MaxResults bounds how many results Tavily returns per search — kept
	// small since every result is spent straight into the model's context.
	MaxResults int `yaml:"max_results"`
}

// WebFetchToolConfig controls the web_fetch tool (internal/tools). Opt-in,
// same reasoning as WebSearchToolConfig.
type WebFetchToolConfig struct {
	Enabled bool `yaml:"enabled"`
}

// YandexStationConfig configures the Yandex Station-specific TTS parameters.
type YandexStationConfig struct {
	ChunkMaxChars      int `yaml:"chunk_max_chars"`
	IdlePollIntervalMS int `yaml:"idle_poll_interval_ms"`
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
	// DefaultDevice is the friendly name of the media_player entity that
	// receives TTS output when no specific device is requested — e.g.
	// "Станция Мини 3 Про". The dispatcher resolves this name to an
	// entity_id via the HA REST API at runtime (see ha.Client.ResolveMediaPlayer).
	// speak_reply's optional device argument overrides this per-call.
	DefaultDevice string `yaml:"default_device"`
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
			ScheduleSQLitePath: "./data/schedule.db",
			KeyringSQLitePath:  "./data/keyring.db",
			TTSCacheDir:        "./data/storage",
		},
		Logging: LoggingConfig{
			Dir:        "./logs",
			Level:      "info",
			MaxSizeMB:  10,
			MaxBackups: 5,
			MaxAgeDays: 30,
		},
		LLM: LLMConfig{
			Providers:       nil,
			DefaultProvider: "",
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
		// Disabled by default — needs a real Tavily API key, same posture as
		// TelegramConfig/WebAuthnConfig below.
		Tavily: TavilyConfig{
			APIKeyEnv: "TAVILY_API_KEY",
			WebSearch: WebSearchToolConfig{Enabled: false, MaxResults: 5},
			WebFetch:  WebFetchToolConfig{Enabled: false},
		},
		TTS: TTSConfig{
			// Opt-in gemini_tts is a new channel, not a default swap — this
			// stays the always-available, zero-external-dependency provider.
			Primary: "yandex_station_text",
			YandexStation: YandexStationConfig{
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

При вызове HassTurnOn, HassTurnOff и любых других инструментов управления
устройствами всегда указывай параметр area (а если известен — и floor),
когда он тебе известен из запроса пользователя или из GetLiveContext. В доме
есть устройства с одинаковыми названиями в разных комнатах, поэтому вызов
только с параметром name может выполнить действие не в той комнате. Если
после GetLiveContext видно, что устройств с таким name несколько и они
находятся в разных area, никогда не вызывай инструмент с одним только name —
либо передай area, определив нужную комнату из контекста разговора, либо
уточни комнату у пользователя, прежде чем действовать.

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
Если вопрос не связан с управлением домом или его состоянием, GetLiveContext вызывать не нужно.
Это не запрещает пользоваться другими инструментами: если для ответа нужны актуальные внешние данные (курсы валют, погода, новости и т.п.), используй веб-поиск/веб-fetch/выполнение кода. Инструменты remember_this, end_conversation, forget_conversation, speak_reply, stop_speech и send_telegram вызывай всегда, когда пользователь просит именно это действие, независимо от того, идёт ли речь о доме.
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
			RegisterWebhook: true,
		},
		Schedule: ScheduleConfig{
			Enabled: true,
		},
		FileUpload: FileUploadConfig{
			// Opt-in, same posture as Telegram/WebAuthn — requires a
			// configured sandbox MCP server URL to proxy uploads to, so
			// there's no safe auto-detected default.
			Enabled:                false,
			MaxFileSizeBytes:       50 * 1024 * 1024, // 50 MB
			DownloadRecordTTLHours: 720,              // 30 days
		},
		// No default Users: web UI login is mandatory and fails closed until
		// config.yaml lists at least one account (see internal/users).
		Users: nil,
	}
}

// SandboxFilesURL returns the HTTP endpoint URL and bearer token for the
// sandbox's file-upload API, derived from the MCP server named by
// FileUpload.SandboxMCPServerName. The endpoint URL is that server's URL
// with its "/mcp" suffix replaced by "/files"; the token is the resolved
// value of the server's TokenEnv environment variable. Returns an error if
// FileUpload is disabled, the server name is empty, or no matching MCP
// server entry exists.
func (c *Config) SandboxFilesURL() (filesURL, token string, err error) {
	if !c.FileUpload.Enabled {
		return "", "", fmt.Errorf("config: file_upload is not enabled")
	}
	if c.FileUpload.SandboxMCPServerName == "" {
		return "", "", fmt.Errorf("config: file_upload.sandbox_mcp_server_name is not set")
	}
	for _, s := range c.MCP.Servers {
		if s.Name == c.FileUpload.SandboxMCPServerName {
			return s.FilesEndpoint()
		}
	}
	return "", "", fmt.Errorf("config: no MCP server named %q found in mcp.servers", c.FileUpload.SandboxMCPServerName)
}

// Load reads the YAML file at path and merges it over Default(). A missing
// file is not an error: it just means the defaults are used as-is, so the
// agent has a working config out of the box.
//
// yaml.Unmarshal only overwrites fields present in the document, so starting
// from a fully-populated default struct gives us a cheap partial merge
// without a separate deep-merge step.
//
// Multiple paths are applied in order, each one layered over the result of
// the previous, so the config can be split into per-topic files (e.g.
// llm.yaml, tts.yaml, users.yaml). A missing file is silently skipped —
// only the files that exist need to be provided. Note that slices (users,
// mcp.servers) are replaced in full by whichever file last defines them, so
// each slice should live in exactly one file.
func Load(paths ...string) (Config, error) {
	cfg := Default()

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return cfg, fmt.Errorf("config: read %s: %w", path, err)
		}

		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	if err := validateMCPServerNames(cfg.MCP.Servers); err != nil {
		return cfg, err
	}
	if err := validateFileUploadConfig(cfg.FileUpload, cfg.MCP.Servers); err != nil {
		return cfg, err
	}
	if err := validateEncryptionKeyServers(cfg.MCP.Servers); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// validateMCPServerNames rejects two enabled mcp.servers entries sharing a
// name. Manager namespaces tools by "<server>_" and cmd/miranda spawns one
// background reconnect goroutine per server keyed on that name — a
// duplicate (e.g. a copy-pasted block in config.yaml left unrenamed) would
// race two independent connections against the same Manager slot, silently
// leaking whichever one loses instead of failing loudly at startup.
func validateMCPServerNames(servers []MCPServer) error {
	seen := make(map[string]bool, len(servers))
	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		if seen[s.Name] {
			return fmt.Errorf("config: mcp.servers has more than one enabled server named %q", s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}

// validateFileUploadConfig rejects a FileUploadConfig that names an MCP
// server which doesn't exist in the server list. The check only runs when
// file_upload.enabled is true and sandbox_mcp_server_name is set — if the
// feature is disabled there is nothing to validate.
func validateFileUploadConfig(cfg FileUploadConfig, servers []MCPServer) error {
	if !cfg.Enabled || cfg.SandboxMCPServerName == "" {
		return nil
	}
	for _, s := range servers {
		if s.Name == cfg.SandboxMCPServerName {
			return nil
		}
	}
	return fmt.Errorf("config: file_upload.sandbox_mcp_server_name %q does not match any entry in mcp.servers", cfg.SandboxMCPServerName)
}

// validateEncryptionKeyServers rejects an MCP server marked
// EncryptionKeyAllowed whose URL isn't "https://" (see
// MCPServer.EncryptionKeyPermitted) — sending a user's real master key over
// plaintext HTTP would defeat the point of gating it at all. Failing fast
// here at config-load time is a first line of defense; cmd/miranda
// re-checks the same condition (via the same EncryptionKeyPermitted method)
// when it builds the runtime whitelist at startup, so this stays true even
// if this validation is ever bypassed or the check drifts.
func validateEncryptionKeyServers(servers []MCPServer) error {
	for _, s := range servers {
		if s.EncryptionKeyAllowed && !s.EncryptionKeyPermitted() {
			return fmt.Errorf("config: mcp.servers %q has encryption_key_allowed=true but url %q is not https://", s.Name, s.URL)
		}
	}
	return nil
}
