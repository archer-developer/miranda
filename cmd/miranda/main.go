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

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/anthropic"
	"github.com/archer-developer/miranda-llm/gemini"
	"github.com/archer-developer/miranda-llm/llmtrace"
	"github.com/archer-developer/miranda-llm/openaicompat"
	"github.com/archer-developer/miranda-llm/router"
	agentloop "github.com/archer-developer/miranda/internal/agent_loop"
	"github.com/archer-developer/miranda/internal/attachments"
	"github.com/archer-developer/miranda/internal/backup"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/envfile"
	"github.com/archer-developer/miranda/internal/ha"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/httpapi"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/keyring"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/memory"
	"github.com/archer-developer/miranda/internal/oauth2"
	"github.com/archer-developer/miranda/internal/redact"
	"github.com/archer-developer/miranda/internal/schedule"
	"github.com/archer-developer/miranda/internal/session"
	"github.com/archer-developer/miranda/internal/tavily"
	"github.com/archer-developer/miranda/internal/telegram"
	"github.com/archer-developer/miranda/internal/tlscert"
	"github.com/archer-developer/miranda/internal/tools"
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

// scheduleSweepInterval is how often the background scheduled-task sweeper
// checks for due tasks — independent of any individual task's own
// recurrence, this is just the polling cadence (see sweepScheduledTasks).
const scheduleSweepInterval = time.Minute

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

// oauthRefreshTickInterval is how often oauth2.Service.StartRefresher polls
// for access tokens nearing their proactive-refresh margin — cheap (a
// single indexed SQLite query when nothing is due), so a short interval
// just means refresh happens close to that margin rather than up to a whole
// interval late.
const oauthRefreshTickInterval = time.Minute

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

	// `miranda backup` runs one backup cycle (internal/backup.Run) and exits
	// — for an on-demand run (before trusting the ticker, or right before a
	// risky change) without starting the full service (HTTP server, MCP
	// connections, TTS, ...). Uses cfg.Backup.Dir/RetentionCount regardless
	// of cfg.Backup.Enabled, since that flag only gates the automatic
	// ticker in run/sweepBackups, not this explicit invocation.
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		if err := backup.Run(context.Background(), cfg.Backup, cfg.Storage, configDir, dotEnvPath, bootstrap); err != nil {
			bootstrap.Error("backup failed", "error", err)
			os.Exit(1)
		}
		return
	}

	// `miranda llm-trace` reads back logs/llm.log (see runLLMTrace's own doc
	// comment) — a diagnostic, not the running service, so it exits here the
	// same way `backup` does, before the full service wiring below.
	if len(os.Args) > 1 && os.Args[1] == "llm-trace" {
		if err := runLLMTrace(cfg.Logging, os.Args[2:]); err != nil {
			bootstrap.Error("llm-trace failed", "error", err)
			os.Exit(1)
		}
		return
	}

	// Built before setupLogging so the app logger can also mirror into it
	// (see setupLogging) — the web UI's log-viewer screen and live event
	// pane both read from this one Hub over /ws/logs. app_log/llm_log get
	// their own replay-buffer policy (see hub.SourceLimit's doc comment for
	// why they can't share one) — every other source falls back to a plain
	// count cap of LogBufferSize.
	eventHub := hub.New(cfg.WebUI.LogBufferSize, map[string]hub.SourceLimit{
		"app_log": {MaxBytes: cfg.WebUI.AppLogMaxKB * 1024},
		"llm_log": {MaxCount: cfg.WebUI.LLMLogMaxBlocks},
	})

	logger, closeLogs, err := setupLogging(cfg.Logging, eventHub)
	if err != nil {
		bootstrap.Error("fatal", "error", err)
		os.Exit(1)
	}

	os.Exit(runAndClose(cfg, logger, eventHub, configDir, closeLogs))
}

// runAndClose wraps run so closeLogs runs (via defer) before main calls
// os.Exit — os.Exit itself never runs deferred calls, so that has to happen
// in a function main returns from normally, not in main's own body.
func runAndClose(cfg config.Config, logger *slog.Logger, eventHub *hub.Hub, configDir string, closeLogs func()) int {
	defer closeLogs()
	if err := run(cfg, logger, eventHub, configDir); err != nil {
		logger.Error("fatal", "error", err)
		return 1
	}
	return 0
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

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, func() {}, fmt.Errorf("main: invalid log level %q: %w", cfg.Level, err)
	}

	appLogFile := rotatingLogFile(cfg, "miranda.log")
	opts := &slog.HandlerOptions{Level: level}
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stdout, appLogFile, eventHub.Writer("app_log")), opts))
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

