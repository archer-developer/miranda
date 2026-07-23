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
type currentUserView struct {
	Username    string
	DisplayName string
	Avatar      string
}

func newCurrentUserView(u *users.User) currentUserView {
	if u == nil {
		return currentUserView{}
	}
	return currentUserView{Username: u.Username, DisplayName: u.DisplayName(), Avatar: avatarURL(u.Avatar)}
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
	data, err := json.Marshal(strings)
	if err != nil {
		return "{}"
	}
	return template.JS(data)
}

type indexPageData struct {
	Lang        string
	Strings     map[string]string
	StringsJSON template.JS
	User        currentUserView
	Languages   []languageOption
}

type loginPageData struct {
	Lang      string
	Strings   map[string]string
	Error     bool
	Languages []languageOption
}
