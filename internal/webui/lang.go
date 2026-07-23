package webui

import (
	"net/http"
	"strings"
	"time"

	"github.com/archer-developer/miranda/internal/users"
	"github.com/archer-developer/miranda/internal/webui/i18n"
)

const langCookieName = "miranda_lang"

// resolveLanguage picks the UI language for this request: an explicit
// per-browser cookie (set via the header switcher) wins, then the signed-in
// user's configured preference, then the server-wide default.
func (h *Handler) resolveLanguage(r *http.Request, user *users.User) string {
	if cookie, err := r.Cookie(langCookieName); err == nil && i18n.IsSupported(cookie.Value) {
		return cookie.Value
	}
	if user != nil && i18n.IsSupported(user.Language) {
		return user.Language
	}
	if i18n.IsSupported(h.defaultLanguage) {
		return h.defaultLanguage
	}
	return i18n.Default
}

// handleSetLanguage is a plain-link endpoint (no JS required) the header's
// language switcher points at: it sets the language cookie and redirects
// back to wherever the request came from.
func (h *Handler) handleSetLanguage(w http.ResponseWriter, r *http.Request) {
	if lang := r.URL.Query().Get("lang"); i18n.IsSupported(lang) {
		setLangCookie(w, lang)
	}
	http.Redirect(w, r, safeRedirectTarget(r.URL.Query().Get("next")), http.StatusSeeOther)
}

// safeRedirectTarget only allows same-site, path-absolute targets, so
// ?next= can't be used to bounce a user off-site (open redirect).
func safeRedirectTarget(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

func setLangCookie(w http.ResponseWriter, lang string) {
	http.SetCookie(w, &http.Cookie{
		Name:     langCookieName,
		Value:    lang,
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
	})
}