// configDir is passed through (rather than re-derived from
// os.Getenv("MIRANDA_CONFIG_DIR") here too) so run's only source of truth
// for where config.yaml's *.yaml files live is main's own resolution of it,
// used both to load cfg and, later, by sweepBackups to back the same
// directory up.
//
//nolint:gocyclo // linear service-wiring: each step is an independent `if err != nil { return err }`, not branching logic — splitting it up would scatter one bootstrap sequence across files without reducing real complexity
func run(cfg config.Config, logger *slog.Logger, eventHub *hub.Hub, configDir string) error {
	if err := validateEscalationToolNames(cfg.LLM.Providers); err != nil {
		return err
	}

	llmTraceFile := rotatingLogFile(cfg.Logging, "llm.log")
	defer func() { _ = llmTraceFile.Close() }()
	// Mirrored into eventHub as Source: "llm_log" events too, same as the
	// app logger above — the web UI's log-viewer screen tabs between the
	// two over the one existing /ws/logs connection. Unlike the app logger's
	// plain hub.Writer, this uses hub.Hub.LLMTraceWriter: it reassembles
	// each traced call into a whole miranda-llm/llmtrace/analyze.Block
	// before publishing, so the frontend renders already-parsed calls
	// instead of reassembling a flood of raw lines itself.
	// Wrapped in ContextTracer (rather than SetTracer(llmTracer) directly)
	// so Handle can tee an extra, turn-scoped anomaly.Recorder onto this same
	// stream via ctx — see llmtrace.WithTracer's doc comment for why that's
	// safe despite the tracer being installed once, globally, below.
	llmTracer := &llmtrace.ContextTracer{Default: llmtrace.New(io.MultiWriter(llmTraceFile, eventHub.LLMTraceWriter("llm_log")))}

	// Built before any store that will use it, so a bad lexicon or an unknown
	// format-rule name fails the boot rather than surfacing on the first
	// message that would have been masked. A nil redactor (redaction turned
	// off) is a working pass-through, so every SetRedactor call below is
	// unconditional. Note this is the *opposite* of the webauthnSvc case
	// further down: there a nil pointer in an interface is a footgun, here it
	// is the intended design — (*redact.Redactor).Redact has a nil-receiver
	// path, so the non-nil interface wrapping a nil pointer behaves exactly
	// as a disabled redactor should.
	redactor, err := redact.New(redact.Config(cfg.Redact))
	if err != nil {
		return err
	}
	if redactor == nil {
		logger.Warn("redaction is disabled — pin codes, passwords and API keys will be written verbatim to the dialog history, memory files and logs/llm.log")
	}

	historyStore, err := history.Open(cfg.Storage.SQLitePath)
	if err != nil {
		return err
	}
	defer func() { _ = historyStore.Close() }()
	historyStore.SetRedactor(redactor)

	memoryStore, err := memory.New(cfg.Storage.MemoryDir)
	if err != nil {
		return err
	}
	memoryStore.SetRedactor(redactor)

	var scheduleStore *schedule.Store
	if cfg.Schedule.Enabled {
		scheduleStore, err = schedule.Open(cfg.Storage.ScheduleSQLitePath)
		if err != nil {
			return err
		}
		defer func() { _ = scheduleStore.Close() }()
		scheduleStore.SetRedactor(redactor)
	}

	// Created here (rather than at its previous spot right before
	// serveUntilInterrupted) so it can also bound buildProviders' gemini.New
	// calls and connectMCP's background reconnect goroutines below — they
	// must stop at shutdown like everything else, not leak past it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	providers, err := buildProviders(ctx, cfg.LLM.Providers, logger)
	if err != nil {
		return err
	}
	llmRouter, err := router.New(providers, buildEscalations(cfg.LLM.Providers), cfg.LLM.DefaultProvider)
	if err != nil {
		return err
	}
	// Masking wraps the *outer* ContextTracer rather than its Default. That
	// one placement covers every consumer of a trace at once, because
	// ContextTracer fans out below this point: logs/llm.log, the web UI's
	// live LLM-trace tab, and the per-turn anomaly.Recorder that Handle
	// attaches via ctx — which in turn is what keeps logs/anomalies/ clean.
	// Wrapping Default instead would leave the ctx-attached recorder
	// receiving the unmasked dump.
	llmRouter.SetTracer(&redact.Tracer{Next: llmTracer, Redactor: redactor})

	toolManager := connectMCP(ctx, cfg.MCP.Servers, logger)

	webTools, err := buildWebTools(cfg.Tavily, logger)
	if err != nil {
		return err
	}

	dispatcher, ttsAudioHandler, ttsHAClient := buildTTSDispatcher(cfg.TTS, cfg.Storage, eventHub, logger)

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

	// The keyring is always on (no config toggle — see internal/keyring) so
	// that a user's data-encryption key is available from their very first
	// login, regardless of when this feature shipped relative to their
	// account's creation.
	keyringStore, err := keyring.Open(cfg.Storage.KeyringSQLitePath)
	if err != nil {
		return fmt.Errorf("main: configure keyring: %w", err)
	}
	defer func() { _ = keyringStore.Close() }()
	keyringService := keyring.NewService(keyringStore, keyring.NewCache())

	tgClient, tgChats, tgSecret, err := setupTelegram(cfg.Telegram, cfg.Storage, logger)
	if err != nil {
		return err
	}

	oauthStore, oauthSvc, err := setupOAuth(cfg.OAuth, cfg.Storage, logger)
	if err != nil {
		return err
	}
	if oauthStore != nil {
		defer func() { _ = oauthStore.Close() }()
	}

	defaultUserID := "debug"
	orchestrator := agentloop.NewOrchestrator(
		llmRouter, toolManager, historyStore, memoryStore, dispatcher, eventHub, usersRegistry,
		cfg.Agent, cfg.Memory, cfg.TTS, ttsChunkMaxChars(cfg.TTS), defaultUserID,
	)
	orchestrator.SetLogger(logger)
	orchestrator.SetRedactor(redactor)
	// Unconditional, unlike medical-card's own equivalent wiring — Miranda's
	// llm.log is always on (no debug-only gate), so there's always a real
	// tracer for a turn's Recorder to tee onto. See agentloop.AnomalyConfig.
	orchestrator.SetAnomalyConfig(agentloop.AnomalyConfig{
		LLMLogPath: filepath.Join(cfg.Logging.Dir, "llm.log"),
		Dir:        filepath.Join(cfg.Logging.Dir, "anomalies"),
	})
	if cfg.Telegram.Enabled {
		orchestrator.SetTelegram(telegram.NewSender(tgClient, tgChats), cfg.Telegram)
	}
	if len(webTools) > 0 {
		orchestrator.SetWebTools(webTools)
	}
	if cfg.Schedule.Enabled {
		orchestrator.SetSchedule(scheduleStore)
	}
	if ttsHAClient != nil {
		orchestrator.SetSpeakerHA(ttsHAClient)
	}
	orchestrator.SetKeyring(keyringService)
	if lazy := cfg.LazyMCPServers(); len(lazy) > 0 {
		orchestrator.SetLazyMCPServers(lazy)
	}
	if oauthSvc != nil {
		orchestrator.SetOAuth(oauthSvc, mcpReconnectInterval, mcpMaxReconnectInterval, mcpConnectTimeout)
		go oauthSvc.StartRefresher(ctx, oauthRefreshTickInterval)
	}

	// mcpExtensions bundles every configured MCP server's static opt-ins
	// (encryption-key/session-id injection now, file-URI download detection
	// merged in below once it's known) into the one map
	// SetMCPServerExtensions takes — see agentloop.MCPServerExtension.
	mcpExtensions := mcpServerExtensions(cfg.MCP.Servers, logger)
	var downloadRecordTTL time.Duration

	// File upload is opt-in (Enabled defaults false). Resolve config early
	// so a misconfigured expose_files entry fails fast before the HTTP
	// server starts. The attachments.Store and SetUploadHandler wiring
	// happens after server construction below (SetUploadHandler registers
	// the route on the server's mux, which only exists after NewServer).
	var uploadAttachStore *attachments.Store
	if cfg.FileUpload.Enabled {
		uploadAttachStore = attachments.NewStore(0) // 0 → default 1-hour TTL
		defer uploadAttachStore.Close()
		orchestrator.SetAttachmentStore(uploadAttachStore)

		// Every file-serving MCP server (the sandbox included — it has no
		// dedicated path of its own here, see config.MCPServer.ExposeFiles)
		// opts in via expose_files: true; executeTool scans that server's
		// tool results for an embedded file URI rather than Miranda relying
		// on any one hardcoded tool name.
		fileExposingServers, err := cfg.FileExposingServers()
		if err != nil {
			return fmt.Errorf("main: configure file upload: %w", err)
		}
		if len(fileExposingServers) == 0 {
			// Not fatal — a deployment may genuinely only want the upload
			// direction — but silent is worse than loud here: this is the
			// exact shape of a forgotten/mismatched expose_files: true that
			// used to fail startup outright before servers could opt in
			// individually. Surface it instead of letting it be discovered
			// only when a user reports a missing download chip.
			logger.Warn("main: file_upload is enabled but no MCP server has expose_files: true — tool-returned files will never be detected or downloadable")
		}
		for name, endpoint := range fileExposingServers {
			endpoint := endpoint
			ext := mcpExtensions[name]
			ext.FilesEndpoint = &endpoint
			mcpExtensions[name] = ext
		}
		downloadRecordTTL = time.Duration(cfg.FileUpload.DownloadRecordTTLHours) * time.Hour

		// Required for processAttachments to build a fileURI any tool can
		// pull an upload's bytes from (see docs/file-staging-refactor.md) —
		// fail fast at startup rather than silently serve broken URIs,
		// mirroring gemini_tts's identical PublicBaseURL requirement
		// (internal/tts/gemini.go).
		if cfg.FileUpload.PublicBaseURL == "" {
			return fmt.Errorf("main: file_upload.public_base_url is not configured — other services have no way to fetch an uploaded file's bytes")
		}
		orchestrator.SetFilesPublicBaseURL(cfg.FileUpload.PublicBaseURL)
	}
	orchestrator.SetMCPServerExtensions(mcpExtensions, downloadRecordTTL)

	var webHandler http.Handler
	if cfg.WebUI.Enabled {
		wh, err := webui.New(historyStore, memoryStore, orchestrator, webauthnSvc, keyringService, usersRegistry, sessions, cfg.WebUI.DefaultLanguage, cfg.Storage.AvatarsDir, logger)
		if err != nil {
			return err
		}
		webHandler = wh
	}

	server := httpapi.NewServer(orchestrator, eventHub, cfg.Server.AuthToken, webHandler, logger, usersRegistry, sessions)
	if ttsAudioHandler != nil {
		server.SetTTSAudioHandler(ttsAudioHandler)
	}
	if cfg.FileUpload.Enabled {
		server.SetUploadHandler(cfg.FileUpload.MaxFileSizeBytes)
	}
	if cfg.Telegram.Enabled {
		server.SetTelegramWebhook(&httpapi.TelegramWebhook{
			Path:   cfg.Telegram.WebhookPath,
			Secret: tgSecret,
			Client: tgClient,
			Chats:  tgChats,
		})
	}
	if oauthSvc != nil {
		server.SetOAuthCallback(&httpapi.OAuthCallback{
			PathPrefix: cfg.OAuth.CallbackPath,
			Service:    oauthSvc,
		})
	}
	httpServer := &http.Server{
		Addr:    cfg.Server.HTTPAddr,
		Handler: server,
		// ReadHeaderTimeout guards against Slowloris; WriteTimeout is the
		// hard ceiling for the entire request lifecycle, covering worst-case
		// LLM escalation chains and key-rotation retries. Keep the two in
		// sync: if you raise WriteTimeout here, raise the client-side fetch
		// timeout (if any) to match.
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      5 * time.Minute,
	}

	// HTTPS is additive, never a replacement: httpServer above keeps serving
	// plain HTTP on cfg.Server.HTTPAddr regardless of cfg.Server.TLS.Enabled.
	var httpsServer *http.Server
	if cfg.Server.TLS.Enabled {
		if err := tlscert.EnsureSelfSigned(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile, cfg.Server.TLS.Hosts, logger); err != nil {
			return fmt.Errorf("main: prepare TLS certificate: %w", err)
		}
		httpsServer = &http.Server{
			Addr:              cfg.Server.TLS.Addr,
			Handler:           server,
			ReadHeaderTimeout: 30 * time.Second,
			WriteTimeout:      5 * time.Minute,
		}
	}

	go sweepIdleSessions(ctx, orchestrator, cfg.Memory, logger)
	go sweepScheduledTasks(ctx, orchestrator, cfg.Schedule, logger)
	go sweepBackups(ctx, cfg.Backup, cfg.Storage, configDir, dotEnvPath, logger)

	return serveUntilInterrupted(ctx, httpServer, httpsServer, cfg.Server.TLS, logger)
}

