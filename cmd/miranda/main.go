// Command miranda is the agent's entrypoint: it loads config.yaml, wires up
// storage, LLM providers, MCP tool sources, TTS, and the unified HTTP
// command interface (plus the embedded web UI), and serves until
// interrupted. It compiles to a single self-contained binary — no Docker.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/envfile"
	"github.com/archer-developer/miranda/internal/ha"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/httpapi"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/llm/anthropic"
	"github.com/archer-developer/miranda/internal/llm/openaicompat"
	"github.com/archer-developer/miranda/internal/llm/router"
	"github.com/archer-developer/miranda/internal/llmtrace"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/memory"
	"github.com/archer-developer/miranda/internal/session"
	"github.com/archer-developer/miranda/internal/telegram"
	"github.com/archer-developer/miranda/internal/tts"
	"github.com/archer-developer/miranda/internal/users"
	"github.com/archer-developer/miranda/internal/webauthn"
	"github.com/archer-developer/miranda/internal/webui"
)

const shutdownTimeout = 10 * time.Second

// sessionTTL is how long a web UI login stays valid. Long-lived on purpose:
// this is a home dashboard on a trusted LAN, not a public multi-tenant
// service, and re-entering a password on every visit isn't worth the
// marginal security gain here.
const sessionTTL = 30 * 24 * time.Hour

// idleSweepInterval is how often the background memory sweeper checks for
// conversations that have gone idle. It's independent of
// config.MemoryConfig.SessionIdleTimeoutMinutes (that's the idle threshold
// itself) — this is just the polling cadence, and cheap to run often since
// it's a single indexed SQLite query.
const idleSweepInterval = time.Minute

// webauthnCeremonyTTL bounds how long a pending passkey registration/login
// ceremony (the gap between its begin and finish HTTP calls) stays valid —
// comfortably above the WebAuthn library's own ~60s ceremony timeout.
const webauthnCeremonyTTL = 2 * time.Minute

// mcpConnectTimeout bounds a single MCP connection attempt (the initial
// handshake), so a server that accepts the TCP connection but never answers
// "initialize" can't hang mcp.Manager.KeepConnected's retry loop forever.
const mcpConnectTimeout = 15 * time.Second

// mcpReconnectInterval is how often mcp.Manager.KeepConnected first retries
// an MCP server that's currently disconnected — at startup, and again any
// time mcp.Manager.Tools/Call evicts it after a disconnection. Doubles on
// each consecutive failure up to mcpMaxReconnectInterval, so an extended
// outage doesn't cost an indefinite connection attempt every 30s.
const mcpReconnectInterval = 30 * time.Second

// mcpMaxReconnectInterval caps the backoff mcpReconnectInterval grows into
// during an extended outage.
const mcpMaxReconnectInterval = 10 * time.Minute

// dotEnvPath is a .env file in the project root, loaded for local-dev
// convenience (see internal/envfile) so secrets like ANTHROPIC_API_KEY or
// HA_MCP_TOKEN don't need to be exported by hand every session. Real
// environment variables always win over it, so the same .env can sit
// untouched in production where those are set some other way.
const dotEnvPath = ".env"

func main() {
	// Bootstrap logger: stdout only, since config.yaml (which says where the
	// real log files go) hasn't been loaded yet. Only used for the handful of
	// messages between here and setupLogging succeeding.
	bootstrap := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := envfile.Load(dotEnvPath); err != nil {
		bootstrap.Warn("failed to load .env, continuing with the process environment as-is", "error", err)
	}

	configDir := "config"
	if v := os.Getenv("MIRANDA_CONFIG_DIR"); v != "" {
		configDir = v
	}
	configPaths, err := filepath.Glob(filepath.Join(configDir, "*.yaml"))
	if err != nil {
		bootstrap.Error("fatal", "error", err)
		os.Exit(1)
	}

	cfg, err := config.Load(configPaths...)
	if err != nil {
		bootstrap.Error("fatal", "error", err)
		os.Exit(1)
	}

	// Built before setupLogging so the app logger can also mirror into it
	// (see setupLogging) — the web UI's log-viewer screen and live event
	// pane both read from this one Hub over /ws/logs.
	eventHub := hub.New(cfg.WebUI.LogBufferSize)

	logger, closeLogs, err := setupLogging(cfg.Logging, eventHub)
	if err != nil {
		bootstrap.Error("fatal", "error", err)
		os.Exit(1)
	}
	defer closeLogs()

	if err := run(cfg, logger, eventHub); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// setupLogging builds the application logger so everything it logs is
// mirrored to a size-rotated file under cfg.Dir *and* into eventHub (as
// Source: "app_log" events, see hub.Hub.Writer) in addition to stdout — the
// same messages you'd see in the terminal are also on disk for later review
// and live on the web UI's log-viewer screen. The returned close func closes
// the underlying log file and should run at shutdown (best-effort:
// lumberjack writes synchronously, so nothing is lost even if the process
// exits without calling it).
func setupLogging(cfg config.LoggingConfig, eventHub *hub.Hub) (*slog.Logger, func(), error) {
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, func() {}, fmt.Errorf("main: create log dir %s: %w", cfg.Dir, err)
	}

	appLogFile := rotatingLogFile(cfg, "miranda.log")
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stdout, appLogFile, eventHub.Writer("app_log")), nil))
	return logger, func() { _ = appLogFile.Close() }, nil
}

