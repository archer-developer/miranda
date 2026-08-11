package httpapi

import (
	"context"
	"fmt"
	"time"

	"github.com/archer-developer/miranda/internal/attachments"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/keyring"
	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/llm/router"
	"github.com/archer-developer/miranda/internal/llmtrace"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/memory"
	"github.com/archer-developer/miranda/internal/schedule"
	"github.com/archer-developer/miranda/internal/telegram"
	"github.com/archer-developer/miranda/internal/tools"
	"github.com/archer-developer/miranda/internal/tts"
	"github.com/archer-developer/miranda/internal/users"
)

// maxToolIterations bounds the agent loop (model calls a tool, gets a
// result, decides what to do next) so a misbehaving model can't loop forever
// racking up API calls.
const maxToolIterations = 15

const rememberToolName = "remember_this"

const searchHistoryToolName = "search_history"

const endConversationToolName = "end_conversation"

const forgetConversationToolName = "forget_conversation"

const speakReplyToolName = "speak_reply"

const stopSpeechToolName = "stop_speech"

const sendTelegramToolName = "send_telegram"

const createScheduledTaskToolName = "create_scheduled_task"

const listScheduledTasksToolName = "list_scheduled_tasks"

const deleteScheduledTaskToolName = "delete_scheduled_task"

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
	return []string{
		rememberToolName,
		searchHistoryToolName,
		endConversationToolName,
		forgetConversationToolName,
		speakReplyToolName,
		stopSpeechToolName,
		sendTelegramToolName,
		createScheduledTaskToolName,
		listScheduledTasksToolName,
		deleteScheduledTaskToolName,
		tools.WebSearchToolName,
		tools.WebFetchToolName,
	}
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
}

