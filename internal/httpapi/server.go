// Package httpapi implements Miranda's unified command interface — a single
// HTTP endpoint that accepts both Home Assistant's forwarded assistant text
// and manual curl/web UI commands — plus the WebSocket log stream the web UI
// dashboard tails in real time.
package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/archer-developer/miranda/internal/hub"
)

// Server is Miranda's HTTP server: the unified command interface, the
// WebSocket log stream, and (if provided) the embedded web UI.
type Server struct {
	mux          *http.ServeMux
	orchestrator *Orchestrator
	hub          *hub.Hub
	authToken    string
	logger       *slog.Logger
}

// NewServer builds a Server. webUI, if non-nil, is mounted at "/" (see
// internal/webui) — nil is fine for tests that only care about the API.
func NewServer(orchestrator *Orchestrator, h *hub.Hub, authToken string, webUI http.Handler, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{orchestrator: orchestrator, hub: h, authToken: authToken, logger: logger}

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
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req InputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}

	s.hub.Publish(hub.Event{Source: req.Source, Message: fmt.Sprintf("%s: %s", req.UserID, req.Text)})

	resp, err := s.orchestrator.Handle(r.Context(), req)
	if err != nil {
		s.logger.Error("orchestrator.Handle failed", "error", err, "source", req.Source, "user_id", req.UserID)
		s.hub.Publish(hub.Event{Source: "error", Message: err.Error()})
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) authorized(r *http.Request) bool {
	if s.authToken == "" {
		return true // no token configured: LAN-only dev mode
	}
	return r.Header.Get("Authorization") == "Bearer "+s.authToken
}

// handleWSLogs streams hub events to a connected web UI tab in real time,
// replaying the recent buffer immediately on connect so a late-opened tab
// isn't blind to what already happened.
func (s *Server) handleWSLogs(w http.ResponseWriter, r *http.Request) {
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
