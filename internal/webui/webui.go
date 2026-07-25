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
	GetConversation(ctx context.Context, conversationID string) (*history.Conversation, error)
}

// Memory is the subset of *memory.Store the dashboard needs to show and let
// the logged-in user edit their own memory file.
type Memory interface {
	Read(userID string) (string, error)
	Write(userID, content string) error
}

// Handler serves the dashboard page, its static assets, the auth flow, the
// dialog history JSON API, and the current user's memory file API.
type Handler struct {
	mux             *http.ServeMux
	indexTmpl       *template.Template
	loginTmpl       *template.Template
	history         History
	memory          Memory
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
func New(h History, mem Memory, usersRegistry *users.Registry, sessions *session.Store, defaultLanguage, avatarsDir string) (*Handler, error) {
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
		memory:          mem,
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
	mux.Handle("GET /api/memory", handler.requireAuthAPI(http.HandlerFunc(handler.handleGetMemory)))
	mux.Handle("PUT /api/memory", handler.requireAuthAPI(http.HandlerFunc(handler.handlePutMemory)))
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

// handleDialogs lists the logged-in user's own conversations, newest first.
// It deliberately ignores any user_id the client might send: history is only
// ever browsable for yourself, never for another account.
func (h *Handler) handleDialogs(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)

	limit := defaultDialogLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}

	conversations, err := h.history.RecentConversations(r.Context(), user.Username, limit)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, conversations)
}

// handleDialogMessages returns one conversation's messages, but only if it
// belongs to the logged-in user — otherwise a guessable conversation id
// would leak another account's dialog.
func (h *Handler) handleDialogMessages(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := r.PathValue("id")

	conv, err := h.history.GetConversation(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if conv == nil || conv.UserID != user.Username {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	messages, err := h.history.ConversationMessages(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, messages)
}

// handleGetMemory returns the logged-in user's memory file content, so the
// dashboard can show/edit it. An empty content means they have no memory
// file yet — not an error.
func (h *Handler) handleGetMemory(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)

	content, err := h.memory.Read(user.Username)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, memoryView{Content: content})
}

// handlePutMemory overwrites the logged-in user's entire memory file with
// the submitted content — the dashboard editor's save action.
func (h *Handler) handlePutMemory(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)

	var body memoryView
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.memory.Write(user.Username, body.Content); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, memoryView{Content: body.Content})
}

// memoryView is the JSON shape for GET/PUT /api/memory.
type memoryView struct {
	Content string `json:"content"`
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
