// Package redact masks sensitive values out of text before that text is
// written anywhere durable — the SQLite dialog log, the markdown memory
// files, the scheduled-task prompts, and the llm.log / anomalies trace files.
//
// The boundary this package defends is *the disk*, not the network: a turn's
// in-flight messages still carry the user's original words to the model, so
// the assistant can act on a secret it was just told. Only the copies that
// outlive the turn are masked. The consequence is deliberate — on the next
// turn the conversation replays from SQLite, so the assistant reads the
// masked text and no longer knows the secret. Nothing here is reversible;
// there is no vault and no un-masking path anywhere in Miranda.
//
// Detection is deterministic by construction — no model call, no sampling,
// no map iteration. The same input always produces the same output, and
// Redact is idempotent, so text that passes through two sinks (e.g. a
// restored conversation copied row-to-row) is not progressively mangled.
//
// Two rule families do the work:
//
//   - Anchored rules: a trigger word ("пин-код", "password", "cvv") followed,
//     within a short window, by something value-shaped. This is what catches
//     "пин-код от телефона Ани 665533" — a bare six-digit number that no
//     standalone pattern could flag without also flagging "мне 45 лет".
//     The trigger lexicon is configuration, not code (see config.RedactConfig).
//
//   - Format rules: values that identify themselves by shape alone and need
//     no trigger — a Luhn-valid card number, a JWT, an API key, a PEM private
//     key block. These live in this package as code (regexp plus, where the
//     shape isn't sufficient, a Go-level validator); configuration only names
//     which of them are switched on.
package redact

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Config is the redactor's settings. It is deliberately shape-identical to
// config.RedactConfig so cmd/miranda can convert one into the other with a
// plain Go struct conversion rather than a field-by-field mapper — the same
// mirroring convention internal/config already uses for miranda-llm's option
// structs (see internal/config/CLAUDE.md).
//
// Triggers and TriggerExclusions are regexp fragments, not literals, so the
// lexicon can spell Russian morphology directly (`пин[-\s]?код[\p{L}]*`
// covers пинкод / пин-код / пин-кода / пин-кодом without a stemmer).
type Config struct {
	Enabled           bool
	MaxMaskLength     int
	WindowRunes       int
	Triggers          []string
	TriggerExclusions []string
	Formats           []string
	ExtraPatterns     []string
}

// Finding is one masked span, reported for observability only. Start and End
// are byte offsets into the *original* text, and the value itself is
// deliberately absent — a Finding is safe to log, which is the whole point of
// having it (over-masking is otherwise invisible).
type Finding struct {
	Rule       string
	Start, End int
}

// Redactor is a compiled rule set. Build one with New; it is immutable and
// safe for concurrent use afterwards. A nil *Redactor is valid and passes
// text through untouched, so callers can treat redaction as an optional
// dependency without nil checks at every call site — the same trick
// llmtrace.Logger uses for an optional tracer.
type Redactor struct {
	maxMask    int
	window     int
	triggers   *regexp.Regexp
	exclusions *regexp.Regexp
	formats    []formatRule
}

// New compiles cfg into a Redactor. It returns a nil *Redactor (a working
// no-op, not an error) when cfg.Enabled is false, so cmd/miranda can wire the
// result in unconditionally. An unknown name in cfg.Formats or an
// uncompilable pattern is an error: both should stop the process at startup
// rather than surface on the first message that would have been masked.
func New(cfg Config) (*Redactor, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	r := &Redactor{maxMask: cfg.MaxMaskLength, window: cfg.WindowRunes}
	if r.maxMask <= 0 {
		r.maxMask = defaultMaxMaskLength
	}
	if r.window <= 0 {
		r.window = defaultWindowRunes
	}

	var err error
	if r.triggers, err = compileAlternation("triggers", cfg.Triggers); err != nil {
		return nil, err
	}
	if r.exclusions, err = compileAlternation("trigger_exclusions", cfg.TriggerExclusions); err != nil {
		return nil, err
	}

	// Selected by walking the registry, not cfg.Formats, so the order rules
	// run in is fixed by this package and cannot be perturbed by how the
	// YAML happens to list them — one of several places determinism is a
	// design constraint rather than a happy accident.
	wanted := make(map[string]bool, len(cfg.Formats))
	for _, name := range cfg.Formats {
		wanted[name] = true
	}
	for _, rule := range formatRules {
		if wanted[rule.name] {
			r.formats = append(r.formats, rule)
			delete(wanted, rule.name)
		}
	}
	// Whatever is left named nothing real. Sorted so the error message is
	// itself deterministic — map iteration order is not.
	if len(wanted) > 0 {
		unknown := make([]string, 0, len(wanted))
		for name := range wanted {
			unknown = append(unknown, name)
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("redact: unknown format rule(s) %s (known: %s)",
			strings.Join(unknown, ", "), strings.Join(formatRuleNames(), ", "))
	}

	for i, pattern := range cfg.ExtraPatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("redact: extra_patterns[%d] %q: %w", i, pattern, err)
		}
		// Group 1 when the author supplied one (so a pattern can match
		// context but mask only part of it), the whole match otherwise.
		group := 0
		if re.NumSubexp() >= 1 {
			group = 1
		}
		r.formats = append(r.formats, formatRule{
			name:  fmt.Sprintf("custom_%d", i),
			re:    re,
			group: group,
		})
	}

	return r, nil
}