// rotatingLogFile builds a size-rotated log file named filename under
// cfg.Dir, per cfg's rotation policy (see config.LoggingConfig).
func rotatingLogFile(cfg config.LoggingConfig, filename string) *lumberjack.Logger {
	return &lumberjack.Logger{
		Filename:   filepath.Join(cfg.Dir, filename),
		MaxSize:    cfg.MaxSizeMB,
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAgeDays,
		Compress:   true,
	}
}

func run(cfg config.Config, logger *slog.Logger, eventHub *hub.Hub) error {
	llmTraceFile := rotatingLogFile(cfg.Logging, "llm.log")
	defer func() { _ = llmTraceFile.Close() }()
	// Mirrored into eventHub as Source: "llm_log" events too, same as the
	// app logger above — the web UI's log-viewer screen tabs between the
	// two over the one existing /ws/logs connection.
	llmTracer := llmtrace.New(io.MultiWriter(llmTraceFile, eventHub.Writer("llm_log")))

	historyStore, err := history.Open(cfg.Storage.SQLitePath)
	if err != nil {
		return err
	}
	defer func() { _ = historyStore.Close() }()

	memoryStore, err := memory.New(cfg.Storage.MemoryDir)
	if err != nil {
		return err
	}

	providers, err := buildProviders(cfg.LLM.Providers)
	if err != nil {
		return err
	}
	llmRouter, err := router.New(providers, cfg.LLM.Escalation)
	if err != nil {
		return err
	}
	llmRouter.SetTracer(llmTracer)

	// Created here (rather than at its previous spot right before
	// serveUntilInterrupted) so it can also bound connectMCP's background
	// reconnect goroutines below — they must stop at shutdown like
	// everything else, not leak past it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	toolManager := connectMCP(ctx, cfg.MCP.Servers, logger)

	dispatcher, ttsAudioHandler := buildTTSDispatcher(cfg.TTS, cfg.Storage, eventHub, logger)

	usersRegistry, err := users.NewRegistry(cfg.Users)
	if err != nil {
		return err
	}
	if usersRegistry.Empty() {
		logger.Warn("no users configured — the web UI is unreachable until config.yaml lists at least one account under `users:`")
	}
	sessions := session.NewStore(sessionTTL)

	// webauthnSvc stays a true nil interface (not a nil *webauthn.Service)
	// when disabled, so webui.New's "webauthnSvc != nil" check to skip
	// registering the passkey routes actually works — assigning a nil
	// *webauthn.Service to an interface variable would produce a non-nil
	// interface wrapping a nil pointer, the classic Go footgun.
	var webauthnSvc webui.WebAuthnService
	if cfg.WebAuthn.Enabled {
		if cfg.WebAuthn.RPID == "" || len(cfg.WebAuthn.RPOrigins) == 0 {
			return fmt.Errorf("main: webauthn.enabled is true but rp_id/rp_origins are not configured")
		}
		webauthnStore, err := webauthn.Open(cfg.Storage.WebAuthnSQLitePath)
		if err != nil {
			return err
		}
		defer func() { _ = webauthnStore.Close() }()

		svc, err := webauthn.NewService(
			cfg.WebAuthn.RPID, cfg.WebAuthn.RPDisplayName, cfg.WebAuthn.RPOrigins,
			webauthnStore, webauthn.NewCeremonyStore(webauthnCeremonyTTL), usersRegistry,
		)
		if err != nil {
			return fmt.Errorf("main: configure webauthn: %w", err)
		}
		webauthnSvc = svc
	}

	tgClient, tgChats, tgSecret, err := setupTelegram(cfg.Telegram, cfg.Storage, logger)
	if err != nil {
		return err
	}

	defaultUserID := "debug"
	orchestrator := httpapi.NewOrchestrator(
		llmRouter, toolManager, historyStore, memoryStore, dispatcher, eventHub, usersRegistry,
		cfg.Agent, cfg.Memory, cfg.TTS, cfg.LLM.Escalation, ttsChunkMaxChars(cfg.TTS), defaultUserID,
	)
	if cfg.Telegram.Enabled {
		orchestrator.SetTelegram(telegram.NewSender(tgClient, tgChats), cfg.Telegram)
	}

	var webHandler http.Handler
	if cfg.WebUI.Enabled {
		wh, err := webui.New(historyStore, memoryStore, webauthnSvc, usersRegistry, sessions, cfg.WebUI.DefaultLanguage, cfg.Storage.AvatarsDir)
		if err != nil {
			return err
		}
		webHandler = wh
	}

	server := httpapi.NewServer(orchestrator, eventHub, cfg.Server.AuthToken, webHandler, logger, usersRegistry, sessions)
	if ttsAudioHandler != nil {
		server.SetTTSAudioHandler(ttsAudioHandler)
	}
	if cfg.Telegram.Enabled {
		server.SetTelegramWebhook(&httpapi.TelegramWebhook{
			Path:   cfg.Telegram.WebhookPath,
			Secret: tgSecret,
			Client: tgClient,
			Chats:  tgChats,
		})
	}
	httpServer := &http.Server{Addr: cfg.Server.HTTPAddr, Handler: server}

	go sweepIdleSessions(ctx, orchestrator, cfg.Memory, logger)

	return serveUntilInterrupted(ctx, httpServer, logger)
}