// sweepIdleSessions periodically marks conversations that have sat idle past
// cfg.SessionIdleTimeoutMinutes as ended, distilling each into its user's
// memory file first when cfg.AutoSummarize allows it (see
// Orchestrator.SummarizeIdleSessions/summarizeConversation — that flag
// controls only whether the LLM-based recap/memory step runs, never whether
// the session actually closes on schedule). Exits once ctx is cancelled at
// shutdown.
func sweepIdleSessions(ctx context.Context, o *agentloop.Orchestrator, cfg config.MemoryConfig, logger *slog.Logger) {
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

// sweepScheduledTasks periodically fires any scheduled tasks that are due
// (see Orchestrator.RunScheduledTasks). It's a no-op ticker when
// cfg.Enabled is off, and exits once ctx is cancelled at shutdown.
func sweepScheduledTasks(ctx context.Context, o *agentloop.Orchestrator, cfg config.ScheduleConfig, logger *slog.Logger) {
	if !cfg.Enabled {
		return
	}

	ticker := time.NewTicker(scheduleSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := o.RunScheduledTasks(ctx, logger); err != nil {
				logger.Error("scheduled task sweep failed", "error", err)
			}
		}
	}
}

// sweepBackups runs a full database + config backup (internal/backup.Run)
// every cfg.IntervalMinutes. Unlike sweepIdleSessions/sweepScheduledTasks,
// the ticker interval here isn't a separate polling cadence decoupled from
// some per-item due time — every tick just performs a backup, so cfg.
// IntervalMinutes drives the ticker directly. A no-op if cfg.Enabled is
// false, and exits once ctx is cancelled at shutdown.
func sweepBackups(ctx context.Context, cfg config.BackupConfig, storageCfg config.StorageConfig, configDir, envPath string, logger *slog.Logger) {
	if !cfg.Enabled {
		return
	}

	ticker := time.NewTicker(time.Duration(cfg.IntervalMinutes) * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := backup.Run(ctx, cfg, storageCfg, configDir, envPath, logger); err != nil {
				logger.Error("database backup failed", "error", err)
			}
		}
	}
}

