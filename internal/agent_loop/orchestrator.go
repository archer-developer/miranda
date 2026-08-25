package agentloop

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtrace"
	"github.com/archer-developer/miranda-llm/llmtrace/anomaly"
	"github.com/archer-developer/miranda-llm/router"
	"github.com/archer-developer/miranda/internal/attachments"
	"github.com/archer-developer/miranda/internal/calendar"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/keyring"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/memory"
	"github.com/archer-developer/miranda/internal/oauth2"
	"github.com/archer-developer/miranda/internal/redact"
	"github.com/archer-developer/miranda/internal/replyformat"
	"github.com/archer-developer/miranda/internal/schedule"
	"github.com/archer-developer/miranda/internal/telegram"
	"github.com/archer-developer/miranda/internal/tools"
	"github.com/archer-developer/miranda/internal/tts"
	"github.com/archer-developer/miranda/internal/users"
)

// googleCalendarProvider is the oauth.providers name internal/calendar's
// tools are gated on — matches config.yaml.dist's example and
// docs/adr/oauth2-layer.md, not a user-configurable setting: there's only
// one calendar integration today, so there's nothing to make generic yet
// (see internal/calendar's own package doc comment for why this bypasses
// Google's hosted Calendar MCP server entirely).
const googleCalendarProvider = "google_calendar"

// maxToolIterations bounds the agent loop (model calls a tool, gets a
// result, decides what to do next) so a misbehaving model can't loop forever
// racking up API calls.
const maxToolIterations = 15

const rememberToolName = "remember_this"

const searchHistoryToolName = "search_history"

// restoreConversationToolName names the tool that resumes a past
// conversation found via search_history (or the most recent one) as the new
// active session — see turnControl.restoreConversationID and
// Orchestrator.restoreConversation.
const restoreConversationToolName = "restore_conversation"

const endConversationToolName = "end_conversation"

const forgetConversationToolName = "forget_conversation"

const speakReplyToolName = "speak_reply"

const stopSpeechToolName = "stop_speech"

const sendTelegramToolName = "send_telegram"

const createScheduledTaskToolName = "create_scheduled_task"

const listScheduledTasksToolName = "list_scheduled_tasks"

const deleteScheduledTaskToolName = "delete_scheduled_task"

// loadToolGroupToolName is the shared stub tool that stands in for every
// not-yet-loaded lazy MCP server's real tools — see
// docs/adr/lazy-mcp-tool-loading.md.
const loadToolGroupToolName = "load_tool_group"

// oauthAuthorizeToolName is the generic (provider-agnostic) tool that starts
// a household member's OAuth2 authorization for a third-party MCP server —
// see docs/adr/oauth2-layer.md.
const oauthAuthorizeToolName = "oauth_authorize"

// ReservedToolNames returns every name Miranda's own agent loop can
// advertise as a tool: every hardcoded built-in above, plus internal/tools'
// fixed tavily_web_search/tavily_web_fetch names — regardless of whether
// config currently enables each one (a name is reserved the moment Miranda
// could ever advertise it, not only while it's actively turned on).
// cmd/miranda uses this at startup to reject a config where an LLMProvider's
// escalation.tool_name collides with one of these: router.deliver
// intercepts any model tool call matching esc.ToolName as an escalation
// trigger before it ever reaches Orchestrator.executeTool, so a collision
// would silently swallow every real call to that tool instead of running
// it, with nothing to explain why.
func ReservedToolNames() []string {
	names := []string{
		rememberToolName,
		searchHistoryToolName,
		restoreConversationToolName,
		endConversationToolName,
		forgetConversationToolName,
		speakReplyToolName,
		stopSpeechToolName,
		sendTelegramToolName,
		createScheduledTaskToolName,
		listScheduledTasksToolName,
		deleteScheduledTaskToolName,
		loadToolGroupToolName,
		oauthAuthorizeToolName,
		tools.WebSearchToolName,
		tools.WebFetchToolName,
	}
	return append(names, calendarToolNames()...)
}

