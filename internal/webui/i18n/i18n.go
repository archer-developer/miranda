// Package i18n holds the web UI's translation catalog (Russian, Belarusian,
// English) as embedded JSON, one flat key->string map per language. It only
// covers UI chrome (labels, buttons, status text) — it has nothing to do
// with what language Miranda converses in, which is unconstrained (see
// ConversationEntity.supported_languages = "*" in the HA thin client).
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
)

//go:embed locales/*.json
var localesFS embed.FS

// Default is used whenever a request's resolved language isn't one of
// Languages, and matches the project's Belarusian-first default elsewhere
// (web_ui.default_language in config.yaml).
const Default = "be"

// Languages are the supported locale codes, in the order they should appear
// in the UI's language switcher.
var Languages = []string{"be", "ru", "en"}

var catalog map[string]map[string]string

func init() {
	catalog = make(map[string]map[string]string, len(Languages))
	for _, lang := range Languages {
		data, err := localesFS.ReadFile("locales/" + lang + ".json")
		if err != nil {
			// The locale files are embedded at build time; a missing one is
			// a packaging bug, not a runtime condition to handle gracefully.
			panic(fmt.Sprintf("i18n: missing embedded locale %q: %v", lang, err))
		}
		var strings map[string]string
		if err := json.Unmarshal(data, &strings); err != nil {
			panic(fmt.Sprintf("i18n: invalid locale %q: %v", lang, err))
		}
		catalog[lang] = strings
	}
}

// IsSupported reports whether lang is one of Languages.
func IsSupported(lang string) bool {
	_, ok := catalog[lang]
	return ok
}

// Strings returns the full translation map for lang, falling back to
// Default if lang isn't supported.
func Strings(lang string) map[string]string {
	if strings, ok := catalog[lang]; ok {
		return strings
	}
	return catalog[Default]
}