// serveUntilInterrupted runs httpServer (always) and httpsServer (only if
// non-nil, i.e. cfg.Server.TLS.Enabled) concurrently until ctx is cancelled,
// then shuts both down. Either listener failing outright (anything but a
// graceful http.ErrServerClosed) is treated as fatal for the whole process —
// consistent with the single-listener behavior this replaced, and simpler
// than trying to keep running on just one of the two.
func serveUntilInterrupted(ctx context.Context, httpServer, httpsServer *http.Server, tlsCfg config.TLSConfig, logger *slog.Logger) error {
	errCh := make(chan error, 2)
	go func() {
		logger.Info("listening", "addr", httpServer.Addr, "scheme", "http")
		errCh <- httpServer.ListenAndServe()
	}()
	if httpsServer != nil {
		go func() {
			logger.Info("listening", "addr", httpsServer.Addr, "scheme", "https")
			errCh <- httpsServer.ListenAndServeTLS(tlsCfg.CertFile, tlsCfg.KeyFile)
		}()
	}

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
		err := httpServer.Shutdown(shutdownCtx)
		if httpsServer != nil {
			if httpsErr := httpsServer.Shutdown(shutdownCtx); httpsErr != nil && err == nil {
				err = httpsErr
			}
		}
		return err
	}
}

