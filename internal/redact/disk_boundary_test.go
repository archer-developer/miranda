package redact_test

// The one test that checks the actual promise rather than a piece of it:
// drive every store Miranda writes to, with the *shipped* lexicon and the
// real wiring, then sweep every byte on disk for the secret.
//
// It lives in package redact_test (not redact) because it imports
// internal/history, internal/memory and internal/schedule — none of which
// internal/redact itself may depend on.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/memory"
	"github.com/archer-developer/miranda/internal/redact"
	"github.com/archer-developer/miranda/internal/schedule"
)

const (
	secret   = "665533"
	userText = "пин-код от телефона Ани 665533"
)

// fileTracer stands in for llmtrace.Logger — it does the same thing that
// matters here, which is writing whatever it is handed straight to a file.
type fileTracer struct{ path string }

func (f *fileTracer) Trace(_ context.Context, provider, request, response string, _ error) {
	fh, err := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = fh.Close() }()
	_, _ = fh.WriteString("=== provider=" + provider + " ===\n--- request ---\n" + request +
		"\n--- response ---\n" + response + "\n")
}

func TestSecretNeverReachesDisk(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	redactor, err := redact.New(redact.Config(config.Default().Redact))
	require.NoError(t, err)

	historyStore, err := history.Open(filepath.Join(dir, "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, historyStore.Close()) })
	historyStore.SetRedactor(redactor)

	memoryStore, err := memory.New(filepath.Join(dir, "memory"))
	require.NoError(t, err)
	memoryStore.SetRedactor(redactor)

	scheduleStore, err := schedule.Open(filepath.Join(dir, "schedule.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, scheduleStore.Close()) })
	scheduleStore.SetRedactor(redactor)

	llmLog := filepath.Join(dir, "llm.log")
	tracer := &redact.Tracer{Next: &fileTracer{path: llmLog}, Redactor: redactor}

	// One turn's worth of writes, in the order Orchestrator.Handle makes them.
	convID, err := historyStore.StartConversation(ctx, "alex", "ha_assist")
	require.NoError(t, err)

	_, err = historyStore.AppendMessage(ctx, convID, "user", userText)
	require.NoError(t, err)

	// The system prompt carries the memory file folded into it.
	require.NoError(t, historyStore.SetSystemPrompt(ctx, convID,
		"Ты Miranda.\n\n## Remembered\n- (2026-08-25) "+userText))

	msgID, err := historyStore.AppendAssistantMessage(ctx, convID,
		"записала, пин-код 665533",
		[]history.ToolCallRef{{ID: "c1", Name: "remember_this", Arguments: `{"fact":"пин-код Ани 665533"}`}},
		nil)
	require.NoError(t, err)

	_, err = historyStore.AppendToolResultMessage(ctx, convID, "c1",
		"запомнила: пин-код Ани 665533")
	require.NoError(t, err)
	require.NoError(t, historyStore.AppendToolCall(ctx, msgID, "remember_this", "",
		`{"fact":"пин-код Ани 665533"}`, `{"saved":"пин-код Ани 665533"}`))

	require.NoError(t, memoryStore.Remember("alex", userText))
	require.NoError(t, memoryStore.RememberShared("код от домофона 665533"))

	_, err = scheduleStore.Create(ctx, schedule.Task{
		UserID: "alex", Prompt: "напомни Ане пин-код 665533", CronExpr: "0 9 * * *",
	})
	require.NoError(t, err)

	// The provider dump, in the shape anthropic.Provider builds it.
	tracer.Trace(ctx, "anthropic",
		`{"model":"claude","messages":[{"role":"user","content":"`+userText+`"}]}`,
		`{"content":[{"type":"text","text":"записала, пин-код 665533"}]}`, nil)

	require.NoError(t, historyStore.EndConversationWithSummary(ctx, convID,
		"Alex сообщил пин-код от телефона Ани 665533."))

	// Sweep every byte written anywhere under the data directory. This is
	// deliberately a blind walk rather than a list of columns to check: a new
	// table or a new file that forgets to redact should fail this test
	// without anyone remembering to extend it.
	var hits []string
	require.NoError(t, filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is not what this test is about
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(b), secret) {
			hits = append(hits, strings.TrimPrefix(path, dir))
		}
		return nil
	}))
	require.Emptyf(t, hits, "the raw value %q reached disk", secret)

	// And prove this masked rather than dropped: the conversation must still
	// be readable and searchable by everything around the secret.
	messages, err := historyStore.ConversationMessages(ctx, convID)
	require.NoError(t, err)
	require.Equal(t, "пин-код от телефона Ани ******", messages[0].Content)

	found, err := historyStore.SearchMessages(ctx, "alex", "телефона", 10)
	require.NoError(t, err)
	require.NotEmpty(t, found, "surrounding text must stay searchable")

	mem, err := memoryStore.Read("alex")
	require.NoError(t, err)
	require.Contains(t, mem, "пин-код от телефона Ани ******")
}

// TestUntriggeredValueIsNotMasked documents the accepted limit of anchored
// detection, so that nobody reads the test above as a stronger promise than
// it is. A bare number with no trigger word and no self-identifying format is
// indistinguishable from "мне 45 лет", and is left alone by design — the
// alternative is masking every number a household assistant ever hears.
//
// In practice the text around a secret carries the trigger (a tool result
// echoes the fact it just saved, as in the test above), which is why the
// sweep passes on realistic data.
func TestUntriggeredValueIsNotMasked(t *testing.T) {
	redactor, err := redact.New(redact.Config(config.Default().Redact))
	require.NoError(t, err)

	require.Equal(t, `{"ok":"665533"}`, redactor.Redact(`{"ok":"665533"}`),
		"a bare number with no trigger nearby is not detectable deterministically")
}