// compileAlternation joins regexp fragments into one case-insensitive
// alternation. Case folding here is Unicode-aware (Go's RE2 (?i) folds
// Cyrillic just as it folds ASCII), so "ПИН-КОДА" matches the same fragment
// as "пин-кода". Returns nil for an empty list, which callers treat as
// "never matches".
func compileAlternation(field string, fragments []string) (*regexp.Regexp, error) {
	if len(fragments) == 0 {
		return nil, nil
	}
	re, err := regexp.Compile("(?i)(?:" + strings.Join(fragments, "|") + ")")
	if err != nil {
		return nil, fmt.Errorf("redact: %s: %w", field, err)
	}
	return re, nil
}

// Redact returns text with every detected secret replaced by a mask.
func (r *Redactor) Redact(text string) string {
	masked, _ := r.RedactWithFindings(text)
	return masked
}

// RedactWithFindings is Redact plus the spans it masked, for callers that
// want to log how much was redacted (never what).
func (r *Redactor) RedactWithFindings(text string) (string, []Finding) {
	if r == nil || text == "" {
		return text, nil
	}
	findings := r.find(text)
	if len(findings) == 0 {
		return text, nil
	}
	return r.apply(text, findings), findings
}

// find collects every span to mask, already de-overlapped and in ascending
// order.
func (r *Redactor) find(text string) []Finding {
	var candidates []Finding
	candidates = append(candidates, r.findFormats(text)...)
	candidates = append(candidates, r.findAnchored(text)...)
	return resolveOverlaps(candidates)
}

// findFormats runs the self-identifying rules — no trigger needed, the shape
// is the evidence.
func (r *Redactor) findFormats(text string) []Finding {
	var out []Finding
	for _, rule := range r.formats {
		for _, m := range rule.re.FindAllStringSubmatchIndex(text, -1) {
			start, end := m[2*rule.group], m[2*rule.group+1]
			if start < 0 || end <= start {
				continue
			}
			if rule.validate != nil && !rule.validate(text[start:end]) {
				continue
			}
			if isAllMask(text[start:end]) {
				continue
			}
			out = append(out, Finding{Rule: rule.name, Start: start, End: end})
		}
	}
	return out
}

// findAnchored runs the trigger-plus-nearby-value rules: for each trigger
// occurrence, look a short way ahead for the first thing that looks like a
// value and mask that, and only that. Masking stops at the first candidate so
// a single trigger can never swallow the rest of a sentence.
func (r *Redactor) findAnchored(text string) []Finding {
	if r.triggers == nil {
		return nil
	}

	var out []Finding
	for _, m := range r.triggers.FindAllStringIndex(text, -1) {
		start, end := m[0], m[1]

		// Reject a trigger that is really the tail of a longer word.
		// "промокод" ends in "код" and RE2 would happily match there; Go's
		// \b is ASCII-only and reports no boundary between two Cyrillic
		// runes, so it cannot express this — hence the explicit check.
		if !startsWord(text, start) || !endsWord(text, end) {
			continue
		}
		// Hyphenated compounds ("штрих-код", "QR-код") clear the boundary
		// check above, since "-" is not a letter. Those, and phrases like
		// "код ошибки", are what the exclusion list is for.
		if r.excluded(text, start, end) {
			continue
		}

		if f, ok := r.valueAfter(text, end); ok {
			out = append(out, f)
		}
	}
	return out
}

// excluded reports whether the trigger at [start,end) falls inside a span the
// exclusion list claims.
func (r *Redactor) excluded(text string, start, end int) bool {
	if r.exclusions == nil {
		return false
	}
	for _, e := range r.exclusions.FindAllStringIndex(text, -1) {
		if start < e[1] && e[0] < end {
			return true
		}
	}
	return false
}