// buildProviders instantiates one llm.Provider per configured entry, in
// order — that order becomes the router's fallback chain (unless
// LLMConfig.DefaultProvider reorders it, see router.New). ctx bounds
// gemini.New's client construction, which needs a real request-scoped
// context the same way connectMCP's background goroutines do.
func buildProviders(ctx context.Context, configs []config.LLMProvider, logger *slog.Logger) ([]llm.Provider, error) {
	var providers []llm.Provider
	for _, c := range configs {
		switch c.Type {
		case "anthropic":
			providers = append(providers, anthropic.New(c.Name, c.Model, firstAPIKey(c.APIKeyEnvs), anthropic.ToolsConfig(c.AnthropicTools)))
		case "openai_compat":
			providers = append(providers, openaicompat.New(c.Name, c.BaseURL, c.Model, firstAPIKey(c.APIKeyEnvs)))
		case "gemini":
			p, err := gemini.New(ctx, c.Name, c.Model, c.APIKeyEnvs, gemini.ToolsConfig(c.GeminiTools), gemini.RotationConfig(c.GeminiRotation), logger)
			if err != nil {
				return nil, fmt.Errorf("main: build gemini provider %q: %w", c.Name, err)
			}
			providers = append(providers, p)
		default:
			return nil, fmt.Errorf("main: unknown llm provider type %q for provider %q", c.Type, c.Name)
		}
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("main: no llm.providers configured in config.yaml")
	}
	return providers, nil
}