// ChatEvent is published on internal/hub (Source: "chat", scoped by UserID)
// every time a conversation is mutated, so GET /ws/chat/{username} can keep
// every open tab/channel for that user in sync without polling — see
// CLAUDE.md's "Session ownership" for why this can happen on a channel a
// given tab never itself talked to (HA, Telegram, another tab).
type ChatEvent struct {
	// Type is "message" (a new history.Message was recorded — Message is
	// set), "conversation_deleted" (forget_conversation), or
	// "conversation_ended" (end_conversation / the idle sweep).
	Type           string           `json:"type"`
	ConversationID string           `json:"conversation_id"`
	Message        *history.Message `json:"message,omitempty"`
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
	router           *router.Router
	tools            *mcp.Manager
	history          *history.Store
	memory           *memory.Store
	tts              *tts.Dispatcher // may be nil: TTS dispatch is best-effort and optional
	hub              *hub.Hub
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
	// fileExposingServers is set via SetFileExposingServers: which MCP
	// servers (keyed by their unprefixed config.MCPServer.Name, matching
	// what mcp.Manager.ServerForTool resolves a tool call to) have opted
	// into the file-URI download proxy (config.MCPServer.ExposeFiles), and
	// the FilesEndpoint each one's own file links are expected to be rooted
	// at. executeTool scans a matching server's tool-call results for a URL
	// under that prefix and proxies it — detected by URL shape, not by any
	// specific tool name, which is what lets every file-serving MCP server
	// (the sandbox's download_file included — it has no dedicated path of
	// its own) share this one mechanism. A server absent from this map
	// never has its tool results scanned at all — see
	// config.MCPServer.ExposeFiles's doc comment for why this is opt-in per
	// server.
	fileExposingServers map[string]config.FileServerEndpoint
	// downloadRecordTTL is set via SetFileExposingServers from
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
	// encryptionKeyAllowed below — see docs/encryption.md.
	keyring *keyring.Service
	// encryptionKeyAllowed is set via SetEncryptionKeyAllowedServers: which
	// MCP server names (by their unprefixed config.MCPServer.Name) may
	// receive a user's unwrapped master key as a tool-call argument, and
	// under which tool-call argument name each one expects it (that
	// server's config.MCPServer.EncryptionKeyArg()). Static config data
	// (config.MCPServer.EncryptionKeyPermitted, computed once at startup),
	// deliberately kept here rather than on mcp.Manager — Manager's job is
	// connection lifecycle over live/reconnecting clients, and this
	// permission bit is neither, so executeTool reads it directly from the
	// Orchestrator instead of asking the connection manager to remember a
	// static config fact. A server absent from the map is not permitted.
	encryptionKeyAllowed map[string]string
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

// SetFileExposingServers wires in the set of MCP servers that opted into
// the file-URI download proxy (config.MCPServer.ExposeFiles), via
// config.Config.FileExposingServers, and recordTTL from
// config.FileUploadConfig.DownloadRecordTTLHours — call it once from
// cmd/miranda, mirroring SetAttachmentStore's post-construction style, only
// when config.FileUploadConfig.Enabled (the same gate that creates
// o.attachStore). Leaving it uncalled (the default, a nil map) means
// executeTool never scans any MCP tool result for an embedded file URI —
// every server needs an explicit, present entry to be trusted this way,
// including the sandbox, which has no dedicated path of its own here.
func (o *Orchestrator) SetFileExposingServers(servers map[string]config.FileServerEndpoint, recordTTL time.Duration) {
	o.fileExposingServers = servers
	o.downloadRecordTTL = recordTTL
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

// SetEncryptionKeyAllowedServers wires in the static, config-derived set of
// MCP server names permitted to receive a user's unwrapped master key,
// mapped to the tool-call argument name each one expects it under — call it
// once from cmd/miranda with a map built from every config.MCPServer whose
// EncryptionKeyPermitted() is true, keyed to that server's EncryptionKeyArg().
// Leaving it uncalled (the default, a nil map) means every server reads as
// not-allowed.
func (o *Orchestrator) SetEncryptionKeyAllowedServers(allowed map[string]string) {
	o.encryptionKeyAllowed = allowed
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

	convID, priorMessages, err := o.resolveConversation(ctx, userID, req.Source)
	if err != nil {
		return InputResponse{}, err
	}
	// Tags every LLM call this turn makes (via the router's trace log) with
	// which conversation it belongs to, so logs/llm.log can be correlated
	// with a specific dialog.
	ctx = llmtrace.WithConversationID(ctx, convID)

	sharedMem, err := o.memory.ReadShared()
	if err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: read shared memory: %w", err)
	}
	memContent, err := o.memory.Read(userID)
	if err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: read memory: %w", err)
	}

	// Build the enriched user content: req.Text plus any inline file context
	// (text file contents) and placeholder annotations (images, binary, PDF).
	// imageParts carries base64 image blocks for vision models and is only
	// used in the current turn's LLM message — not in history, since
	// future replays don't re-send the image bytes.
	userContent, imageParts := o.processAttachments(userID, req.Text, req.Attachments)

	userMsgID, err := o.history.AppendMessage(ctx, convID, "user", userContent)
	if err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: record user message: %w", err)
	}
	o.publishChatMessage(userID, convID, history.Message{ID: userMsgID, ConversationID: convID, Role: "user", Content: userContent})

	systemPrompt := o.buildSystemPrompt(userID, sharedMem, memContent)
	if err := o.history.SetSystemPrompt(ctx, convID, systemPrompt); err != nil {
		return InputResponse{}, fmt.Errorf("orchestrator: set system prompt: %w", err)
	}

	messages := append([]llm.Message{{Role: llm.RoleSystem, Content: systemPrompt}}, priorMessages...)
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: userContent, Parts: imageParts})

	tools := o.availableTools(ctx)

	control := &turnControl{}
	finalText, providerUsed, err := o.runAgentLoop(ctx, userID, convID, req.Source, messages, tools, control)
	if err != nil {
		// An earlier tool-call iteration in this same loop may have already
		// called download_file successfully (attachStore.Put and
		// control.downloadedFiles both happen synchronously at call time in
		// executeTool) before a *later* iteration errored out. Without this,
		// that file would be staged and ownership-recorded with nowhere the
		// user could discover it through normal use — see the success path
		// below this mirrors. Best-effort: the turn still reports its
		// original error.
		if len(control.downloadedFiles) > 0 {
			if _, recErr := o.history.AppendAssistantMessage(ctx, convID, "", nil, toDownloadRefs(control.downloadedFiles)); recErr != nil {
				o.hub.Publish(hub.Event{Source: "error", Message: "record recovered downloads: " + recErr.Error()})
			}
		}
		return InputResponse{}, err
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
	// an end/forget request never costs the user their answer to this turn.
	// Both are best-effort: a failure here shouldn't fail a turn the user
	// already got a correct reply to, just leave the conversation open for
	// the next sweep/turn to retry against.
	switch {
	case control.forgetRequested:
		if err := o.history.DeleteConversation(ctx, convID); err != nil {
			o.hub.Publish(hub.Event{Source: "error", Message: "forget conversation: " + err.Error()})
		} else {
			o.hub.Publish(hub.Event{Source: "chat", UserID: userID, Data: ChatEvent{Type: "conversation_deleted", ConversationID: convID}})
		}
	case control.endRequested:
		if err := o.summarizeConversation(ctx, convID, userID); err != nil {
			o.hub.Publish(hub.Event{Source: "error", Message: "end conversation: " + err.Error()})
		}
	}

	return InputResponse{
		ConversationID:     convID,
		Reply:              appendDownloadFootnotes(finalText, control.downloadedFiles, req.Source),
		ProviderUsed:       providerUsed,
		UserMessageID:      userMsgID,
		AssistantMessageID: assistantMsgID,
		Downloads:          downloads,
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
	o.hub.Publish(hub.Event{Source: "chat", UserID: userID, Data: ChatEvent{Type: "message", ConversationID: convID, Message: &msg}})
}
