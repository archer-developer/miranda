// Package webui serves Miranda's monitoring dashboard: a server-rendered Go
// template plus a small vanilla-JS client, styled with Tailwind CSS v4
// (compiled ahead of time by the standalone CLI — see scripts/build-css.sh —
// so there's no Node/npm runtime dependency, only a build-time one). The
// live log tail itself is served by internal/httpapi's /ws/logs endpoint;
// this package serves the page shell, static assets, the dialog history
// JSON API, and — since login is mandatory — the whole auth flow (see
// auth.go) and UI language selection (see lang.go).
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
	"github.com/archer-developer/miranda/internal/session"
	"github.com/archer-developer/miranda/internal/users"
)

//go:embed templates/index.html templates/login.html
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

// Handler serves the dashboard page, its static assets, the auth flow, and
// the dialog history JSON API.
type Handler struct {
	mux             *http.ServeMux
	indexTmpl       *template.Template
	loginTmpl       *template.Template
	history         History
	users           *users.Registry
	sessions        *session.Store
	defaultLanguage string
}

// New builds a Handler. usersRegistry and sessions back the mandatory login
// flow (see auth.go) — with an empty usersRegistry, nobody can log in and
// the dashboard is unreachable, which is the intended fail-closed state
// rather than a bug to work around. avatarsDir, if non-empty, is served at
// /static/avatars/ for UserConfig.Avatar values that name a local file
// rather than an http(s) URL.
func New(h History, usersRegistry *users.Registry, sessions *session.Store, defaultLanguage, avatarsDir string) (*Handler, error) {
	indexTmpl, err := template.ParseFS(templatesFS, "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("webui: parse index template: %w", err)
	}
	loginTmpl, err := template.ParseFS(templatesFS, "templates/login.html")
	if err != nil {
		return nil, fmt.Errorf("webui: parse login template: %w", err)
	}

	handler := &Handler{
		indexTmpl:       indexTmpl,
		loginTmpl:       loginTmpl,
		history:         h,
		users:           usersRegistry,
		sessions:        sessions,
		defaultLanguage: defaultLanguage,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", handler.handleLoginPage)
	mux.HandleFunc("POST /login", handler.handleLoginSubmit)
	mux.HandleFunc("POST /logout", handler.handleLogout)
	mux.HandleFunc("GET /set-lang", handler.handleSetLanguage)
	mux.Handle("GET /static/", http.FileServerFS(staticFS)) // public: needed to render the login page itself
	if avatarsDir != "" {
		// Registered as a more specific pattern than "/static/" above;
		// net/http's ServeMux routes to the longest matching pattern
		// regardless of registration order, so this correctly takes
		// priority for anything under /static/avatars/.
		mux.Handle("GET /static/avatars/", http.StripPrefix("/static/avatars/", http.FileServer(http.Dir(avatarsDir))))
	}

	mux.Handle("GET /{$}", handler.requireAuth(http.HandlerFunc(handler.handleIndex)))
	mux.Handle("GET /api/dialogs", handler.requireAuthAPI(http.HandlerFunc(handler.handleDialogs)))
	mux.Handle("GET /api/dialogs/{id}", handler.requireAuthAPI(http.HandlerFunc(handler.handleDialogMessages)))
	handler.mux = mux

	return handler, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	lang := h.resolveLanguage(r, user)
	strings := localizedStrings(lang)

	data := indexPageData{
		Lang:        lang,
		Strings:     strings,
		StringsJSON: stringsJSON(strings),
		User:        newCurrentUserView(user),
		Languages:   languageOptions(lang),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.indexTmpl.Execute(w, data)
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