// firstAPIKey resolves the first non-empty env var in envs. anthropic/
// openai_compat providers take a single credential — APIKeyEnvs is a list
// on config.LLMProvider only for schema consistency with gemini's
// multi-key rotation (see that field's doc comment); any entries beyond
// the first are ignored for those two provider types.
func firstAPIKey(envs []string) string {
	for _, e := range envs {
		if v := os.Getenv(e); v != "" {
			return v
		}
	}
	return ""
}

// validateEscalationToolNames rejects a config where any provider's
// escalation.tool_name collides with one of agentloop.ReservedToolNames() —
// see that function's doc comment for why a collision would silently
// swallow real tool calls instead of erroring loudly. Checked once at
// startup, before any provider/router construction, so a config mistake
// fails fast the same way validateMCPServerNames/
// validateNoDuplicateWebTools already do for their own analogous
// collisions (internal/config.Load).
func validateEscalationToolNames(providers []config.LLMProvider) error {
	reserved := make(map[string]bool)
	for _, name := range agentloop.ReservedToolNames() {
		reserved[name] = true
	}
	for _, p := range providers {
		if p.Escalation.Enabled && reserved[p.Escalation.ToolName] {
			return fmt.Errorf("main: llm.providers[%q].escalation.tool_name %q collides with one of Miranda's own tool names", p.Name, p.Escalation.ToolName)
		}
	}
	return nil
}

