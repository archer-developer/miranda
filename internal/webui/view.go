package webui

import (
	"encoding/json"
	"html/template"
	"strings"

	"github.com/archer-developer/miranda/internal/users"
	"github.com/archer-developer/miranda/internal/webui/i18n"
)

// currentUserView is the subset of users.User templates/JS need — never the
// PasswordHash.
// currentUserView is both a Go template value (index.html/login.html) and,
// via UserJSON below, a JSON value injected as window.MIRANDA_USER for the
// SPA's profile screen — hence the json tags alongside the exported
// (template-visible) field names.
type currentUserView struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Avatar      string `json:"avatar,omitempty"`
	Language    string `json:"language,omitempty"`
	HAUserID    string `json:"haUserId,omitempty"`
}

func newCurrentUserView(u *users.User) currentUserView {
	if u == nil {
		return currentUserView{}
	}
	return currentUserView{
		Username:    u.Username,
		DisplayName: u.DisplayName(),
		Avatar:      avatarURL(u.Avatar),
		Language:    u.Language,
		HAUserID:    u.HAUserID,
	}
}

// avatarURL passes http(s) URLs through unchanged; anything else is treated
// as a filename under the configured avatars directory (see webui.New).
func avatarURL(avatar string) string {
	if avatar == "" {
		return ""
	}
	if strings.HasPrefix(avatar, "http://") || strings.HasPrefix(avatar, "https://") {
		return avatar
	}
	return "/static/avatars/" + avatar
}

type languageOption struct {
	Code   string
	Label  string
	Active bool
}

var languageLabels = map[string]string{"ru": "RU", "be": "BE", "en": "EN"}

func languageOptions(active string) []languageOption {
	opts := make([]languageOption, 0, len(i18n.Languages))
	for _, code := range i18n.Languages {
		opts = append(opts, languageOption{Code: code, Label: languageLabels[code], Active: code == active})
	}
	return opts
}

func localizedStrings(lang string) map[string]string {
	return i18n.Strings(lang)
}

// stringsJSON marshals the translation map for inlining into the page as
// window.MIRANDA_I18N, so app.js can localize the dynamic strings it
// produces (WS status changes, fetch error messages) without a round trip.
func stringsJSON(strings map[string]string) template.JS {
	return toJSON(strings)
}

// toJSON marshals any value for inlining into a <script> tag. Used for both
// window.MIRANDA_I18N and window.MIRANDA_USER — the SPA's screens read the
// latter instead of re-fetching the logged-in user's own profile fields.
func toJSON(v any) template.JS {
	data, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return template.JS(data)
}

type indexPageData struct {
	Lang            string
	Strings         map[string]string
	StringsJSON     template.JS
	User            currentUserView
	UserJSON        template.JS
	Languages       []languageOption
	WebAuthnEnabled bool
	// AssetVersion is embedded in /static/v<AssetVersion>/... URLs so a new
	// build's assets get a new URL — see staticAssetVersion in webui.go.
	AssetVersion string
}

type loginPageData struct {
	Lang        string
	Strings     map[string]string
	StringsJSON template.JS
	Error       bool
	// Expired distinguishes a bounce-back from auth-fetch.js's global 401/403
	// handler (session died server-side, e.g. a process restart wiped
	// internal/session.Store's in-memory map) from a wrong-password Error —
	// same alert box, a message that doesn't imply the user typed something
	// wrong.
	Expired         bool
	Languages       []languageOption
	WebAuthnEnabled bool
	AssetVersion    string
}

// manifestData is the (sole) value templates/manifest.webmanifest is
// rendered with — see Handler.handleManifest.
type manifestData struct {
	AssetVersion string
}
