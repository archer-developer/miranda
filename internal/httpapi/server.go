// Package httpapi implements Miranda's unified command interface — a single
// HTTP endpoint that accepts both Home Assistant's forwarded assistant text
// and manual curl/web UI commands — plus the WebSocket log stream the web UI
// dashboard tails in real time.
package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	agentloop "github.com/archer-developer/miranda/internal/agent_loop"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/session"
	"github.com/archer-developer/miranda/internal/users"
)

// Server is Miranda's HTTP server: the unified command interface, the
// WebSocket log stream, and (if provided) the embedded web UI.
type Server struct {
	mux           *http.ServeMux
	orchestrator  *agentloop.Orchestrator
	hub           *hub.Hub
	authToken     string
	users         *users.Registry
	sessions      *session.Store
	logger        *slog.Logger
	telegram      *TelegramWebhook // set via SetTelegramWebhook; nil means the channel is disabled
	upload        *uploadConfig    // set via SetUploadHandler; nil means POST /api/upload, GET /files/{id}, and GET /api/files/{file_id} are not registered
	oauthCallback *OAuthCallback   // set via SetOAuthCallback; nil means the OAuth2 callback route is not registered
}

// uploadConfig holds the static configuration for the upload/download
// routes. handleDownload no longer reads a fixed remote target from here —
// each attachments.Record it proxies against carries its own RemoteURL/
// RemoteToken (set by executeTool, one per file, possibly pointing at
// different backend services) — so the only thing left here is the upload
// size limit. Wired in by SetUploadHandler at startup.
type uploadConfig struct {
	maxBytes int64 // per-file size limit, enforced only on POST /api/upload
}

// NewServer builds a Server. webUI, if non-nil, is mounted at "/" (see
// internal/webui) — nil is fine for tests that only care about the API.
// usersRegistry/sessions authenticate browser-originated requests (web UI
// login) as an alternative to authToken's bearer-token auth (HA/curl/scripts);
// either may be nil, in which case that auth path is simply unavailable.
func NewServer(orchestrator *agentloop.Orchestrator, h *hub.Hub, authToken string, webUI http.Handler, logger *slog.Logger, usersRegistry *users.Registry, sessions *session.Store) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		orchestrator: orchestrator,
		hub:          h,
		authToken:    authToken,
		users:        usersRegistry,
		sessions:     sessions,
		logger:       logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /api/v1/input", s.handleInput)
	mux.HandleFunc("GET /ws/logs", s.handleWSLogs)
	mux.HandleFunc("GET /ws/chat/{username}", s.handleWSChat)
	if webUI != nil {
		mux.Handle("/", webUI)
	}
	s.mux = mux

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// SetUploadHandler wires the optional file routes in when file_upload is
// enabled, mirroring SetTTSAudioHandler's post-construction style for
// optional routes. maxBytes caps the per-file read limit
// (config.FileUploadConfig.MaxFileSizeBytes) on POST /api/upload only — a
// download's own remote size limit (e.g. the sandbox's
// max_download_size_bytes) already bounds it upstream, so nothing here
// needs to re-check it.
//
// Three routes:
//   - POST /api/upload (handleUpload) stages a file straight into
//     o.attachStore (wired separately via Orchestrator.SetAttachmentStore)
//     and returns Miranda's own file_id for later inclusion in an
//     InputRequest.Attachments list.
//   - GET /files/{id} (handleFilesServe) — unauthenticated by design, see
//     its own doc comment — is what any external MCP server's tool fetches
//     an uploaded attachment's bytes from, given the fileURI
//     processAttachments handed the model.
//   - GET /api/files/{file_id} (handleDownload) is unrelated to the above
//     two: it proxies a file the model retrieved from some other backend
//     service, detected by the generic MCP file-URI detector (see
//     Orchestrator.SetFileExposingServers — covers the sandbox's own
//     download_file the same way as any other opted-in server, no
//     server-specific path) back out to whoever the chat UI is rendering
//     a <download>...</download> marker for, using that file's own
//     attachments.Record.RemoteURL/RemoteToken rather than a single fixed
//     target.
func (s *Server) SetUploadHandler(maxBytes int64) {
	s.upload = &uploadConfig{maxBytes: maxBytes}
	s.mux.HandleFunc("POST /api/upload", s.handleUpload)
	s.mux.HandleFunc("GET /files/{id}", s.handleFilesServe)
	s.mux.HandleFunc("GET /api/files/{file_id}", s.handleDownload)
}

