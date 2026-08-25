package redact

import (
	"regexp"
	"unicode"
)

// Tuning constants New falls back to when the config leaves them at zero.
const (
	// defaultWindowRunes is how far past a trigger word a value may sit and
	// still count as that trigger's value. 40 is sized off the motivating
	// example — "пин-код от телефона Ани 665533" puts 18 characters of
	// ordinary Russian between the trigger and the number.
	defaultWindowRunes = 40
	// defaultMaxMaskLength caps a mask so a redacted PEM block does not
	// become several hundred asterisks.
	defaultMaxMaskLength = 32
	// maxValueRunes is how far past the window a value may run before the
	// search gives up. The window bounds where a value may start; this bounds
	// how long it may be, so a 200-character API key written right after
	// "пароль:" is masked whole instead of only up to the window edge.
	// Anything longer is a key block, which the format rules take wholesale.
	maxValueRunes = 512
	// maskChar is what a redacted value is replaced with. Single-character,
	// ASCII, and JSON-safe on purpose: llm.log's request/response blocks are
	// marshalled JSON, and masking must not be able to invalidate them.
	maskChar = "*"
)

// valueRule matches the *value* half of an anchored rule — the thing that
// follows a trigger word. Ordered; the index breaks ties in ranking, so this
// slice must never become a map.
type valueRule struct {
	name  string
	re    *regexp.Regexp
	group int
	// validate rejects a syntactic match that isn't plausibly a secret.
	// Needed because RE2 has no lookahead, so "contains both a digit and a
	// letter" cannot be expressed in the pattern itself.
	validate func(string) bool
}

// valueRules is tried in order for every trigger occurrence. The first two
// are anchored to the start of the window: an explicit separator right after
// the trigger ("пароль: hunter2", `"password": "hunter2"`) is strong enough
// evidence to mask a value that would otherwise look like an ordinary word.
// The bare token and digit runs are the unanchored fallbacks.
//
// Two invariants hold across every rule here, and both are load-bearing:
//
//   - No matched span may contain a double quote or a backslash. Most of what
//     this package masks is the marshalled JSON in llm.log, and a span that
//     could straddle a quote — or swallow the backslash escaping one — would
//     let masking turn valid JSON into invalid JSON. Since maskChar needs no
//     escaping and a mask is never longer than what it replaces, excluding
//     those two bytes is what makes JSON validity structural rather than
//     hoped-for.
//   - maskChar is absent from every character class. Together with
//     hasDigitAndLetter that is what stops an already-masked run of asterisks
//     from matching again, and hence what makes Redact idempotent.
var valueRules = []valueRule{
	{
		// `"password": "hunter2"` — the JSON shape, masking strictly inside
		// the value's quotes. The leading `"?` absorbs the quote that closes
		// the key, which is why this works on a trace dump.
		name:  "json_value",
		re:    regexp.MustCompile(`^"?\s*[:=]\s*"([^"\n\\]{1,128})"`),
		group: 1,
	},
	{
		// "пароль: hunter2", "password=abc123", "код — 4455".
		name:     "separator_value",
		re:       regexp.MustCompile(`^"?\s*(?:[:=]|[-—–])\s*([^\s,;!?"\\]{4,})`),
		group:    1,
		validate: notCyrillicWord,
	},
	{
		// Guillemets are the Russian quoting convention and never appear as
		// JSON structure, so this one needs no anchoring.
		name:  "quoted",
		re:    regexp.MustCompile(`«([^»\n"\\]{1,128})»`),
		group: 1,
	},
	{
		// A mixed alphanumeric run. The first character class omits "=" so
		// this cannot start on the separator in "password=abc123xyz" and
		// out-leftmost separator_value, which knows to mask only the value.
		name:     "token",
		re:       regexp.MustCompile(`[A-Za-z0-9!@#$%^&_+-][A-Za-z0-9!@#$%^&_+=-]{7,}`),
		validate: hasDigitAndLetter,
	},
	{name: "digits", re: regexp.MustCompile(`\d{3,}`)},
}

// timeLikeRE guards the digit-run rule against masking a clock time, which is
// the most common innocent digit run to sit next to a trigger word
// ("пришли код в 18:30").
var timeLikeRE = regexp.MustCompile(`^\d{1,2}:\d{2}`)