// Attachment describes one file already uploaded via POST /api/upload and
// attached to this message. The file's bytes are looked up in the
// Orchestrator's attachments.Store by FileID; only the metadata is carried
// in the InputRequest itself to keep the JSON payload small.
type Attachment struct {
	FileID    string `json:"file_id"`
	Filename  string `json:"filename"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
}

// InputRequest is the body of POST /api/v1/input — the single entry point
// for both Home Assistant's thin conversation agent and manual curl/web UI
// commands, distinguished by Source.
//
// ConversationID is accepted for backward-compatible request shape but is
// never read: session continuity is owned by the server, keyed only on
// UserID (see resolveConversation), so the same user talking through HA,
// the web UI, or any future channel always continues the same conversation
// regardless of what conversation_id (if any) the caller sends.
type InputRequest struct {
	Source         string         `json:"source"`
	UserID         string         `json:"user_id"`
	Text           string         `json:"text"`
	ConversationID string         `json:"conversation_id,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	// Attachments lists files that were pre-uploaded via POST /api/upload
	// and should be included as context in this turn. Each entry's FileID
	// is resolved against the Orchestrator's in-memory attachments.Store;
	// entries whose TTL has expired are surfaced to the model as an
	// inline error notice rather than silently dropped.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// InputResponse is the synchronous reply to an InputRequest.
type InputResponse struct {
	ConversationID string `json:"conversation_id"`
	Reply          string `json:"reply"`
	ProviderUsed   string `json:"provider_used,omitempty"`
	// UserMessageID/AssistantMessageID are the history.Message.ID values
	// this turn just recorded — the caller (the web UI's own chat screen)
	// uses them to reconcile its optimistic render against the same
	// per-user WS chat stream (GET /ws/chat/{username}, see ChatEvent)
	// every other channel/tab learns about this turn from, so a message
	// this tab already rendered from the HTTP response is never rendered a
	// second time when its ChatEvent arrives.
	UserMessageID      int64 `json:"user_message_id"`
	AssistantMessageID int64 `json:"assistant_message_id"`
	// Downloads carries this turn's file references structurally, the same
	// shape history.Message.Downloads persists — the web UI's chat screen
	// renders a chip straight from this field rather than parsing anything
	// out of Reply. See history.Message.Downloads for why files are never
	// represented in Reply's own text.
	Downloads []history.DownloadRef `json:"downloads,omitempty"`
	// Blocks is the web UI's rendering of the assistant's own reply text
	// (before any voice-stripping/download-footnote transform applied to
	// Reply — see replyformat.Parse), computed on read rather than
	// persisted since it's 100% derivable from that text. Only ever set for
	// the assistant's turn; there is no separate "user's own message"
	// InputResponse.
	Blocks []replyformat.Block `json:"blocks,omitempty"`
}

// ChatEvent is published on internal/hub (Source: "chat", scoped by UserID)
// every time a conversation is mutated, so GET /ws/chat/{username} can keep
// every open tab/channel for that user in sync without polling — see
// CLAUDE.md's "Session ownership" for why this can happen on a channel a
// given tab never itself talked to (HA, Telegram, another tab).
type ChatEvent struct {
	// Type is "message" (a new history.Message was recorded — Message is
	// set), "conversation_deleted" (forget_conversation),
	// "conversation_ended" (end_conversation / the idle sweep),
	// "turn_started"/"turn_ended" (a Handle call began/finished for this
	// user — edge-triggered, published from Handle itself, see
	// TurnTracker), or "turn_in_progress" (a point-in-time snapshot sent
	// once when GET /ws/chat/{username} connects, see
	// internal/httpapi.handleWSChat).
	Type           string           `json:"type"`
	ConversationID string           `json:"conversation_id"`
	Message        *history.Message `json:"message,omitempty"`
	// Blocks mirrors InputResponse.Blocks — the web UI's chat screen renders
	// straight from this on every live WS event (including messages from a
	// channel this tab never itself talked to), the same way it renders
	// InputResponse.Blocks for the tab's own synchronous reply. Only set
	// when Message.Role == "assistant" — see publishChatMessage.
	Blocks []replyformat.Block `json:"blocks,omitempty"`
	// InProgress is set on "turn_in_progress" snapshots only (false is a
	// meaningful value there, so it's not omitempty) — see TurnTracker.
	InProgress bool `json:"in_progress,omitempty"`
	// StartedAt is set on "turn_started" and on "turn_in_progress" when
	// InProgress is true — lets the web UI render elapsed time on its
	// waiting bubble for long-running turns (tool calls, web search,
	// escalation) instead of a plain, ambiguous spinner. Zero/omitted on
	// "turn_ended" and on a not-in-progress snapshot.
	StartedAt time.Time `json:"started_at,omitempty"`
}

// speakerHA is the subset of *ha.Client the Orchestrator needs for per-entity
// speaker coordination before alice MCP tool calls: resolve a speaker's
// friendly name to its entity_id so WaitEntityIdle can poll the right entity.
type speakerHA interface {
	ResolveMediaPlayer(ctx context.Context, friendlyName string) (string, error)
}

// Orchestrator drives one full agent turn: it loads the user's memory and
// prior conversation, calls the LLM router (looping on tool calls), executes
// tools (either locally — remember_this — or via the MCP tool manager),
// dispatches assistant text to TTS as it streams, and persists the turn to
// history.
type Orchestrator struct {
	router  *router.Router
	tools   *mcp.Manager
	history *history.Store
	memory  *memory.Store
	tts     *tts.Dispatcher // may be nil: TTS dispatch is best-effort and optional
	hub     *hub.Hub
	// logger is set via SetLogger; nil means tool-call failures (e.g. an
	// MCP server rejecting a call) are only visible in logs/llm.log's full
	// request/response trace, not in the app log/journal or the web UI's
	// live Logs screen (this logger is wired to mirror into both — see
	// cmd/miranda.setupLogging).
	logger *slog.Logger
	// redactor is set via SetRedactor and used only to report how much a turn
	// masked — the actual masking is the stores' job. nil means no reporting.
	redactor         *redact.Redactor
	users            *users.Registry // may be nil: falls back to the raw user id in the system prompt
	memoryCfg        config.MemoryConfig
	ttsCfg           config.TTSConfig
	chunkMaxChars    int
	defaultUserID    string
	baseSystemPrompt string
	telegram         *telegram.Sender // set via SetTelegram; nil means the send_telegram tool is never offered
	telegramCfg      config.TelegramConfig
	// webTools is set via SetWebTools; empty means none offered. Kept as a
	// slice (not a map) so availableTools advertises them in the same order
	// on every turn — anthropic.Provider places its prompt-cache breakpoint
	// on the last tool in the list (see toAnthropicMessages), which only
	// caches if that list is byte-identical turn to turn; a map's
	// intentionally randomized iteration order would silently defeat that
	// cache on every single call.
	webTools []tools.Tool
	// schedule is set via SetSchedule; nil means the three scheduled-task
	// tools are never offered and RunScheduledTasks is a no-op.
	schedule *schedule.Store
	// attachStore is set via SetAttachmentStore; nil means Attachments in
	// InputRequest are ignored (file_upload.enabled is false). Also doubles
	// as the store for download records staged by the generic file-URI
	// detector (see fileExposingServers) so GET /api/files/{file_id} can
	// reject a household member fetching another member's file, the same
	// check processAttachments already does for uploaded files.
	attachStore *attachments.Store
	// mcpExtensions is set via SetMCPServerExtensions: every config-driven,
	// per-MCP-server argument-injection/detection behavior Orchestrator
	// applies around a tool call (see MCPServerExtension), keyed by that
	// server's unprefixed config.MCPServer.Name — matching what
	// mcp.Manager.ServerAndTool resolves a tool call's prefixed name to.
	// This used to be three independent maps (encryption-key allowlist,
	// session-id allowlist, file-exposing endpoints), each with its own
	// setter and config-load path; they're bundled into one map/one setter
	// now because they're all the same shape of thing — a server opting
	// into a specific behavior via static config — and executeTool only
	// ever needs one lookup by server name to learn everything it's
	// permitted to do for a given call, not three. Static config data,
	// deliberately kept here rather than on mcp.Manager — Manager's job is
	// connection lifecycle over live/reconnecting clients, and none of this
	// is that. A server absent from the map (or a zero-value entry) has
	// none of these behaviors applied.
	mcpExtensions map[string]MCPServerExtension
	// downloadRecordTTL is set via SetMCPServerExtensions from
	// config.FileUploadConfig.DownloadRecordTTLHours and stamped onto every
	// download record's Record.TTL (see executeTool) — longer than
	// attachStore's own short upload-oriented default TTL, since a download
	// marker is embedded durably in persisted conversation history and can be
	// revisited long after the turn that created it.
	downloadRecordTTL time.Duration
	// speakerHA is set via SetSpeakerHA; nil means pre-alice-tool speaker
	// coordination (WaitIdle + alice_state polling) is skipped entirely.
	speakerHA speakerHA
	// keyring is set via SetKeyring; nil means executeTool never injects an
	// encryption_key argument into any MCP tool call, regardless of
	// mcpExtensions above — see docs/encryption.md.
	keyring *keyring.Service
	// filesPublicBaseURL is set via SetFilesPublicBaseURL: the base URL
	// other backend services (sandbox, medical-card, ...) can reach
	// Miranda's own GET /files/{id} route through, e.g.
	// "http://192.168.1.50:8787" — mirrors config.TTSConfig's
	// gemini_tts.PublicBaseURL, which solves the identical problem (a LAN
	// device needs to fetch a Miranda-hosted resource by URL) for
	// synthesized audio. processAttachments uses this to build the fileURI
	// handed to the model for each attachment — see
	// docs/file-staging-refactor.md. Empty means no URI is ever included
	// (file_upload.enabled is false, or public_base_url isn't configured).
	filesPublicBaseURL string
	// memoryMu guards memoryCache.
	memoryMu sync.Mutex
	// memoryCache holds each open conversation's shared+personal memory
	// snapshot, read once and reused for the rest of that conversation —
	// see conversationMemory/clearConversationMemory
	// (docs/adr/system-prompt-caching.md).
	memoryCache map[string]cachedMemory
	// lazyServerDescriptions is set via SetLazyMCPServers: every MCP server
	// (config.MCPServer.Lazy) whose own real tool schemas are hidden behind
	// the shared load_tool_group stub until the model calls it for that
	// server's Name, keyed by that unprefixed Name to its one-line
	// model-facing Description. nil/empty (the default) means every
	// configured MCP server's tools are included directly on every turn,
	// exactly as before this feature existed. See
	// docs/adr/lazy-mcp-tool-loading.md.
	lazyServerDescriptions map[string]string
	// oauth is set via SetOAuth; nil means no MCP server can be OAuth-gated —
	// the oauth_authorize tool is never offered and executeTool's
	// OAuthProvider branch (see MCPServerExtension) is always skipped. See
	// docs/adr/oauth2-layer.md.
	oauth *oauth2.Service
	// oauthReconnect/oauthMaxReconnect/oauthConnectTimeout tune
	// EnsureUserSession's per-user background reconnect loop — set together
	// with oauth via SetOAuth, mirroring the same three knobs cmd/miranda
	// already uses for the global MCP KeepConnected loops.
	oauthReconnect, oauthMaxReconnect, oauthConnectTimeout time.Duration
	// calendar is internal/calendar's stateless REST client — always
	// non-nil (holds no credentials, costs nothing to construct), unlike
	// every other optional dependency on this struct. Its tools are only
	// advertised/dispatched when calendarEnabled() is true (oauth is wired
	// AND googleCalendarProvider is one of its configured providers) — see
	// availableTools/executeTool's calendar_* branches.
	calendar *calendar.Client
	// turns tracks, per user, how many Handle calls are currently in
	// flight and when the oldest of them started — see TurnTracker,
	// TurnStatus, and the turn_started/turn_ended events publishTurnStatus
	// broadcasts around every Handle call. Always non-nil (constructed in
	// NewOrchestrator, holds no external dependencies).
	turns *TurnTracker
	// anomaly is set via SetAnomalyConfig: per-turn anomaly detection (see
	// anomaly.go). Zero value (the default) disables it entirely — Handle
	// never attaches a Recorder to a turn's ctx.
	anomaly AnomalyConfig
}

// TurnStatus reports whether userID currently has a Handle turn in flight
// (on any channel — HA, web, Telegram, scheduled) and, if so, when the
// oldest still-running one started. Used by GET /ws/chat/{username}'s
// connect-time snapshot (internal/httpapi.handleWSChat) and by
// internal/webui's GET /api/turn-status polling fallback.
func (o *Orchestrator) TurnStatus(userID string) (inProgress bool, startedAt time.Time) {
	return o.turns.Status(userID)
}

// calendarEnabled reports whether the calendar_* tools should be offered:
// the OAuth2 layer must be wired in at all (o.oauth != nil, see SetOAuth)
// AND googleCalendarProvider must be one of its configured providers —
// otherwise oauth_authorize would offer a provider name calendar tools then
// can't actually get a token for.
func (o *Orchestrator) calendarEnabled() bool {
	return o.oauth != nil && slices.Contains(o.oauth.ProviderNames(), googleCalendarProvider)
}

// SetOAuth wires in the optional OAuth2 authorization layer (see
// internal/oauth2, docs/adr/oauth2-layer.md), mirroring SetKeyring/
// SetSchedule's post-construction style — call once from cmd/miranda, only
// when config.OAuthConfig.Enabled. Leaving it uncalled (the default) means
// the oauth_authorize tool is never offered and no MCP server can be
// OAuth-gated, regardless of what MCPServerExtension.OAuthProvider says.
func (o *Orchestrator) SetOAuth(svc *oauth2.Service, reconnectInterval, maxReconnectInterval, connectTimeout time.Duration) {
	o.oauth = svc
	o.oauthReconnect = reconnectInterval
	o.oauthMaxReconnect = maxReconnectInterval
	o.oauthConnectTimeout = connectTimeout
}

// SetTelegram wires the optional send_telegram tool in, mirroring
// router.SetTracer's post-construction style for an optional dependency —
// call it once from cmd/miranda after building a telegram.Sender, only
// when config.TelegramConfig.Enabled. Leaving it uncalled (the default)
// means availableTools never offers send_telegram at all.
func (o *Orchestrator) SetTelegram(sender *telegram.Sender, cfg config.TelegramConfig) {
	o.telegram = sender
	o.telegramCfg = cfg
}

// SetWebTools wires in Miranda's own tavily_web_search/tavily_web_fetch tools
// (see internal/tools), mirroring SetTelegram's post-construction style for an
// optional dependency — call it once from cmd/miranda after building
// whichever tools config.TavilyConfig enables. Leaving it uncalled (the
// default, a nil slice) means availableTools never offers either tool.
func (o *Orchestrator) SetWebTools(ts []tools.Tool) {
	o.webTools = ts
}

// SetSchedule wires the optional create_scheduled_task/list_scheduled_tasks/
// delete_scheduled_task tools in, mirroring SetTelegram/SetWebTools's
// post-construction style for an optional dependency — call it once from
// cmd/miranda after opening a schedule.Store, only when
// config.ScheduleConfig.Enabled. Leaving it uncalled (the default) means
// availableTools never offers any of the three tools, and
// RunScheduledTasks is a no-op.
func (o *Orchestrator) SetSchedule(s *schedule.Store) {
	o.schedule = s
}

// SetSpeakerHA wires the optional speaker coordinator in — call once from
// cmd/miranda with the same *ha.Client used for TTS, when HA is configured.
// Without it, executeTool skips per-entity pre-call coordination and alice
// tool calls fire immediately regardless of what the station is playing.
func (o *Orchestrator) SetSpeakerHA(ha speakerHA) {
	o.speakerHA = ha
}

// SetAttachmentStore wires the optional in-memory file-attachment cache in,
// mirroring SetTelegram/SetWebTools's post-construction style — call it once
// from cmd/miranda after creating an attachments.Store, only when
// config.FileUploadConfig.Enabled. Leaving it uncalled (the default) means
// any Attachments field in an InputRequest is ignored silently.
func (o *Orchestrator) SetAttachmentStore(s *attachments.Store) {
	o.attachStore = s
}

// AttachStore returns the attachment store wired in via SetAttachmentStore,
// or nil if file_upload is disabled — used by httpapi's upload/download
// handlers, which live in a separate package from Orchestrator and so need
// an exported accessor rather than direct field access.
func (o *Orchestrator) AttachStore() *attachments.Store {
	return o.attachStore
}

// Hub returns the event hub passed to NewOrchestrator — used by httpapi's
// tests (and NewServer call sites generally) to share the same hub between
// Server and Orchestrator without Server needing direct field access across
// the package boundary.
func (o *Orchestrator) Hub() *hub.Hub {
	return o.hub
}

// SetKeyring wires the per-user data-encryption keyring in, mirroring
// SetTelegram/SetSchedule's post-construction style — cmd/miranda always
// calls this once at startup, since the keyring has no config toggle (see
// internal/keyring). Leaving it uncalled — only ever true in tests that
// don't need it — means executeTool never injects an encryption_key
// argument into any MCP tool call, even for a whitelisted, https-only
// server — see docs/encryption.md.
func (o *Orchestrator) SetKeyring(k *keyring.Service) {
	o.keyring = k
}

// SetLazyMCPServers wires in the config-derived one-line descriptions for
// MCP servers whose real tool schemas should only reach the model on demand
// (see config.MCPServer.Lazy/Description and config.Config.LazyMCPServers),
// keyed by each such server's own unprefixed Name — mirrors SetSchedule's
// post-construction style for an optional dependency. Leaving it uncalled
// (the default, a nil map) makes availableTools/composeTools behave exactly
// like before this feature existed: every enabled MCP server's tools
// included on every turn, with no load_tool_group stub ever offered.
func (o *Orchestrator) SetLazyMCPServers(descriptions map[string]string) {
	o.lazyServerDescriptions = descriptions
}

// SetLogger wires in the app's slog logger — mirrors SetSchedule's
// post-construction style for an optional dependency. Leaving it uncalled
// (the default, nil) just means executeTool's tool-call-failure logging is
// skipped, same nil-safe posture as every other optional Set*.
func (o *Orchestrator) SetLogger(logger *slog.Logger) {
	o.logger = logger
}

// SetRedactor wires in the redactor for observability only. The stores do
// their own masking (see history.Store.SetRedactor) — this copy exists purely
// so a turn can report *how much* it redacted, which is otherwise invisible:
// the masking happens deep inside a store, and over-masking is the failure
// mode worth being able to notice. Optional, nil-safe, same posture as every
// other Set*.
func (o *Orchestrator) SetRedactor(r *redact.Redactor) {
	o.redactor = r
}

// logRedactions reports how many values this turn's user text would have
// masked, and under which rules — never the values themselves, which is the
// entire reason redact.Finding carries offsets instead of text.
//
// Gated on the logger actually being at debug level, so the extra scan costs
// nothing in normal operation.
func (o *Orchestrator) logRedactions(ctx context.Context, convID, text string) {
	if o.redactor == nil || o.logger == nil || !o.logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	_, findings := o.redactor.RedactWithFindings(text)
	if len(findings) == 0 {
		return
	}
	rules := make([]string, 0, len(findings))
	for _, f := range findings {
		rules = append(rules, f.Rule)
	}
	o.logger.Debug("orchestrator: redacted sensitive values before storing turn",
		"conversationId", convID, "count", len(findings), "rules", strings.Join(rules, ","))
}

// MCPServerExtension bundles the config-driven, per-MCP-server behaviors
// executeTool applies around a tool call routed to that server — replacing
// what used to be three separate maps (encryption-key allowlist, session-id
// allowlist, file-exposing endpoints), each independently keyed by the same
// server name. A zero-value MCPServerExtension (the value SetMCPServerExtensions
// implicitly gives any server absent from its argument) grants none of these.
type MCPServerExtension struct {
	// EncryptionKeyArg is the tool-call argument name executeTool injects a
	// user's unwrapped master key under — "" means this server may never
	// receive one, regardless of whether the keyring itself is configured
	// (see config.MCPServer.EncryptionKeyPermitted). See docs/encryption.md.
	EncryptionKeyArg string
	// SessionIDArg/SessionIDTools mirror config.MCPServer.SessionIDArg()/
	// SessionIDTools: SessionIDArg is "" when no tool on this server should
	// ever receive Miranda's own resolved conversation id; otherwise only
	// the (unprefixed) tool names present as true keys in SessionIDTools do
	// — permission here is scoped per-tool, not per-server, unlike
	// EncryptionKeyArg, since most tools on a server won't declare a
	// session-id parameter in their own schema at all. See
	// docs/medical-card-session-injection.md.
	SessionIDArg   string
	SessionIDTools map[string]bool
	// FilesEndpoint is non-nil when this server opted into the file-URI
	// download proxy (config.MCPServer.ExposeFiles) — executeTool scans a
	// matching server's tool-call results for a URL rooted at
	// FilesEndpoint.FilesURL and proxies it, detected by URL shape rather
	// than by any specific tool name, which is what lets every
	// file-serving MCP server (the sandbox's download_file included) share
	// this one mechanism.
	FilesEndpoint *config.FileServerEndpoint
	// OAuthProvider names the oauth2.Provider (config.OAuthConfig.Providers
	// entry) this server's tool calls must be authorized against, per
	// household member independently — "" (the default) means this server
	// is unaffected by the OAuth2 layer, and its existing static TokenEnv
	// bearer token (or no auth at all) behaves exactly as before. See
	// config.MCPServer.OAuthProvider, docs/adr/oauth2-layer.md.
	OAuthProvider string
	// MCPServerURL duplicates config.MCPServer.URL for an OAuth-gated server
	// only, so executeTool can build a fresh per-user mcp.Connect call
	// without Orchestrator holding the full config.MCPServer slice just for
	// this one field — meaningless (and unused) for any non-OAuth-gated
	// server, whose single global connection mcp.Manager already owns from
	// cmd/miranda's connectMCP.
	MCPServerURL string
}

// SetMCPServerExtensions wires in the static, config-derived set of
// per-server behaviors above (encryption-key injection, session-id
// injection, file-URI download detection), keyed by each server's
// unprefixed config.MCPServer.Name, plus recordTTL from
// config.FileUploadConfig.DownloadRecordTTLHours for any staged download
// record. Call it once from cmd/miranda after resolving every server's
// config opt-ins (mirrors SetAttachmentStore's post-construction style).
// Leaving it uncalled (the default, a nil map) means no server has any of
// these behaviors applied.
func (o *Orchestrator) SetMCPServerExtensions(extensions map[string]MCPServerExtension, recordTTL time.Duration) {
	o.mcpExtensions = extensions
	o.downloadRecordTTL = recordTTL
}

// SetFilesPublicBaseURL wires in the base URL other backend services can
// reach Miranda's own GET /files/{id} route through (see
// config.FileUploadConfig.PublicBaseURL), mirroring SetAttachmentStore's
// post-construction style — call once from cmd/miranda, only when
// config.FileUploadConfig.Enabled. Leaving it uncalled (the default, "")
// means processAttachments never includes a fileURI for any attachment.
func (o *Orchestrator) SetFilesPublicBaseURL(url string) {
	o.filesPublicBaseURL = url
}

// NewOrchestrator wires the agent loop's dependencies together.
func NewOrchestrator(
	r *router.Router,
	toolManager *mcp.Manager,
	historyStore *history.Store,
	memoryStore *memory.Store,
	ttsDispatcher *tts.Dispatcher,
	h *hub.Hub,
	usersRegistry *users.Registry,
	agentCfg config.AgentConfig,
	memoryCfg config.MemoryConfig,
	ttsCfg config.TTSConfig,
	chunkMaxChars int,
	defaultUserID string,
) *Orchestrator {
	return &Orchestrator{
		router:           r,
		tools:            toolManager,
		history:          historyStore,
		memory:           memoryStore,
		tts:              ttsDispatcher,
		hub:              h,
		users:            usersRegistry,
		memoryCfg:        memoryCfg,
		ttsCfg:           ttsCfg,
		chunkMaxChars:    chunkMaxChars,
		defaultUserID:    defaultUserID,
		baseSystemPrompt: agentCfg.SystemPrompt,
		memoryCache:      make(map[string]cachedMemory),
		calendar:         calendar.New(),
		turns:            NewTurnTracker(),
	}
}

// Handle runs one full turn and returns the assistant's reply.
func (o *Orchestrator) Handle(ctx context.Context, req InputRequest) (InputResponse, error) {
	if req.Source == users.SourceHAAssist && o.tts != nil {
		// Barge-in: interrupt whatever the station is still finishing from a
		// previous turn *before* this turn's own speech has any chance to
		// enqueue anything, so a new voice turn always cuts in rather than
		// queuing after what's already playing.
		o.tts.Stop()
	}

	userID := req.UserID
	if userID == "" {
		userID = o.defaultUserID
	}

	// Bracket the whole turn so TurnStatus/the web UI can tell "Miranda is
	// still working on something for this user" apart from a client-side
	// guess (see TurnTracker, publishTurnStatus, and the fixed-timeout hack
	// this replaced in chat.js). end() is deferred immediately so every
	// exit path below — success, every early error return, and the
	// TurnTimeout deadline expiring inside runAgentLoop — clears it.
	// Registered before the turn_ended publish defer so LIFO clears the
	// tracker first (a client reacting to turn_ended never races ahead of
	// TurnStatus reflecting it).
	end := o.turns.Begin(userID)
	defer end()
	_, startedAt := o.turns.Status(userID)
	o.publishTurnStatus(userID, ChatEvent{Type: "turn_started", StartedAt: startedAt})
	defer func() { o.publishTurnStatus(userID, ChatEvent{Type: "turn_ended"}) }()

	convID, priorMessages, err := o.resolveConversation(ctx, userID, req.Source)
	if err != nil {
		return InputResponse{}, err
	}
	// Tags every LLM call this turn makes (via the router's trace log) with
	// which conversation it belongs to, so logs/llm.log can be correlated
	// with a specific dialog.
	ctx = llmtrace.WithConversationID(ctx, convID)

	// A Recorder scoped to just this turn, teed onto whatever the
	// process-wide tracer already does (see llmtrace.ContextTracer) — only
	// attached when anomaly detection is enabled (see SetAnomalyConfig).
	// outcome is filled in by runAgentLoop as the turn ends (normal reply,
	// error/timeout, or the iteration cap) and read back here, after the
	// turn is fully done, by the deferred reportAnomalies call.
	var recorder *anomaly.Recorder
	outcome := &anomaly.Outcome{MaxIterations: maxToolIterations}
	if o.anomaly.enabled() {
		recorder = anomaly.NewRecorder(convID)
		ctx = llmtrace.WithTracer(ctx, recorder)
		defer func() { o.reportAnomalies(convID, recorder, *outcome) }()
	}

	// Read once per conversation, not once per turn — see
	// conversationMemory's doc comment and docs/adr/system-prompt-caching.md
	// for why re-reading on every turn would both hit disk needlessly and
	// defeat the stable system-prompt block's cacheability below.
	sharedMem, memContent, err := o.conversationMemory(userID, convID)
	if err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: read memory: %w", err)
	}

	// Build the enriched user content: req.Text plus any inline file context
	// (text file contents) and placeholder annotations (images, binary, PDF).
	// imageParts carries base64 image blocks for vision models and is only
	// used in the current turn's LLM message — not in history, since
	// future replays don't re-send the image bytes.
	userContent, imageParts := o.processAttachments(userID, req.Text, req.Attachments)

	o.logRedactions(ctx, convID, userContent)

	userMsgID, err := o.history.AppendMessage(ctx, convID, "user", userContent)
	if err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: record user message: %w", err)
	}
	o.publishChatMessage(userID, convID, history.Message{ID: userMsgID, ConversationID: convID, Role: "user", Content: userContent})

	stableSystem, volatileSystem := o.buildSystemPrompt(userID, sharedMem, memContent)
	if err := o.history.SetSystemPrompt(ctx, convID, stableSystem+"\n\n"+volatileSystem); err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: set system prompt: %w", err)
	}

	// Sent as two separate RoleSystem messages, stable first — see
	// buildSystemPrompt's doc comment and docs/adr/system-prompt-caching.md:
	// anthropic.Provider places its prompt-cache breakpoint on the FIRST
	// system message specifically so the volatile one (current time,
	// different on every turn) never defeats reuse of the stable one.
	messages := append([]llm.Message{
		{Role: llm.RoleSystem, Content: stableSystem},
		{Role: llm.RoleSystem, Content: volatileSystem},
	}, priorMessages...)
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: userContent, Parts: imageParts})

	control := &turnControl{}
	tools := o.availableTools(ctx, userID, control)
	finalText, providerUsed, err := o.runAgentLoop(ctx, userID, convID, req.Source, messages, tools, control, outcome)
	if err != nil && req.Source == users.SourceScheduled {
		// RunScheduledTasks needs the real error, not a graceful cover-up:
		// it's what tells the store the fire failed, keeps the task due for
		// the next sweep instead of silently marking it done, and records
		// schedule.StatusError in the task's history. Nobody is watching a
		// scheduled turn live for a reply the way a chat user is (see
		// CLAUDE.md: every non-ha_assist source stays silent unless the
		// model itself calls speak_reply/send_telegram) — swallowing the
		// error into an unseen fallback reply here would just make a failed
		// reminder look like it fired.
		if len(control.downloadedFiles) > 0 {
			if _, recErr := o.history.AppendAssistantMessage(ctx, convID, "", nil, toDownloadRefs(control.downloadedFiles)); recErr != nil {
				o.hub.Publish(hub.Event{Source: "error", Message: "record recovered downloads: " + recErr.Error()})
			}
		}
		return InputResponse{}, err
	}
	if err != nil {
		// A provider outage, a mid-stream error, or the turn's own
		// deadline expiring (e.g. after several slow tool round-trips ate
		// the whole TurnTimeout budget, leaving escalation no time to even
		// attempt a call — see logs/anomalies/'s "timeout" kind) used to
		// propagate all the way to httpapi as a bare HTTP 500, leaving the
		// user with total silence: no chat bubble, no TTS, nothing to react
		// to after however long the turn had been working. Anomaly
		// detection (reportAnomalies, deferred above) already records the
		// real failure for later review regardless of what Handle returns,
		// so swallowing it into an ordinary-looking reply here loses no
		// operator visibility — it only fixes what the user experiences.
		//
		// An earlier tool-call iteration in this same loop may have already
		// called download_file successfully (attachStore.Put and
		// control.downloadedFiles both happen synchronously at call time in
		// executeTool) before a *later* iteration errored out — that file
		// stays staged and gets recorded below anyway, alongside the
		// fallback reply text, via the same toDownloadRefs(control.
		// downloadedFiles) the success path already uses; nothing extra to
		// do here for it.
		if o.logger != nil {
			o.logger.Error("orchestrator: agent loop failed, replying with fallback message", "error", err, "conversationId", convID, "userId", userID)
		}
		o.hub.Publish(hub.Event{Source: "error", Message: err.Error()})
		finalText = o.turnFailureReply(userID)
		providerUsed = ""
		if req.Source == users.SourceHAAssist {
			// The normal live-TTS path (streamOneTurn's speakChunks) never
			// got a chance to speak anything for this failed final
			// call — say the fallback text out loud too, not just show it.
			o.speakText(ctx, voiceText(finalText))
		}
	}
	// downloads is recorded structurally alongside finalText, never folded
	// into it — see history.Message.Downloads/toDownloadRefs for why. This
	// is what's always persisted to history regardless of req.Source, so
	// the web UI's history browser can render a chip for a conversation
	// that originated on any channel; only the outbound reply below needs
	// a plain-text fallback for a non-web channel.
	downloads := toDownloadRefs(control.downloadedFiles)

	assistantMsgID, err := o.history.AppendAssistantMessage(ctx, convID, finalText, nil, downloads)
	if err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: record assistant message: %w", err)
	}
	o.publishChatMessage(userID, convID, history.Message{ID: assistantMsgID, ConversationID: convID, Role: "assistant", Content: finalText, Downloads: downloads})

	// Applied after the reply is recorded (and, via TTS, already spoken) so
	// an end/forget/restore request never costs the user their answer to
	// this turn. All three are best-effort: a failure here shouldn't fail a
	// turn the user already got a correct reply to, just leave the
	// conversation open for the next sweep/turn to retry against.
	switch {
	case control.forgetRequested:
		if err := o.history.DeleteConversation(ctx, convID); err != nil {
			o.hub.Publish(hub.Event{Source: "error", Message: "forget conversation: " + err.Error()})
		} else {
			o.clearConversationMemory(convID)
			o.hub.Publish(hub.Event{Source: "chat", UserID: userID, Data: ChatEvent{Type: "conversation_deleted", ConversationID: convID}})
		}
	case control.restoreConversationID != "":
		o.restoreConversation(ctx, convID, userID, req.Source, control.restoreConversationID)
	case control.endRequested:
		if err := o.summarizeConversation(ctx, convID, userID); err != nil {
			o.hub.Publish(hub.Event{Source: "error", Message: "end conversation: " + err.Error()})
		}
	}

	// A voice-classified source's HTTP reply is treated as equivalent to
	// what gets spoken: same transformer as speakChunks/speak_reply, no
	// footnote (voice doesn't need attachments — see appendDownloadFootnotes).
	// Every other source gets the plain-text download footnote fallback the
	// web UI doesn't need (it renders a chip from Downloads instead).
	var reply string
	switch req.Source {
	case users.SourceHAAssist, users.SourceScheduled:
		reply = voiceText(finalText)
	default:
		reply = appendDownloadFootnotes(finalText, control.downloadedFiles, req.Source)
	}

	return InputResponse{
		ConversationID:     convID,
		Reply:              reply,
		ProviderUsed:       providerUsed,
		UserMessageID:      userMsgID,
		AssistantMessageID: assistantMsgID,
		Downloads:          downloads,
		Blocks:             replyformat.Parse(finalText),
	}, nil
}

