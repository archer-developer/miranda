// Package httpapi implements Miranda's unified command interface — a single
// HTTP endpoint that accepts both Home Assistant's forwarded assistant text
// and manual curl/web UI commands — plus the WebSocket log stream the web UI
// dashboard tails in real time.
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/session"
	"github.com/archer-developer/miranda/internal/users"
)

// webUISource is the InputRequest.Source value forced onto any request
// authenticated via a web UI session cookie, regardless of what the client
// sent — a browser can't be allowed to claim source: "ha_assist" and ride
// the HA user-id-mapping path, or spoof another user_id.
const webUISource = "web_ui"

// turnTimeout bounds one full agent turn: LLM generation, tool calls, and
// any synchronous TTS dispatch (speakChunks blocks for the physical
// speaker's actual playback duration — see internal/tts.Dispatcher.waitIdle
// — which for a multi-sentence reply can legitimately take well past what a
// browser tab, VPN link, or reverse proxy will hold a connection open for).
// detachedTurnContext below is what makes this the thing that bounds a
// turn instead of the caller's connection: once a reply may already have
// been spoken aloud or sent to Telegram, it must still get recorded to
// history even if the original HTTP/webhook connection drops first.
const turnTimeout = 5 * time.Minute

// detachedTurnContext derives a context for one orchestrator.Handle call
// that keeps parent values (deadlines aside) but is immune to the parent
// being cancelled by the inbound connection closing — see turnTimeout.
// Without this, a dropped client connection cancels ctx mid-turn, and
// everything downstream that still needs to run (finishing TTS chunks
// already audible on a physical speaker, persisting the assistant's
// reply to history so the next turn sees it) fails with context.Canceled.
func detachedTurnContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), turnTimeout)
}

// Server is Miranda's HTTP server: the unified command interface, the
// WebSocket log stream, and (if provided) the embedded web UI.
type Server struct {
	mux          *http.ServeMux
	orchestrator *Orchestrator
	hub          *hub.Hub
	authToken    string
	users        *users.Registry
	sessions     *session.Store
	logger       *slog.Logger
	telegram     *TelegramWebhook // set via SetTelegramWebhook; nil means the channel is disabled
}

// NewServer builds a Server. webUI, if non-nil, is mounted at "/" (see
// internal/webui) — nil is fine for tests that only care about the API.
// usersRegistry/sessions authenticate browser-originated requests (web UI
// login) as an alternative to authToken's bearer-token auth (HA/curl/scripts);
// either may be nil, in which case that auth path is simply unavailable.
func NewServer(orchestrator *Orchestrator, h *hub.Hub, authToken string, webUI http.Handler, logger *slog.Logger, usersRegistry *users.Registry, sessions *session.Store) *Server {
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
	if webUI != nil {
		mux.Handle("/", webUI)
	}
	s.mux = mux

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
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

	var req InputRequest
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
		req.Source = webUISource
	} else if s.users != nil {
		// Bearer-token auth (HA thin client, curl, scripts): translate a
		// raw HA speaker-recognition user id to our canonical username, if
		// it matches a configured user, so memory/history stay keyed the
		// same way regardless of which channel a person used.
		req.UserID = s.users.ResolveUserID(req.Source, req.UserID)
	}

	s.hub.Publish(hub.Event{Source: req.Source, Message: fmt.Sprintf("%s: %s", req.UserID, req.Text)})

	turnCtx, cancel := detachedTurnContext(r.Context())
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

	ch, replay, unsubscribe := s.hub.Subscribe()
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
