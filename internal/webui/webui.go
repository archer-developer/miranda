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
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	texttemplate "text/template"

	"github.com/go-webauthn/webauthn/protocol"

	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/session"
	"github.com/archer-developer/miranda/internal/users"
	"github.com/archer-developer/miranda/internal/webauthn"
)

//go:embed templates/index.html templates/login.html templates/manifest.webmanifest
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

// WebAuthnService is the subset of *webauthn.Service the dashboard needs
// for passkey registration/login/management. A nil WebAuthnService passed
// to New disables the feature entirely — see New's doc comment.
type WebAuthnService interface {
	BeginRegistration(ctx context.Context, username, ceremonyKey string) (*protocol.CredentialCreation, error)
	FinishRegistration(ctx context.Context, username, ceremonyKey, nickname string, body []byte) (webauthn.CredentialInfo, error)
	BeginDiscoverableLogin(ctx context.Context) (*protocol.CredentialAssertion, string, error)
	FinishDiscoverableLogin(ctx context.Context, ceremonyID string, body []byte) (username string, credentialID, prfOutput []byte, err error)
	ListCredentials(ctx context.Context, username string) ([]webauthn.CredentialInfo, error)
	DeleteCredential(ctx context.Context, username string, credentialID []byte) error
	// BeginKeyProbe/FinishKeyProbe run a follow-up assertion ceremony scoped
	// to one just-registered credential, so a KeyringService (below) can
	// capture its PRF output right after registration — see
	// internal/webauthn.Service.BeginKeyProbe's doc comment.
	BeginKeyProbe(ctx context.Context, username, ceremonyKey string, credentialID []byte) (*protocol.CredentialAssertion, error)
	FinishKeyProbe(ctx context.Context, username, ceremonyKey string, body []byte) (credentialID, prfOutput []byte, err error)
}

// KeyringService is the subset of *keyring.Service the dashboard needs to
// unlock/lock a user's master key around login/logout, and to add a newly
// registered passkey's wrapped-key slot — see internal/keyring and
// docs/encryption.md for the full design. The keyring has no config
// toggle — cmd/miranda always passes a real one — so a nil KeyringService
// here only ever happens in tests that don't exercise it: login/logout
// simply skip these calls, and the passkey registration flow skips its
// PRF probe follow-up too.
type KeyringService interface {
	UnlockWithPassword(ctx context.Context, username, password string) error
	UnlockWithPRF(ctx context.Context, username string, credentialID, prfOutput []byte) error
	AddPasskeySlot(ctx context.Context, username string, credentialID, prfOutput []byte) error
	Lock(username string)
	RemoveSlotForCredential(ctx context.Context, username string, credentialID []byte) error
}

// Handler serves the dashboard page, its static assets, the auth flow, the
// dialog history JSON API, the current user's memory file API, and
// (optionally) passkey registration/login.
type Handler struct {
	mux             *http.ServeMux
	indexTmpl       *template.Template
	loginTmpl       *template.Template
	manifestTmpl    *texttemplate.Template // text/template, not html/template: the output is JSON, not HTML
	history         History
	memory          Memory
	webauthn        WebAuthnService // nil disables passkey login/registration entirely
	keyring         KeyringService  // nil only in tests — cmd/miranda always passes a real one
	users           *users.Registry
	sessions        *session.Store
	defaultLanguage string
	assetVersion    string // see staticAssetVersion; templates embed this in /static/v<version>/ URLs
	logger          *slog.Logger
}

