// Command miranda is the agent's entrypoint: it loads config.yaml, wires up
// storage, LLM providers, MCP tool sources, TTS, and the unified HTTP
// command interface (plus the embedded web UI), and serves until
// interrupted. It compiles to a single self-contained binary — no Docker.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/memory"
	"github.com/archer-developer/miranda/internal/session"
	"github.com/archer-developer/miranda/internal/tts"
	"github.com/archer-developer/miranda/internal/users"
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

// dotEnvPath is a .env file in the project root, loaded for local-dev
// convenience (see internal/envfile) so secrets like ANTHROPIC_API_KEY or
// HA_MCP_TOKEN don't need to be exported by hand every session. Real
// environment variables always win over it, so the same .env can sit
// untouched in production where those are set some other way.
const dotEnvPath = ".env"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := envfile.Load(dotEnvPath); err != nil {
		logger.Warn("failed to load .env, continuing with the process environment as-is", "error", err)
	}

	configPath := "config/config.yaml"
	if v := os.Getenv("MIRANDA_CONFIG"); v != "" {
		configPath = v
	}

	if err := run(configPath, logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

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

	toolManager := connectMCP(cfg.MCP.Servers, logger)

	dispatcher := buildTTSDispatcher(cfg.TTS, logger)

	eventHub := hub.New(cfg.WebUI.LogBufferSize)

	usersRegistry, err := users.NewRegistry(cfg.Users)
	if err != nil {
		return err
	}
	if usersRegistry.Empty() {
		logger.Warn("no users configured — the web UI is unreachable until config.yaml lists at least one account under `users:`")
	}
	sessions := session.NewStore(sessionTTL)

	defaultUserID := "debug"
	orchestrator := httpapi.NewOrchestrator(
		llmRouter, toolManager, historyStore, memoryStore, dispatcher, eventHub,
		cfg.Agent, cfg.Memory, cfg.LLM.Escalation, cfg.TTS.YandexStation.ChunkMaxChars, defaultUserID,
	)

	var webHandler http.Handler
	if cfg.WebUI.Enabled {
		wh, err := webui.New(historyStore, memoryStore, usersRegistry, sessions, cfg.WebUI.DefaultLanguage, cfg.Storage.AvatarsDir)
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