// sweepIdleSessions periodically distills conversations that have sat idle
// past cfg.SessionIdleTimeoutMinutes into their user's memory file and marks
// them ended (see Orchestrator.SummarizeIdleSessions). It's a no-op ticker
// when cfg.AutoSummarize is off, and exits once ctx is cancelled at shutdown.
func sweepIdleSessions(ctx context.Context, o *httpapi.Orchestrator, cfg config.MemoryConfig, logger *slog.Logger) {
	if !cfg.AutoSummarize {
		return
	}
	idleFor := time.Duration(cfg.SessionIdleTimeoutMinutes) * time.Minute

	ticker := time.NewTicker(idleSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := o.SummarizeIdleSessions(ctx, idleFor); err != nil {
				logger.Error("memory sweep failed", "error", err)
			}
		}
	}
}

func serveUntilInterrupted(ctx context.Context, httpServer *http.Server, logger *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", httpServer.Addr)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// buildProviders instantiates one llm.Provider per configured entry, in
// order — that order becomes the router's fallback chain.
func buildProviders(configs []config.LLMProvider) ([]llm.Provider, error) {
	var providers []llm.Provider
	for _, c := range configs {
		apiKey := ""
		if c.APIKeyEnv != "" {
			apiKey = os.Getenv(c.APIKeyEnv)
		}
		switch c.Type {
		case "anthropic":
			providers = append(providers, anthropic.New(c.Name, c.Model, apiKey, c.AnthropicTools))
		case "openai_compat":
			providers = append(providers, openaicompat.New(c.Name, c.BaseURL, c.Model, apiKey))
		default:
			return nil, fmt.Errorf("main: unknown llm provider type %q for provider %q", c.Type, c.Name)
		}
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("main: no llm.providers configured in config.yaml")
	}
	return providers, nil
}

// connectMCP returns a Manager immediately (with no clients yet) and spawns
// one mcp.Manager.KeepConnected goroutine per enabled server to connect it
// in the background — startup never blocks on, or fails because of, an
// unreachable MCP server (a temporarily-down Home Assistant instance, most
// notably). Each goroutine retries its server for as long as ctx is alive,
// so a server that's down at startup — or drops later, once
// mcp.Manager.Tools/Call evicts it after a genuine disconnection — is
// picked back up with no Miranda restart needed.
func connectMCP(ctx context.Context, servers []config.MCPServer, logger *slog.Logger) *mcp.Manager {
	manager := mcp.NewManager(logger)
	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		token := ""
		if s.TokenEnv != "" {
			token = os.Getenv(s.TokenEnv)
		}
		s := s
		connect := func(ctx context.Context) (mcp.Client, error) {
			return mcp.Connect(ctx, s.Name, s.URL, token)
		}
		go manager.KeepConnected(ctx, s.Name, mcpReconnectInterval, mcpMaxReconnectInterval, mcpConnectTimeout, connect)
	}
	return manager
}