// publishChatMessage broadcasts one recorded history.Message over
// /ws/logs, tagged Source: "chat" and scoped to userID so
// handleWSChat can forward it only to that user's GET /ws/chat/{username}
// connections — see ChatEvent. msg.CreatedAt is set to now rather than
// re-reading the row back from history: this turn just wrote it, so "now"
// is accurate enough for a live UI update (the authoritative timestamp is
// still whatever AppendMessage persisted).
func (o *Orchestrator) publishChatMessage(userID, convID string, msg history.Message) {
	msg.CreatedAt = time.Now()
	var blocks []replyformat.Block
	if msg.Role == "assistant" {
		blocks = replyformat.Parse(msg.Content)
	}
	o.hub.Publish(hub.Event{Source: "chat", UserID: userID, Data: ChatEvent{Type: "message", ConversationID: convID, Message: &msg, Blocks: blocks}})
}

// publishTurnStatus broadcasts a turn_started/turn_ended ChatEvent for
// userID — carries no history.Message, unlike publishChatMessage: it exists
// purely so a WS-connected tab (or one about to fall back to REST polling
// GET /api/turn-status) learns a turn is/was in flight without waiting on
// the eventual reply itself. See TurnTracker and Handle's use of this.
func (o *Orchestrator) publishTurnStatus(userID string, ev ChatEvent) {
	o.hub.Publish(hub.Event{Source: "chat", UserID: userID, Data: ev})
}
