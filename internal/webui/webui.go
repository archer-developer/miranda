// Package webui serves Miranda's monitoring dashboard: a server-rendered Go
// template plus a small vanilla-JS client, styled with Tailwind CSS v4
// (compiled ahead of time by the standalone CLI — see scripts/build-css.sh —
// so there's no Node/npm runtime dependency, only a build-time one). The
// live log tail itself is served by internal/httpapi's /ws/logs endpoint;
// this package only serves the page shell, static assets, and a small JSON
// API for browsing dialog history.
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"

	"github.com/archer-developer/miranda/internal/history"
)

//go:embed templates/index.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

const defaultDialogLimit = 20

// History is the subset of *history.Store the dashboard needs to browse
// past conversations.
type History interface {
	RecentConversations(ctx context.Context, userID string, limit int) ([]history.Conversation, error)
	ConversationMessages(ctx context.Context, conversationID string) ([]history.Message, error)
}

// Handler serves the dashboard page, its static assets, and the dialog
// history JSON API.
type Handler struct {
	mux     *http.ServeMux
	tmpl    *template.Template
	history History
}

// New builds a Handler backed by history.
func New(h History) (*Handler, error) {
	tmpl, err := template.ParseFS(templatesFS, "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("webui: parse template: %w", err)
	}

	handler := &Handler{tmpl: tmpl, history: h}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handler.handleIndex)
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /api/dialogs", handler.handleDialogs)
	mux.HandleFunc("GET /api/dialogs/{id}", handler.handleDialogMessages)
	handler.mux = mux

	return handler, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.tmpl.Execute(w, nil)
}

func (h *Handler) handleDialogs(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		http.Error(w, "user_id is required", http.StatusBadRequest)
		return
	}

	limit := defaultDialogLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}

	conversations, err := h.history.RecentConversations(r.Context(), userID, limit)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, conversations)
}

func (h *Handler) handleDialogMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	messages, err := h.history.ConversationMessages(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, messages)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