// buildEscalations extracts each provider's own EscalationConfig, keyed by
// name, for router.New — see config.LLMProvider.Escalation's doc comment
// for why this moved off a single global LLMConfig.Escalation.
func buildEscalations(configs []config.LLMProvider) map[string]router.EscalationConfig {
	m := make(map[string]router.EscalationConfig, len(configs))
	for _, c := range configs {
		m[c.Name] = router.EscalationConfig(c.Escalation)
	}
	return m
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

	// SetBackgroundContext is harmless to set even when no server is
	// OAuth-gated — EnsureUserSession simply never gets called in that case
	// — and it must be set before any per-user session could possibly be
	// requested, so doing it unconditionally here (rather than only when
	// OAuth2 is configured) avoids a startup-ordering dependency between
	// this function and setupOAuth.
	manager.SetBackgroundContext(ctx)

	oauthServers := make(map[string]bool)
	for _, s := range servers {
		if s.OAuthProvider != "" {
			oauthServers[s.Name] = true
		}
	}
	manager.SetOAuthServers(oauthServers)

	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		if s.OAuthProvider != "" {
			// An OAuth-gated server gets no single shared global connection
			// — each household member's own per-user session is instead
			// brought up lazily by agentloop.Orchestrator.executeTool via
			// mcp.Manager.EnsureUserSession once they've authorized (see
			// docs/adr/oauth2-layer.md). config.validateOAuthServers already
			// guarantees such a server is Lazy, so it never appears in a
			// turn's default tool list before that happens.
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

// mcpServerExtensions computes every configured MCP server's static,
// config-driven opt-ins — encryption-key injection permission and session-id
// injection permission (file-URI download detection is merged in
// separately by the caller, since it's only resolved when
// config.FileUploadConfig.Enabled and can fail config validation — see
// config.Config.FileExposingServers) — keyed by server name, ready to hand
// to agentloop.Orchestrator.SetMCPServerExtensions. Entirely independent of
// connection lifecycle, so this is deliberately its own function rather than
// folded into connectMCP: Manager's job is connection bookkeeping over
// live/reconnecting clients, not a static permission store. Logs loudly on
// an encryption-key mismatch even though config.validateEncryptionKeyServers
// should already have rejected EncryptionKeyAllowed=true on a non-https URL
// at load time — defense in depth, not a substitute for that validation. A
// server absent from the returned map (or present with zero-value fields)
// simply has none of these behaviors.
func mcpServerExtensions(servers []config.MCPServer, logger *slog.Logger) map[string]agentloop.MCPServerExtension {
	extensions := make(map[string]agentloop.MCPServerExtension, len(servers))
	for _, s := range servers {
		var ext agentloop.MCPServerExtension

		permitted := s.EncryptionKeyPermitted()
		if s.EncryptionKeyAllowed && !permitted {
			logger.Warn("mcp: encryption_key_allowed set but server url is not https, refusing to grant", "server", s.Name, "url", s.URL)
		}
		if permitted {
			ext.EncryptionKeyArg = s.EncryptionKeyArg()
		}

		if len(s.SessionIDTools) > 0 {
			tools := make(map[string]bool, len(s.SessionIDTools))
			for _, t := range s.SessionIDTools {
				tools[t] = true
			}
			ext.SessionIDArg = s.SessionIDArg()
			ext.SessionIDTools = tools
		}

		if s.OAuthProvider != "" {
			ext.OAuthProvider = s.OAuthProvider
			ext.MCPServerURL = s.URL
		}

		extensions[s.Name] = ext
	}
	return extensions
}

// buildWebTools constructs Miranda's own web_search/web_fetch tools (see
// internal/tools) when config.TavilyConfig enables at least one of them —
// both share a single tavily.Client/API key. Returns a nil slice (not an
// error) when neither is enabled, the default: unlike MCP servers, a
// missing/wrong Tavily key is a startup-time config mistake worth failing
// fast on, rather than something to retry in the background, since there's
// no "comes back later" scenario for a typo'd env var name the way there is
// for a temporarily-down MCP server.
func buildWebTools(cfg config.TavilyConfig, logger *slog.Logger) ([]tools.Tool, error) {
	if !cfg.WebSearch.Enabled && !cfg.WebFetch.Enabled {
		return nil, nil
	}
	apiKey := os.Getenv(cfg.APIKeyEnv)
	if apiKey == "" {
		return nil, fmt.Errorf("main: tavily.web_search/web_fetch enabled but %s is not set", cfg.APIKeyEnv)
	}
	client := tavily.New(apiKey)

	var out []tools.Tool
	if cfg.WebSearch.Enabled {
		out = append(out, tools.NewWebSearch(client, cfg.WebSearch, logger))
	}
	if cfg.WebFetch.Enabled {
		out = append(out, tools.NewWebFetch(client, logger))
	}
	return out, nil
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
	if !cfg.RegisterWebhook {
		// See TelegramConfig.RegisterWebhook's doc comment: this instance
		// still gets a working outbound Client/ChatStore above, it just
		// never tells Telegram to redirect the bot's webhook here — set
		// this way for a second, non-production instance sharing a real
		// deployment's token/PublicBaseURL, so it can't hijack the real
		// instance's webhook registration.
		logger.Info("telegram: register_webhook is false, skipping setWebhook", "url", webhookURL)
		return client, chats, secret, nil
	}
	if err := client.SetWebhook(context.Background(), webhookURL, secret); err != nil {
		logger.Error("telegram: failed to register webhook — the bot will not receive messages until this succeeds on a later restart",
			"error", err, "url", webhookURL)
	} else {
		logger.Info("telegram: webhook registered", "url", webhookURL)
	}

	return client, chats, secret, nil
}

// setupOAuth validates config and wires the optional OAuth2 authorization
// layer (internal/oauth2, docs/adr/oauth2-layer.md): it opens the encrypted
// token store, resolves the server-held master key and every configured
// provider's client id/secret from the environment, and builds the one
// oauth2.Service internal/httpapi and internal/mcp both call into. Returns
// a nil Store/Service when cfg.Enabled is false — callers gate all other
// OAuth2 wiring on that same flag rather than a nil check, mirroring
// setupTelegram's shape. The Store is returned separately (rather than only
// the Service) so run() can defer closing it, the same reason keyring.Open's
// result is kept in run() directly instead of behind a similar helper.
func setupOAuth(cfg config.OAuthConfig, storageCfg config.StorageConfig, logger *slog.Logger) (*oauth2.Store, *oauth2.Service, error) {
	if !cfg.Enabled {
		return nil, nil, nil
	}

	store, err := oauth2.Open(storageCfg.OAuthSQLitePath)
	if err != nil {
		return nil, nil, fmt.Errorf("main: open oauth store: %w", err)
	}

	masterKey, err := oauth2.LoadMasterKey(cfg.MasterKeyEnv)
	if err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("main: configure oauth: %w", err)
	}

	providers := make([]oauth2.Provider, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		clientSecret := ""
		if p.ClientSecretEnv != "" {
			clientSecret = os.Getenv(p.ClientSecretEnv)
		}
		providers = append(providers, oauth2.Provider{
			Name:                 p.Name,
			Description:          p.Description,
			AuthorizeURL:         p.AuthorizeURL,
			TokenURL:             p.TokenURL,
			ClientID:             os.Getenv(p.ClientIDEnv),
			ClientSecret:         clientSecret,
			Scopes:               p.Scopes,
			PKCE:                 p.PKCE,
			ExtraAuthorizeParams: p.ExtraAuthorizeParams,
		})
	}

	svc := oauth2.NewService(store, providers, masterKey, cfg.PublicBaseURL, cfg.CallbackPath, 0, logger)
	return store, svc, nil
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
func buildTTSDispatcher(cfg config.TTSConfig, storageCfg config.StorageConfig, h *hub.Hub, logger *slog.Logger) (*tts.Dispatcher, *tts.HTTPHandler, *ha.Client) {
	baseURL := os.Getenv("HA_BASE_URL")
	if baseURL == "" {
		logger.Warn("HA_BASE_URL not set — TTS dispatch is disabled")
		return nil, nil, nil
	}
	haClient := ha.New(baseURL, os.Getenv("HA_TOKEN"))

	primary, err := tts.NewProvider(cfg.Primary, cfg, storageCfg.TTSCacheDir, haClient, logger)
	if err != nil {
		logger.Error("tts: failed to build primary provider, TTS dispatch is disabled", "error", err, "primary", cfg.Primary)
		return nil, nil, nil
	}

	var fallback tts.Provider
	if cfg.Fallback != "" && cfg.Fallback != cfg.Primary {
		fallback, err = tts.NewProvider(cfg.Fallback, cfg, storageCfg.TTSCacheDir, haClient, logger)
		if err != nil {
			logger.Warn("tts: failed to build fallback provider, continuing without one", "error", err, "fallback", cfg.Fallback)
			fallback = nil
		}
	}

	dispatcher := tts.NewDispatcher(primary, fallback, haClient, cfg.DefaultDevice, h, logger)

	// Always registered (independent of which provider ended up being usable
	// above): a gemini_tts cache directory populated by an earlier, since-
	// reconfigured run should still be servable, and this is cheap to keep
	// around regardless.
	ttsHandler, err := tts.NewHTTPHandler(storageCfg.TTSCacheDir)
	if err != nil {
		logger.Error("tts: failed to create the /tts-audio HTTP handler", "error", err)
		return dispatcher, nil, haClient
	}
	return dispatcher, ttsHandler, haClient
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