// staticAssetVersion hashes every embedded static file's content into a
// short hex digest. Since staticFS is compiled into the binary via go:embed,
// this value is stable for a given build and changes automatically whenever
// any static asset changes — which is what lets templates serve assets at
// /static/v<version>/... and have that path change (and therefore bust
// every client's cache) on every new build, with zero manual bookkeeping.
func staticAssetVersion(fsys fs.FS) (string, error) {
	h := sha256.New()
	err := fs.WalkDir(fsys, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := fsys.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.WriteString(h, path+"\x00"); err != nil {
			return err
		}
		_, err = io.Copy(h, f)
		return err
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// New builds a Handler. usersRegistry and sessions back the mandatory login
// flow (see auth.go) — with an empty usersRegistry, nobody can log in and
// the dashboard is unreachable, which is the intended fail-closed state
// rather than a bug to work around. avatarsDir, if non-empty, is served at
// /static/avatars/ for UserConfig.Avatar values that name a local file
// rather than an http(s) URL. webauthnSvc may be nil (config.WebAuthnConfig.Enabled
// false, the default) — in that case the six /api/webauthn/* routes are
// never registered at all, and the frontend's own capability check
// (window.PublicKeyCredential) keeps the passkey UI hidden; nothing in this
// package needs a separate "is it enabled" branch beyond this one nil check.
// keyringSvc may independently be nil in tests that don't exercise it — see
// KeyringService's doc comment.
func New(h History, mem Memory, webauthnSvc WebAuthnService, keyringSvc KeyringService, usersRegistry *users.Registry, sessions *session.Store, defaultLanguage, avatarsDir string, logger *slog.Logger) (*Handler, error) {
	indexTmpl, err := template.ParseFS(templatesFS, "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("webui: parse index template: %w", err)
	}
	loginTmpl, err := template.ParseFS(templatesFS, "templates/login.html")
	if err != nil {
		return nil, fmt.Errorf("webui: parse login template: %w", err)
	}
	manifestTmpl, err := texttemplate.ParseFS(templatesFS, "templates/manifest.webmanifest")
	if err != nil {
		return nil, fmt.Errorf("webui: parse manifest template: %w", err)
	}
	assetVersion, err := staticAssetVersion(staticFS)
	if err != nil {
		return nil, fmt.Errorf("webui: hash static assets: %w", err)
	}

	handler := &Handler{
		indexTmpl:       indexTmpl,
		loginTmpl:       loginTmpl,
		manifestTmpl:    manifestTmpl,
		history:         h,
		memory:          mem,
		webauthn:        webauthnSvc,
		keyring:         keyringSvc,
		users:           usersRegistry,
		sessions:        sessions,
		defaultLanguage: defaultLanguage,
		assetVersion:    assetVersion,
		logger:          logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", handler.handleLoginPage)
	mux.HandleFunc("POST /login", handler.handleLoginSubmit)
	mux.HandleFunc("POST /logout", handler.handleLogout)
	mux.HandleFunc("GET /set-lang", handler.handleSetLanguage)
	mux.Handle("GET /static/", http.FileServerFS(staticFS)) // public: needed to render the login page itself
	// Registered as a more specific pattern than "/static/" above (same
	// precedence rule the avatarsDir block below relies on), so this wins
	// for exactly this one path — templates/manifest.webmanifest is
	// rendered here instead of served from staticFS so its icon URLs can
	// embed AssetVersion (see handleManifest and that template's own
	// comment for why the manifest's own URL still can't be versioned).
	mux.HandleFunc("GET /static/manifest.webmanifest", handler.handleManifest)
	// The versioned alias templates actually link to: same content, but at a
	// path that changes on every build, so it's safe to tell browsers to
	// cache it forever instead of revalidating on every page load (the
	// plain /static/ above never got a Cache-Control header at all, which is
	// why nothing was being cached — see New's doc comment).
	versionedPrefix := "/static/v" + assetVersion + "/"
	mux.Handle("GET "+versionedPrefix, cacheForever(versionedStatic(versionedPrefix)))
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
	mux.Handle("GET /api/session", handler.requireAuthAPI(http.HandlerFunc(handler.handleSessionCheck)))

	if webauthnSvc != nil {
		// Registration/management require being logged in already (adding a
		// passkey from the profile screen); login is of course the one pair
		// that must be reachable while unauthenticated — it *is* a login
		// mechanism.
		mux.Handle("POST /api/webauthn/register/begin", handler.requireAuthAPI(http.HandlerFunc(handler.handleWebAuthnRegisterBegin)))
		mux.Handle("POST /api/webauthn/register/finish", handler.requireAuthAPI(http.HandlerFunc(handler.handleWebAuthnRegisterFinish)))
		mux.HandleFunc("POST /api/webauthn/login/begin", handler.handleWebAuthnLoginBegin)
		mux.HandleFunc("POST /api/webauthn/login/finish", handler.handleWebAuthnLoginFinish)
		mux.Handle("GET /api/webauthn/credentials", handler.requireAuthAPI(http.HandlerFunc(handler.handleWebAuthnListCredentials)))
		mux.Handle("DELETE /api/webauthn/credentials/{id}", handler.requireAuthAPI(http.HandlerFunc(handler.handleWebAuthnDeleteCredential)))
		if keyringSvc != nil {
			// The PRF-capture follow-up ceremony (see
			// internal/webauthn.Service.BeginKeyProbe) only makes sense when
			// there's a KeyringService to hand its output to — with
			// webauthnSvc set but keyringSvc nil (passkeys enabled, data
			// encryption not), registration completes without ever running
			// this pair, same as if the credential's authenticator simply
			// didn't support PRF.
			mux.Handle("POST /api/webauthn/register/probe-begin", handler.requireAuthAPI(http.HandlerFunc(handler.handleWebAuthnRegisterProbeBegin)))
			mux.Handle("POST /api/webauthn/register/probe-finish", handler.requireAuthAPI(http.HandlerFunc(handler.handleWebAuthnRegisterProbeFinish)))
		}
	}
	handler.mux = mux

	return handler, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// versionedStatic rewrites a request under prefix (e.g. "/static/v<hash>/")
// back to the plain "/static/..." path staticFS actually holds, then serves
// it from the embedded FS. The version segment only exists to make the URL
// change when the build's assets change; it carries no other meaning, so
// there's nothing to validate.
func versionedStatic(prefix string) http.Handler {
	fileServer := http.FileServerFS(staticFS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := new(http.Request)
		*r2 = *r
		r2.URL = new(url.URL)
		*r2.URL = *r.URL
		r2.URL.Path = "/static/" + strings.TrimPrefix(r.URL.Path, prefix)
		fileServer.ServeHTTP(w, r2)
	})
}

// cacheForever marks a response as safe for browsers/proxies to cache
// indefinitely without revalidating — only correct for content served at a
// content-derived URL (see versionedStatic), since the only way to get a
// client to see new content is to change the URL.
func cacheForever(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	})
}

// handleManifest renders the PWA manifest with the current build's
// AssetVersion baked into its icon URLs, so the icons get the same
// cache-forever treatment as every other static asset — unlike the
// manifest's own URL, which must stay fixed (see the route registration
// comment in New).
func (h *Handler) handleManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	_ = h.manifestTmpl.Execute(w, manifestData{AssetVersion: h.assetVersion})
}

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	lang := h.resolveLanguage(r, user)
	strings := localizedStrings(lang)
	userView := newCurrentUserView(user)

	data := indexPageData{
		Lang:            lang,
		Strings:         strings,
		StringsJSON:     stringsJSON(strings),
		User:            userView,
		UserJSON:        toJSON(userView),
		Languages:       languageOptions(lang),
		WebAuthnEnabled: h.webauthn != nil,
		AssetVersion:    h.assetVersion,
	}

	// no-store: this is the PWA's shell document, served at the fixed "/"
	// URL an installed home-screen icon always reopens — unlike the
	// versioned /static/vX/ assets it references, there's no URL change to
	// bust a cache with, so it must never be cached at all or an installed
	// app can get stuck on a stale build until the OS evicts it. Pairs with
	// the "Refresh" button on the profile screen (screens/profile.js), which
	// calls location.reload() to force this fetch on demand.
	w.Header().Set("Cache-Control", "no-store")
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
