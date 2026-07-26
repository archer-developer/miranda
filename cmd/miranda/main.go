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

	configPath := "config/config.yaml"
	if v := os.Getenv("MIRANDA_CONFIG"); v != "" {
		configPath = v
	}

	cfg, err := config.Load(configPath)
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

	toolManager := connectMCP(cfg.MCP.Servers, logger)

	dispatcher := buildTTSDispatcher(cfg.TTS, logger)

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

	defaultUserID := "debug"
	orchestrator := httpapi.NewOrchestrator(
		llmRouter, toolManager, historyStore, memoryStore, dispatcher, eventHub, usersRegistry,
		cfg.Agent, cfg.Memory, cfg.TTS, cfg.LLM.Escalation, cfg.TTS.YandexStation.ChunkMaxChars, defaultUserID,
	)

	var webHandler http.Handler
	if cfg.WebUI.Enabled {
		wh, err := webui.New(historyStore, memoryStore, webauthnSvc, usersRegistry, sessions, cfg.WebUI.DefaultLanguage, cfg.Storage.AvatarsDir)
		if err != nil {
			return err
		}
		webHandler = wh
	}

	server := httpapi.NewServer(orchestrator, eventHub, cfg.Server.AuthToken, webHandler, logger, usersRegistry, sessions)
	httpServer := &http.Server{Addr: cfg.Server.HTTPAddr, Handler: server}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
			providers = append(providers, anthropic.New(c.Name, c.Model, apiKey))
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

// connectMCP connects to every enabled configured MCP server. A server that
// fails to connect is logged and skipped rather than failing startup — a
// temporarily unreachable Home Assistant instance shouldn't prevent the
// agent from starting with its other tool sources.
func connectMCP(servers []config.MCPServer, logger *slog.Logger) *mcp.Manager {
	var clients []mcp.Client
	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		token := ""
		if s.TokenEnv != "" {
			token = os.Getenv(s.TokenEnv)
		}
		client, err := mcp.Connect(context.Background(), s.Name, s.URL, token)
		if err != nil {
			logger.Warn("mcp: failed to connect, skipping this server", "server", s.Name, "url", s.URL, "error", err)
			continue
		}
		clients = append(clients, client)
	}
	return mcp.NewManager(clients...)
}

// buildTTSDispatcher wires up the Home Assistant REST client TTS needs, if
// HA_BASE_URL is configured. TTS is entirely optional: without it the agent
// still answers via the HTTP API/web UI, just without speaking replies.
func buildTTSDispatcher(cfg config.TTSConfig, logger *slog.Logger) *tts.Dispatcher {
	baseURL := os.Getenv("HA_BASE_URL")
	if baseURL == "" {
		logger.Warn("HA_BASE_URL not set — TTS dispatch is disabled")
		return nil
	}
	haClient := ha.New(baseURL, os.Getenv("HA_TOKEN"))
	return tts.NewDispatcher(cfg, haClient)
}