// setupTelegram validates config and wires the optional Telegram bot
// channel: it opens the persisted chat-id store, builds a Bot API client,
// generates a fresh webhook secret (see telegram.RandomSecret's doc
// comment for why this isn't a configured value), and registers
// PublicBaseURL+WebhookPath as the bot's webhook. Returns a nil Client when
// cfg.Enabled is false — callers gate all other Telegram wiring on that
// same flag rather than a nil check, so the two can never drift apart.
//
// A failure to reach Telegram's API at startup is logged, not fatal — the
// same "don't block startup on a flaky external dependency" choice
// connectMCP makes, since registration just needs to succeed on some future
// restart, not this exact one.
func setupTelegram(cfg config.TelegramConfig, storageCfg config.StorageConfig, logger *slog.Logger) (*telegram.Client, *telegram.ChatStore, string, error) {
	if !cfg.Enabled {
		return nil, nil, "", nil
	}
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return nil, nil, "", fmt.Errorf("main: telegram.enabled is true but TELEGRAM_BOT_TOKEN is not set")
	}
	if cfg.PublicBaseURL == "" || cfg.WebhookPath == "" {
		return nil, nil, "", fmt.Errorf("main: telegram.enabled is true but public_base_url/webhook_path are not configured")
	}

	chats, err := telegram.OpenChatStore(storageCfg.TelegramChatsPath)
	if err != nil {
		return nil, nil, "", err
	}

	secret, err := telegram.RandomSecret()
	if err != nil {
		return nil, nil, "", err
	}

	client := telegram.New(token)
	webhookURL := strings.TrimRight(cfg.PublicBaseURL, "/") + cfg.WebhookPath
	if err := client.SetWebhook(context.Background(), webhookURL, secret); err != nil {
		logger.Error("telegram: failed to register webhook — the bot will not receive messages until this succeeds on a later restart",
			"error", err, "url", webhookURL)
	} else {
		logger.Info("telegram: webhook registered", "url", webhookURL)
	}

	return client, chats, secret, nil
}

// buildTTSDispatcher wires up the Home Assistant REST client TTS needs, if
// HA_BASE_URL is configured, plus the primary/fallback tts.Provider pair
// named by cfg.Primary/cfg.Fallback and (if a gemini_tts provider was built)
// the HTTP handler that serves its rendered audio files back to the
// station. TTS is entirely optional: without HA_BASE_URL the agent still
// answers via the HTTP API/web UI, just without speaking replies.
//
// A provider failing to build (e.g. gemini_tts requested with no usable API
// key) disables TTS dispatch entirely rather than falling back silently to
// some other provider the config didn't actually ask for — better to notice
// a misconfiguration in the logs than to have replies quietly go out over
// the wrong voice.
func buildTTSDispatcher(cfg config.TTSConfig, storageCfg config.StorageConfig, h *hub.Hub, logger *slog.Logger) (*tts.Dispatcher, *tts.HTTPHandler) {
	baseURL := os.Getenv("HA_BASE_URL")
	if baseURL == "" {
		logger.Warn("HA_BASE_URL not set — TTS dispatch is disabled")
		return nil, nil
	}
	haClient := ha.New(baseURL, os.Getenv("HA_TOKEN"))

	primary, err := tts.NewProvider(cfg.Primary, cfg, storageCfg.TTSCacheDir, haClient, logger)
	if err != nil {
		logger.Error("tts: failed to build primary provider, TTS dispatch is disabled", "error", err, "primary", cfg.Primary)
		return nil, nil
	}

	var fallback tts.Provider
	if cfg.Fallback != "" && cfg.Fallback != cfg.Primary {
		fallback, err = tts.NewProvider(cfg.Fallback, cfg, storageCfg.TTSCacheDir, haClient, logger)
		if err != nil {
			logger.Warn("tts: failed to build fallback provider, continuing without one", "error", err, "fallback", cfg.Fallback)
			fallback = nil
		}
	}

	dispatcher := tts.NewDispatcher(primary, fallback, haClient, cfg.YandexStation.Entities, h, logger)

	// Always registered (independent of which provider ended up being usable
	// above): a gemini_tts cache directory populated by an earlier, since-
	// reconfigured run should still be servable, and this is cheap to keep
	// around regardless.
	ttsHandler, err := tts.NewHTTPHandler(storageCfg.TTSCacheDir)
	if err != nil {
		logger.Error("tts: failed to create the /tts-audio HTTP handler", "error", err)
		return dispatcher, nil
	}
	return dispatcher, ttsHandler
}

// ttsChunkMaxChars picks the sentence-boundary chunk size (see
// tts.Accumulator/Chunk) the agent loop uses to split streaming LLM text
// before handing it to TTS — sized to whichever provider is actually
// configured as primary, since gemini_tts renders a real audio file and
// isn't limited by what Yandex Station's own on-device TTS can swallow in
// one call the way yandex_station_text is. Chunking itself is always
// maximally greedy regardless of provider (see Accumulator's doc comment) —
// only the limit differs.
func ttsChunkMaxChars(cfg config.TTSConfig) int {
	if cfg.Primary == "gemini_tts" {
		return cfg.GeminiTTS.ChunkMaxChars
	}
	return cfg.YandexStation.ChunkMaxChars
}
