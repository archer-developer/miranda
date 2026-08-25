package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/redact"
)

// The redaction engine's own semantics are tested in internal/redact against
// small purpose-built fixtures. What is tested here is the *shipped lexicon* —
// the trigger list lives in Default(), so this is where "does the list we
// actually ship catch the things it is meant to, and leave alone the things it
// is not" belongs. internal/redact does not import this package, so there is
// no cycle.

func shippedRedactor(t *testing.T) *redact.Redactor {
	t.Helper()
	r, err := redact.New(redact.Config(Default().Redact))
	require.NoError(t, err)
	require.NotNil(t, r)
	return r
}

func TestDefault_RedactIsEnabled(t *testing.T) {
	cfg := Default().Redact
	require.True(t, cfg.Enabled, "redaction must be on by default")
	require.NotEmpty(t, cfg.Triggers)
	require.NotEmpty(t, cfg.Formats)
	require.Equal(t, 32, cfg.MaxMaskLength)
	require.Equal(t, 40, cfg.WindowRunes)
}

// TestDefault_RedactConfigCompiles guards the one way this config can be
// wrong that a type checker cannot catch: a typo'd format name or an
// uncompilable regexp fragment in the shipped lexicon. redact.New rejects
// both, and cmd/miranda calls it during startup, so this test is the same
// check the process performs at boot.
func TestDefault_RedactConfigCompiles(t *testing.T) {
	shippedRedactor(t)
}

func TestDefault_RedactMasksSecrets(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "the original request's example",
			in:   "пин-код от телефона Ани 665533",
			want: "пин-код от телефона Ани ******",
		},
		{name: "password with colon", in: "пароль: hunter2", want: "пароль: *******"},
		{name: "code", in: "код от домофона 4455", want: "код от домофона ****"},
		{name: "cvv", in: "cvv 123", want: "cvv ***"},
		{name: "english password", in: "the password is s3cret99", want: "the password is ********"},
		{name: "api key phrase", in: "api key abc123def456", want: "api key ************"},
		{name: "passport", in: "паспорт 4509 123456", want: "паспорт **** 123456"},
		{
			name: "card needs no trigger",
			in:   "оплати картой 4111 1111 1111 1111",
			want: "оплати картой *******************",
		},
		{
			// 34 characters masked by 32 asterisks — the whole span is
			// replaced, MaxMaskLength only bounds how many stars stand in
			// for it.
			name: "github token needs no trigger",
			in:   "ghp_abcdefghijklmnopqrstuvwxyz0123",
			want: "********************************",
		},
	}

	r := shippedRedactor(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, r.Redact(tt.in))
		})
	}
}

// TestDefault_RedactLeavesOrdinaryTextAlone is the important half. The
// lexicon is broad — "код" alone is a trigger — so over-masking is the real
// risk of shipping this on by default, and these are the phrases a household
// assistant actually hears.
func TestDefault_RedactLeavesOrdinaryTextAlone(t *testing.T) {
	unchanged := []string{
		"мне 45 лет",
		"купи 500 грамм сыра",
		"встреча в 18:00",
		"включи свет в зале",
		"напомни купить молоко завтра в 9:30",
		"промокод SALE2024 на скидку",
		"штрих-код 4600051000057",
		"код ошибки 500",
		"поставь будильник на 7 утра",
		"сколько будет 128 умножить на 64",
		"какая погода в Минске",
		"добавь в календарь встречу на 15 марта",
	}

	r := shippedRedactor(t)
	for _, in := range unchanged {
		t.Run(in, func(t *testing.T) {
			require.Equal(t, in, r.Redact(in), "ordinary text must not be masked")
		})
	}
}

func TestLoad_RedactOverrideKeepsOtherDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlContent := `
redact:
  window_runes: 12
  extra_patterns:
    - "ACME-(\\d{6})"
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)

	require.Equal(t, 12, cfg.Redact.WindowRunes)
	require.Equal(t, []string{`ACME-(\d{6})`}, cfg.Redact.ExtraPatterns)

	// Untouched siblings keep their defaults — including the lexicon, which
	// a partial override must not silently empty.
	require.True(t, cfg.Redact.Enabled)
	require.Equal(t, 32, cfg.Redact.MaxMaskLength)
	require.Equal(t, Default().Redact.Triggers, cfg.Redact.Triggers)
	require.Equal(t, Default().Redact.Formats, cfg.Redact.Formats)

	r, err := redact.New(redact.Config(cfg.Redact))
	require.NoError(t, err)
	require.Equal(t, "номер ACME-******", r.Redact("номер ACME-123456"))
}

// TestLoad_RedactCanBeDisabled — the escape hatch, and proof that a disabled
// config still yields a usable pass-through rather than something that panics.
func TestLoad_RedactCanBeDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("redact:\n  enabled: false\n"), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.False(t, cfg.Redact.Enabled)

	r, err := redact.New(redact.Config(cfg.Redact))
	require.NoError(t, err)
	require.Nil(t, r)
	require.Equal(t, "пин-код 1234", r.Redact("пин-код 1234"))
}