// SetTTSAudioHandler wires the optional GET /tts-audio/{filename} route in,
// mirroring SetTelegramWebhook's post-construction style for an optional
// dependency — no existing NewServer call site has to change to pass one
// more nil. h is a *tts.HTTPHandler (accepted as the http.Handler interface
// to avoid an import cycle risk between internal/httpapi and internal/tts;
// none exists today, but this keeps the dependency direction the same as
// every other optional-feature setter here). A nil h is a no-op — that's
// the case where TTS dispatch itself is disabled (no HA_BASE_URL) or
// gemini_tts was never configured, so nothing ever serves synthesized
// audio out of config.StorageConfig.TTSCacheDir.
//
// The route pattern captures the whole "{key}.{ext}" filename as one
// wildcard, not two: net/http's ServeMux only supports one wildcard per
// path segment, so tts.HTTPHandler.ServeHTTP splits key/ext itself.
func (s *Server) SetTTSAudioHandler(h http.Handler) {
	if h == nil {
		return
	}
	s.mux.Handle("GET /tts-audio/{filename}", h)
}

// handleHealthz is an unauthenticated liveness check — used by the Home
// Assistant thin client's config flow to validate connectivity before
// saving, and by any external process supervisor/monitoring.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	// Logged raw and first — before auth, before parsing — so a
	// misconfigured HA integration (wrong token, unexpected payload shape,
	// wrong field names) is still fully visible in logs/miranda.log instead
	// of silently 401ing or 400ing with nothing to go on.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	s.logger.Info("received input request", "remote_addr", r.RemoteAddr, "body", string(body))

	sessionUser, authenticated := s.authorize(r)
	if !authenticated {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req agentloop.InputRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}

	if sessionUser != "" {
		// Session-cookie auth: identity comes entirely from who's logged
		// in, never from client-supplied fields.
		req.UserID = sessionUser
		req.Source = agentloop.WebUISource
	} else if s.users != nil {
		// Bearer-token auth (HA thin client, curl, scripts): translate a
		// raw HA speaker-recognition user id to our canonical username, if
		// it matches a configured user, so memory/history stay keyed the
		// same way regardless of which channel a person used.
		req.UserID = s.users.ResolveUserID(req.Source, req.UserID)
	}

	s.hub.Publish(hub.Event{Source: req.Source, Message: fmt.Sprintf("%s: %s", req.UserID, req.Text)})

	turnCtx, cancel := agentloop.DetachedTurnContext(r.Context())
	defer cancel()

	resp, err := s.orchestrator.Handle(turnCtx, req)
	if err != nil {
		s.logger.Error("orchestrator.Handle failed", "error", err, "source", req.Source, "user_id", req.UserID)
		s.hub.Publish(hub.Event{Source: "error", Message: err.Error()})
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// authorize checks both auth paths, preferring a valid web UI session
// cookie over bearer-token auth: a browser tab is logged in as someone
// specific, and that identity must win even when server.auth_token is empty
// (which makes bearerAuthorized always pass — LAN-only dev mode). Checking
// the bearer token first would make that identity unreachable, since an
// empty auth_token would short-circuit every request as anonymous before
// the cookie is ever looked at.
func (s *Server) authorize(r *http.Request) (sessionUser string, ok bool) {
	if s.sessions != nil {
		if cookie, err := r.Cookie(session.CookieName); err == nil {
			if username, valid := s.sessions.Validate(cookie.Value); valid {
				return username, true
			}
		}
	}
	if s.bearerAuthorized(r) {
		return "", true
	}
	return "", false
}

func (s *Server) bearerAuthorized(r *http.Request) bool {
	if s.authToken == "" {
		return true // no token configured: LAN-only dev mode
	}
	return r.Header.Get("Authorization") == "Bearer "+s.authToken
}

// handleWSLogs streams hub events to a connected web UI tab in real time,
// replaying the recent buffer immediately on connect so a late-opened tab
// isn't blind to what already happened. Gated by the same dual auth as
// handleInput — the live log can contain full conversation text, so it's
// not something to leave open to anyone who finds the URL.
func (s *Server) handleWSLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()

	ch, replay, unsubscribe := s.hub.Subscribe(nil)
	defer unsubscribe()

	ctx := r.Context()
	for _, ev := range replay {
		if err := wsjson.Write(ctx, conn, ev); err != nil {
			return
		}
	}

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := wsjson.Write(ctx, conn, ev); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// handleWSChat streams one user's own conversation events (ChatEvent, via
// Orchestrator.publishChatMessage/publishConversationEnded) to a web UI tab
// in real time, so a reply that arrived over HA/Telegram/another tab shows
// up without a page reload — see CLAUDE.md's "Session ownership".
//
// Unlike handleWSLogs, this requires session-cookie identity specifically
// (bearer-token auth has no per-user identity to scope to) and the session's
// own username must match the {username} path segment — otherwise any
// logged-in household member could read another member's live chat just by
// changing the URL, the same IDOR handleDialogs/handleDialogMessages already
// guard against for GET /api/dialogs.
//
// The hub's replay buffer is deliberately skipped, even though Subscribe's
// filter would only surface this user's own chat events from it: it's a
// single ring buffer shared across every source and every user (bounded by
// config.WebUI.LogBufferSize), so whatever slice of it happens to still be
// in memory is not a reliable history — a busy household could evict this
// user's own chat events from the buffer entirely before they ever connect.
// The chat screen hydrates full history via GET /api/dialogs/{id} both on
// mount and on every reconnect (see chat-ws.js's onReconnect/chat.js's
// loadHistory) — that's the real resync mechanism; this connection only
// needs to carry updates live from here forward.
//
// One exception: right after subscribing, this sends a single synthetic
// "turn_in_progress" ChatEvent snapshotting whether a Handle turn is
// currently running for this user (see agentloop.TurnTracker) — not
// message history, just a live boolean (+ start time). Without it, a tab
// that reconnects (or a second tab that opens) while a turn from another
// channel/tab is mid-flight would have no way to show a waiting indicator
// until the next REST poll tick or the turn's own turn_ended event.
func (s *Server) handleWSChat(w http.ResponseWriter, r *http.Request) {
	sessionUser, ok := s.authorize(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if sessionUser == "" || sessionUser != r.PathValue("username") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()

	// Filtering at Subscribe time (rather than discarding irrelevant events
	// after they've already been pulled off the channel) means this
	// connection's bounded channel only ever holds events this user's tab
	// actually cares about — a burst of unrelated traffic (other users'
	// chat activity, log/trace lines, this same user's own assistant-text
	// streaming chunks) can no longer fill it and silently crowd out a chat
	// event meant for this connection; see hub.Hub.Subscribe's doc comment.
	ch, _, unsubscribe := s.hub.Subscribe(func(ev hub.Event) bool {
		return ev.Source == "chat" && ev.UserID == sessionUser
	})
	defer unsubscribe()

	ctx := r.Context()

	inProgress, startedAt := s.orchestrator.TurnStatus(sessionUser)
	if err := wsjson.Write(ctx, conn, hub.Event{
		Source: "chat", UserID: sessionUser,
		Data: agentloop.ChatEvent{Type: "turn_in_progress", InProgress: inProgress, StartedAt: startedAt},
	}); err != nil {
		return
	}

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := wsjson.Write(ctx, conn, ev); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