// formatRule matches a value that identifies itself by shape and needs no
// trigger word. Ordered, for the same determinism reason as valueRules.
type formatRule struct {
	name string
	re   *regexp.Regexp
	// group is the submatch to mask; 0 masks the whole match. Used where the
	// pattern must match surrounding context to be sure of itself but should
	// only redact part of it (see "bearer" and "private_key").
	group int
	// validate is a Go-level check the regexp cannot express — currently only
	// the Luhn checksum, which is what makes the card rule safe to run
	// without a trigger.
	validate func(string) bool
}

// formatRules is the registry of every self-identifying rule. Config names
// which of these are switched on; New selects by walking this slice, so the
// order rules run in is fixed here and cannot be perturbed by the YAML.
var formatRules = []formatRule{
	{
		// 13–19 digits, optionally grouped by spaces or dashes, validated by
		// Luhn. The checksum is what keeps this from firing on ordinary long
		// numbers, and the 13-digit floor is why it can never collide with a
		// 4- or 6-digit PIN.
		name:     "card",
		re:       regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`),
		validate: luhnValid,
	},
	{
		// A JWT is three base64url segments separated by dots, and the header
		// almost always starts `eyJ` ({"...). Distinctive enough to need no
		// trigger.
		name: "jwt",
		re:   regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	},
	{
		// Vendor-prefixed API keys: OpenAI/Anthropic sk-, GitHub gh?_,
		// Google AIza, Slack xox?-. Each prefix is itself the evidence.
		name: "api_key",
		re: regexp.MustCompile(`\b(?:sk-(?:ant-)?[A-Za-z0-9_-]{16,}` +
			`|gh[pousr]_[A-Za-z0-9]{20,}` +
			`|AIza[A-Za-z0-9_-]{30,}` +
			`|xox[baprs]-[A-Za-z0-9-]{10,})`),
	},
	{
		// Telegram bot tokens: <bot id>:<35-char secret>. Shaped tightly
		// enough not to collide with a clock time or a ratio.
		name: "telegram_token",
		re:   regexp.MustCompile(`\b\d{8,10}:[A-Za-z0-9_-]{35}\b`),
	},
	{
		// Masks the credential, not the "Bearer " label, so a redacted trace
		// still shows that an Authorization header was present.
		name:  "bearer",
		re:    regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/=-]{12,})`),
		group: 1,
	},
	{
		// Masks the body between the PEM markers and leaves the markers
		// legible, so llm.log still says a private key was there.
		name:  "private_key",
		re:    regexp.MustCompile(`(?s)(-----BEGIN [A-Z ]*PRIVATE KEY-----)(.*?)(-----END [A-Z ]*PRIVATE KEY-----)`),
		group: 2,
	},
	{
		// СНИЛС — ###-###-### ##.
		name: "snils",
		re:   regexp.MustCompile(`\b\d{3}-\d{3}-\d{3}[ -]\d{2}\b`),
	},
	{
		// IBAN — country code, check digits, then up to 30 alphanumerics.
		name: "iban",
		re:   regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b`),
	},
}

// formatRuleNames lists the registry in order, for the error New returns on
// an unknown name.
func formatRuleNames() []string {
	names := make([]string, 0, len(formatRules))
	for _, r := range formatRules {
		names = append(names, r.name)
	}
	return names
}

// hasDigitAndLetter reports whether s mixes digits and letters. A bare word
// ("телефона") is not a secret and a bare number is already covered by the
// digits rule, so requiring both is what keeps the broad token pattern from
// masking ordinary prose.
func hasDigitAndLetter(s string) bool {
	var digit, letter bool
	for _, r := range s {
		switch {
		case unicode.IsDigit(r):
			digit = true
		case unicode.IsLetter(r):
			letter = true
		}
		if digit && letter {
			return true
		}
	}
	return false
}

// notCyrillicWord rejects a candidate made of nothing but Cyrillic letters.
// A password or code is essentially never a plain Russian word, and this is
// what keeps the permissive separator rule from masking ordinary prose when a
// trigger happens to be followed by a dash ("код — тот же, что и раньше").
func notCyrillicWord(s string) bool {
	for _, r := range s {
		if !unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

// luhnValid runs the Luhn checksum over the digits in s, ignoring the spaces
// and dashes people write card numbers with. It is what lets the card rule
// fire without a trigger word: a random 16-digit number passes only one time
// in ten.
func luhnValid(s string) bool {
	sum, digits := 0, 0
	double := false
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c < '0' || c > '9' {
			continue
		}
		d := int(c - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
		digits++
	}
	return digits >= 13 && digits <= 19 && sum%10 == 0
}
