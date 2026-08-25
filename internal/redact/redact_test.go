package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

// testConfig is a deliberately small lexicon. These tests assert the engine's
// semantics — word boundaries, windowing, candidate ranking, idempotence — not
// which words Miranda happens to ship. The shipped lexicon is exercised
// against the real examples in internal/config's own tests, where it lives.
func testConfig() Config {
	return Config{
		Enabled:       true,
		MaxMaskLength: defaultMaxMaskLength,
		WindowRunes:   defaultWindowRunes,
		Triggers: []string{
			`пин[-\s]?код[\p{L}]*`,
			`пин`,
			`код[\p{L}]*`,
			`пароль[\p{L}]*`,
			`password`,
			`cvv`,
		},
		TriggerExclusions: []string{
			`штрих[-\s]?код[\p{L}]*`,
			`код[\p{L}]*\s+ошибки`,
		},
		Formats: []string{"card", "jwt", "api_key", "telegram_token", "bearer", "private_key", "snils", "iban"},
	}
}

func newTestRedactor(t *testing.T) *Redactor {
	t.Helper()
	r, err := New(testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestRedact(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// The motivating example: a bare six-digit number that only a
		// trigger-anchored rule can find.
		{
			name: "pin from the original request",
			in:   "пин-код от телефона Ани 665533",
			want: "пин-код от телефона Ани ******",
		},

		// Russian morphology, handled by the lexicon's [\p{L}]* suffix
		// rather than a stemmer.
		{name: "no hyphen", in: "пинкод 1234", want: "пинкод ****"},
		{name: "space instead of hyphen", in: "пин код 1234", want: "пин код ****"},
		{name: "genitive case", in: "не помню пин-кода 4321", want: "не помню пин-кода ****"},
		{name: "instrumental case", in: "войти пин-кодом 4321", want: "войти пин-кодом ****"},
		{name: "uppercase", in: "ПИН-КОД 9999", want: "ПИН-КОД ****"},

		// Separator forms.
		{name: "colon then word", in: "пароль: hunter2", want: "пароль: *******"},
		{name: "equals then token", in: "password=abc123xyz", want: "password=*********"},
		{name: "dash then value", in: "код — 4455", want: "код — ****"},
		{name: "guillemets", in: "пароль «qwerty»", want: "пароль «******»"},
		{name: "cvv", in: "cvv 123", want: "cvv ***"},

		// Ranking: the fuller token beats the digit run inside it.
		{
			name: "token preferred over digits within it",
			in:   "пароль от wifi HomeNet2024",
			want: "пароль от wifi ***********",
		},

		// Only the first candidate after a trigger is masked, so one trigger
		// cannot swallow a sentence.
		{
			name: "only first value after trigger",
			in:   "пин-код 1234 и ещё позвони на 5556677",
			want: "пин-код **** и ещё позвони на 5556677",
		},

		// Word-boundary rejection at the *start* of a trigger: "код" inside
		// "промокод" is not a trigger.
		{name: "promo code is not a code", in: "промокод SALE2024", want: "промокод SALE2024"},

		// Word-boundary rejection at the *end*, which is what makes the bare
		// stem "пин" safe to ship: it must not fire inside "пингвин".
		{name: "penguin is not a pin", in: "пингвинов 500 грамм", want: "пингвинов 500 грамм"},
		{name: "bare pin", in: "пин 1234", want: "пин ****"},
		{name: "bare pin from card", in: "пин от карты 4321", want: "пин от карты ****"},
		// A digit may follow a trigger directly — that writes the value onto
		// the trigger rather than starting a new word.
		{name: "value glued to trigger", in: "пин1234", want: "пин****"},
		{name: "cvv glued", in: "cvv123", want: "cvv***"},

		// Exclusion list, for compounds the boundary check cannot catch
		// because a hyphen is not a letter.
		{name: "barcode excluded", in: "штрих-код 4600051000057", want: "штрих-код 4600051000057"},
		{name: "error code excluded", in: "код ошибки 500", want: "код ошибки 500"},

		// Negatives: no trigger, so nothing is touched. These are the
		// false-positive cases the whole anchored design exists to avoid.
		{name: "age", in: "мне 45 лет", want: "мне 45 лет"},
		{name: "grams", in: "купи 500 грамм", want: "купи 500 грамм"},
		{name: "time", in: "встреча в 18:00", want: "встреча в 18:00"},
		{name: "plain sentence", in: "включи свет в зале", want: "включи свет в зале"},

		// A trigger with nothing value-shaped nearby stays untouched.
		{name: "trigger without value", in: "я забыл пароль", want: "я забыл пароль"},
		{name: "two digit time after trigger", in: "код придёт в 18:30", want: "код придёт в 18:30"},
		{name: "cyrillic word after dash", in: "код — тот же", want: "код — тот же"},

		// The window bounds how far a value may sit from its trigger.
		{
			name: "value beyond the window is not claimed",
			in:   "пароль " + strings.Repeat("абвгд ", 9) + "123456",
			want: "пароль " + strings.Repeat("абвгд ", 9) + "123456",
		},

		// Format rules: no trigger required.
		{
			name: "luhn valid card",
			in:   "карта 4111 1111 1111 1111 оплачена",
			want: "карта ******************* оплачена",
		},
		{
			name: "invalid checksum is not a card",
			in:   "заказ 4111 1111 1111 1112 готов",
			want: "заказ 4111 1111 1111 1112 готов",
		},
		{
			name: "jwt",
			in:   "token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
			want: "token " + strings.Repeat("*", 32),
		},
		{name: "anthropic key", in: "ключ sk-ant-api03-abcdefghijklmnop", want: "ключ " + strings.Repeat("*", 29)},
		{name: "github token", in: "ghp_abcdefghijklmnopqrstuvwxyz0123", want: strings.Repeat("*", 32)},
		{
			name: "bearer keeps its label",
			in:   "Authorization: Bearer abcdef123456ghijkl",
			want: "Authorization: Bearer ******************",
		},
		{name: "snils", in: "СНИЛС 112-233-445 95", want: "СНИЛС " + strings.Repeat("*", 14)},

		// Nothing to do.
		{name: "empty", in: "", want: ""},
	}

	r := newTestRedactor(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.Redact(tt.in); got != tt.want {
				t.Errorf("Redact(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestRedact_PrivateKeyKeepsMarkers — a redacted PEM block should still
// announce itself in llm.log, so an operator can see a key was present.
func TestRedact_PrivateKeyKeepsMarkers(t *testing.T) {
	r := newTestRedactor(t)
	in := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\n-----END RSA PRIVATE KEY-----"

	got := r.Redact(in)
	if !strings.Contains(got, "-----BEGIN RSA PRIVATE KEY-----") ||
		!strings.Contains(got, "-----END RSA PRIVATE KEY-----") {
		t.Errorf("markers were masked away: %q", got)
	}
	if strings.Contains(got, "MIIEowIBAAKCAQEA1234") {
		t.Errorf("key body survived: %q", got)
	}
}

// TestRedact_Idempotent — text that reaches two sinks (a restored
// conversation is copied row to row) must not be progressively mangled.
func TestRedact_Idempotent(t *testing.T) {
	r := newTestRedactor(t)
	inputs := []string{
		"пин-код от телефона Ани 665533",
		"пароль: hunter2",
		"карта 4111 1111 1111 1111",
		"пароль «qwerty» и код 4455",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\n-----END RSA PRIVATE KEY-----",
	}
	for _, in := range inputs {
		once := r.Redact(in)
		if twice := r.Redact(once); twice != once {
			t.Errorf("not idempotent for %q:\n once %q\ntwice %q", in, once, twice)
		}
	}
}

// TestRedact_Deterministic — the user asked for a deterministic mechanism, so
// prove it: the same input must never produce two different outputs. Map
// iteration order is the trap this guards against.
func TestRedact_Deterministic(t *testing.T) {
	r := newTestRedactor(t)
	in := "пин-код 1234, пароль: hunter2, карта 4111 1111 1111 1111, cvv 999"

	want := r.Redact(in)
	for i := 0; i < 1000; i++ {
		if got := r.Redact(in); got != want {
			t.Fatalf("run %d differed:\n got %q\nwant %q", i, got, want)
		}
	}
}

// TestRedact_KeepsJSONValid — the llm.log path traces marshalled JSON, so
// masking must never be able to break it. This is the structural guarantee
// the "no quote, no backslash in a span" invariant in rules.go buys.
func TestRedact_KeepsJSONValid(t *testing.T) {
	r := newTestRedactor(t)

	type message struct {
		Role     string `json:"role"`
		Content  string `json:"content"`
		Password string `json:"password"`
	}
	payload, err := json.MarshalIndent([]message{
		{Role: "user", Content: `пин-код от телефона Ани 665533`},
		{Role: "user", Content: `он сказал "пароль: hunter2" вчера`},
		{Role: "user", Content: "путь C:\\Users\\аня и карта 4111 1111 1111 1111"},
		{Role: "assistant", Content: `записала, пин-код 665533`, Password: "s3cret-value"},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	masked := r.Redact(string(payload))

	if !json.Valid([]byte(masked)) {
		t.Fatalf("masking produced invalid JSON:\n%s", masked)
	}
	if strings.Contains(masked, "665533") {
		t.Errorf("pin survived in JSON dump:\n%s", masked)
	}
	if strings.Contains(masked, "hunter2") {
		t.Errorf("password survived in JSON dump:\n%s", masked)
	}
	if strings.Contains(masked, "s3cret-value") {
		t.Errorf("json field value survived:\n%s", masked)
	}
	if strings.Contains(masked, "4111 1111 1111 1111") {
		t.Errorf("card survived in JSON dump:\n%s", masked)
	}
}

// TestRedact_FindingsReportSpansNotValues — a Finding is meant to be safe to
// log, which is the only reason it exists.
func TestRedact_FindingsReportSpansNotValues(t *testing.T) {
	r := newTestRedactor(t)
	in := "пин-код от телефона Ани 665533"

	masked, findings := r.RedactWithFindings(in)
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Rule != "digits" {
		t.Errorf("Rule = %q, want %q", f.Rule, "digits")
	}
	if in[f.Start:f.End] != "665533" {
		t.Errorf("span %d:%d covers %q, want %q", f.Start, f.End, in[f.Start:f.End], "665533")
	}
	if masked != "пин-код от телефона Ани ******" {
		t.Errorf("masked = %q", masked)
	}
}

// TestRedact_MaskLengthCapped — a long secret must not become a wall of
// asterisks, and the cap is also what keeps long values idempotent.
func TestRedact_MaskLengthCapped(t *testing.T) {
	cfg := testConfig()
	cfg.MaxMaskLength = 8
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := r.Redact("пароль: " + strings.Repeat("a1", 40))
	if want := "пароль: " + strings.Repeat("*", 8); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestNew_DisabledYieldsWorkingNoop — cmd/miranda wires the result in
// unconditionally, so a disabled config must produce a usable pass-through
// rather than a nil that panics at the first call.
func TestNew_DisabledYieldsWorkingNoop(t *testing.T) {
	cfg := testConfig()
	cfg.Enabled = false

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r != nil {
		t.Fatalf("want nil Redactor when disabled, got %+v", r)
	}
	if got := r.Redact("пин-код 1234"); got != "пин-код 1234" {
		t.Errorf("nil Redactor should pass text through, got %q", got)
	}
	if _, findings := r.RedactWithFindings("пин-код 1234"); findings != nil {
		t.Errorf("nil Redactor should report no findings, got %+v", findings)
	}
}

// TestNew_RejectsBadConfigAtStartup — both of these should stop the process
// at boot rather than surface on the first message that would be masked.
func TestNew_RejectsBadConfigAtStartup(t *testing.T) {
	t.Run("unknown format rule", func(t *testing.T) {
		cfg := testConfig()
		cfg.Formats = append(cfg.Formats, "nonesuch")
		if _, err := New(cfg); err == nil {
			t.Fatal("want an error for an unknown format rule")
		} else if !strings.Contains(err.Error(), "nonesuch") {
			t.Errorf("error should name the offender: %v", err)
		}
	})

	t.Run("uncompilable extra pattern", func(t *testing.T) {
		cfg := testConfig()
		cfg.ExtraPatterns = []string{"([unterminated"}
		if _, err := New(cfg); err == nil {
			t.Fatal("want an error for an uncompilable pattern")
		}
	})

	t.Run("uncompilable trigger", func(t *testing.T) {
		cfg := testConfig()
		cfg.Triggers = []string{"("}
		if _, err := New(cfg); err == nil {
			t.Fatal("want an error for an uncompilable trigger")
		}
	})
}

// TestNew_ExtraPatterns — the escape hatch for a deployment-specific secret
// shape, masking group 1 when the author supplied one.
func TestNew_ExtraPatterns(t *testing.T) {
	cfg := testConfig()
	cfg.ExtraPatterns = []string{`ACME-(\d{6})`}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got, want := r.Redact("номер ACME-123456"), "номер ACME-******"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
