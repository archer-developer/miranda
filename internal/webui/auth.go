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
	data := loginPageData{
		Lang:      lang,
		Strings:   localizedStrings(lang),
		Error:     r.URL.Query().Get("error") == "1",
		Languages: languageOptions(lang),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.loginTmpl.Execute(w, data)
}

func (h *Handler) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}

	user, ok := h.users.Authenticate(r.FormValue("username"), r.FormValue("password"))
	if !ok {
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}

	token, err := h.sessions.Create(user.Username)
	if err != nil {
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
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

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(session.CookieName); err == nil {
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