// valueAfter scans the window following a trigger and returns the first
// value-shaped run in it. Candidates are ranked leftmost-first, then longest,
// then by the fixed valueRules order — so when "пароль qwerty123" offers both
// "qwerty123" (token, at offset 0) and "123" (digits, at offset 6), the
// fuller token wins, and the ranking never depends on map order.
func (r *Redactor) valueAfter(text string, from int) (Finding, bool) {
	// The window bounds where a value may *begin*, not how long it may be —
	// otherwise a long API key written just after "пароль:" would be masked
	// only up to the window edge and leak its tail. Searching a little past
	// the window and then rejecting late starts keeps the work bounded while
	// letting a value run to its natural end.
	startBound := windowEnd(text, from, r.window)
	search := text[from:windowEnd(text, from, r.window+maxValueRunes)]

	// One candidate per value rule, carrying the rule's registry index so the
	// ranking below never has to consult a map.
	type candidate struct {
		rule       int
		start, end int
	}
	var candidates []candidate
	for i, rule := range valueRules {
		m := rule.re.FindStringSubmatchIndex(search)
		if m == nil {
			continue
		}
		start, end := m[2*rule.group], m[2*rule.group+1]
		if start < 0 || end <= start {
			continue
		}
		if from+start >= startBound {
			continue
		}
		candidates = append(candidates, candidate{rule: i, start: from + start, end: from + end})
	}
	if len(candidates) == 0 {
		return Finding{}, false
	}

	sort.SliceStable(candidates, func(a, b int) bool {
		x, y := candidates[a], candidates[b]
		if x.start != y.start {
			return x.start < y.start
		}
		if x.end != y.end {
			return x.end > y.end // longer first
		}
		return x.rule < y.rule
	})

	for _, c := range candidates {
		rule := valueRules[c.rule]
		value := text[c.start:c.end]
		if rule.validate != nil && !rule.validate(value) {
			continue
		}
		// A clock time reads as a digit run but almost never is a secret —
		// "пришли код в 18:30" should keep its time.
		if timeLikeRE.MatchString(text[c.start:]) {
			continue
		}
		if isAllMask(value) {
			continue
		}
		return Finding{Rule: rule.name, Start: c.start, End: c.end}, true
	}
	return Finding{}, false
}

// startsWord reports whether the byte offset i begins a word — i.e. the rune
// before it is not a letter or digit. This is the Unicode-aware replacement
// for a leading \b, which Go's ASCII-only word-boundary cannot provide for
// Cyrillic text.
func startsWord(text string, i int) bool {
	if i == 0 {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(text[:i])
	return !unicode.IsLetter(prev) && !unicode.IsDigit(prev)
}

// endsWord reports whether the byte offset i ends a word — i.e. the rune at
// it is not a letter. This is what makes a bare stem safe to put in the
// lexicon: without it, "пин" would fire inside "пингвин" and mask whatever
// number came next.
//
// A following *digit* is deliberately allowed, unlike in startsWord: "пин1234"
// and "cvv123" write the value straight onto the trigger, and those should
// still be caught.
func endsWord(text string, i int) bool {
	if i >= len(text) {
		return true
	}
	next, _ := utf8.DecodeRuneInString(text[i:])
	return !unicode.IsLetter(next)
}

// windowEnd returns the byte offset `runes` runes past `from`, clamped to the
// end of text. Counted in runes rather than bytes so the window is the same
// size in Russian as in English.
func windowEnd(text string, from, runes int) int {
	i := from
	for n := 0; n < runes && i < len(text); n++ {
		_, size := utf8.DecodeRuneInString(text[i:])
		i += size
	}
	return i
}

// resolveOverlaps sorts candidates into a stable order and greedily keeps the
// non-overlapping ones, leftmost-longest first. Two rules flagging the same
// text produce the same mask either way, so which label survives matters only
// to the Finding log.
func resolveOverlaps(candidates []Finding) []Finding {
	if len(candidates) <= 1 {
		return candidates
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		x, y := candidates[a], candidates[b]
		if x.Start != y.Start {
			return x.Start < y.Start
		}
		if x.End != y.End {
			return x.End > y.End
		}
		return x.Rule < y.Rule
	})

	out := candidates[:0:0]
	prevEnd := -1
	for _, c := range candidates {
		if c.Start < prevEnd {
			continue
		}
		out = append(out, c)
		prevEnd = c.End
	}
	return out
}

// apply rebuilds text with each span replaced by its mask, in one pass.
func (r *Redactor) apply(text string, findings []Finding) string {
	var b strings.Builder
	b.Grow(len(text))
	prev := 0
	for _, f := range findings {
		b.WriteString(text[prev:f.Start])
		b.WriteString(r.mask(text[f.Start:f.End]))
		prev = f.End
	}
	b.WriteString(text[prev:])
	return b.String()
}

// mask returns the replacement for one value: asterisks matching the value's
// rune length, so "665533" becomes "******" and the surrounding sentence
// still reads naturally. Capped at maxMask so a masked PEM block does not
// become a wall of stars — and, because masking an already-masked run of
// asterisks yields the same capped run, the cap is what keeps Redact
// idempotent for long values.
func (r *Redactor) mask(value string) string {
	n := utf8.RuneCountInString(value)
	if n > r.maxMask {
		n = r.maxMask
	}
	return strings.Repeat(maskChar, n)
}

// isAllMask reports whether s is already nothing but mask characters. Used to
// skip re-masking, which keeps Redact idempotent and keeps Findings honest
// about how much was actually redacted this pass.
func isAllMask(s string) bool {
	return s != "" && strings.Trim(s, maskChar) == ""
}
