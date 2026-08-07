package webui

import (
	"context"
	"net/http"

	"github.com/archer-developer/miranda/internal/session"
	"github.com/archer-developer/miranda/internal/users"
)

type ctxKey int

const userCtxKey ctxKey = iota

// requireAuth gates an HTML page: unauthenticated requests are redirected
// to the login page.
func (h *Handler) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := h.authenticatedUser(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, user)))
	})
}

// requireAuthAPI gates a JSON endpoint: unauthenticated requests get a 401
// instead of a redirect, since the caller here is fetch(), not a browser
// navigation.
func (h *Handler) requireAuthAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := h.authenticatedUser(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, user)))
	})
}

// handleSessionCheck is a deliberately trivial authenticated GET —
// requireAuthAPI does all the actual work — that reconnecting-ws.js pings
// after a WebSocket close. A WS handshake the server rejected for a dead
// session (see internal/httpapi/server.go's authorize()) surfaces to the
// browser as a generic, reason-less close (browsers don't expose the HTTP
// status a rejected Upgrade request got); this endpoint gives that dead-end
// a way back into the ordinary fetch()-based 401/403 handling in
// auth-fetch.js instead of retrying the WS forever with no explanation.
func (h *Handler) handleSessionCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// authenticatedUser validates the session cookie, if any, against the
// session store and resolves it to the full User record.
func (h *Handler) authenticatedUser(r *http.Request) (users.User, bool) {
	cookie, err := r.Cookie(session.CookieName)
	if err != nil {
		return users.User{}, false
	}
	username, ok := h.sessions.Validate(cookie.Value)
	if !ok {
		return users.User{}, false
	}
	return h.users.Get(username)
}

// currentUser reads the User a requireAuth*/requireAuthAPI middleware
// already resolved and stashed in the request context.
func currentUser(r *http.Request) *users.User {
	u, ok := r.Context().Value(userCtxKey).(users.User)
	if !ok {
		return nil
	}
	return &u
}

func (h *Handler) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authenticatedUser(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	lang := h.resolveLanguage(r, nil)
	strings := localizedStrings(lang)
	data := loginPageData{
		Lang:            lang,
		Strings:         strings,
		StringsJSON:     stringsJSON(strings),
		Error:           r.URL.Query().Get("error") == "1",
		Expired:         r.URL.Query().Get("expired") == "1",
		Languages:       languageOptions(lang),
		WebAuthnEnabled: h.webauthn != nil,
		AssetVersion:    h.assetVersion,
	}

	// no-store: same reasoning as handleIndex — the login page is just as
	// valid a PWA entry point (see login.html's manifest link) and must
	// never be served stale from cache either.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.loginTmpl.Execute(w, data)
}

func (h *Handler) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}

	password := r.FormValue("password")
	user, ok := h.users.Authenticate(r.FormValue("username"), password)
	if !ok {
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}

	// Best-effort: derive/unwrap this user's master key from the plaintext
	// password while it's still in scope (never persisted — see
	// internal/keyring). A keyring failure must never turn into a login
	// failure; it just means encrypted-data tools stay unavailable this
	// session, same as if the feature were disabled.
	if h.keyring != nil {
		if err := h.keyring.UnlockWithPassword(r.Context(), user.Username, password); err != nil {
			h.logger.Warn("keyring: unlock with password failed", "username", user.Username, "error", err)
		}
	}

	if err := h.issueSessionCookie(w, r, user); err != nil {
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// issueSessionCookie creates a session for user and sets the session
// cookie — the final step shared by every login method (password form,
// WebAuthn — see webauthn.go's login/finish handler), so the two auth paths
// can never drift apart on cookie attributes or first-login language
// adoption.
func (h *Handler) issueSessionCookie(w http.ResponseWriter, r *http.Request, user users.User) error {
	token, err := h.sessions.Create(user.Username)
	if err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     session.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		// Lax rather than Strict: still blocks the cookie on cross-site POSTs
		// (the CSRF-relevant case) while allowing normal top-level navigation
		// to work. No separate CSRF token — this is a small home-LAN
		// dashboard, not a multi-tenant public service.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(h.sessions.TTL().Seconds()),
	})

	// First login this session: adopt the account's preferred language if
	// nothing's been explicitly chosen via the switcher yet.
	if user.Language != "" {
		if _, err := r.Cookie(langCookieName); err != nil {
			setLangCookie(w, user.Language)
		}
	}
	return nil
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(session.CookieName); err == nil {
		// Resolve the username before destroying the session token, so the
		// user's unlocked master key can be dropped from memory on explicit
		// logout — the one thing (besides process restart) that ever locks
		// it, since internal/keyring.Cache has no auto-lock timer.
		if h.keyring != nil {
			if username, ok := h.sessions.Validate(cookie.Value); ok {
				h.keyring.Lock(username)
			}
		}
		h.sessions.Destroy(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     session.CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
