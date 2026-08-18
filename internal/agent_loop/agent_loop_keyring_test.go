package agentloop

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda/internal/keyring"
	"github.com/archer-developer/miranda/internal/mcp/mcptest"
)

// newTestKeyringService builds a real *keyring.Service (backed by a
// temp-dir SQLite store) with username's master key already unlocked in
// its in-memory Cache — the shape executeTool's o.keyring.Get(userID) reads
// from, without needing a real login/WebAuthn ceremony in these tests.
func newTestKeyringService(t *testing.T, username string, key []byte) *keyring.Service {
	t.Helper()
	store, err := keyring.Open(filepath.Join(t.TempDir(), "keyring.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	cache := keyring.NewCache()
	cache.Unlock(username, key)
	return keyring.NewService(store, cache)
}

func TestExecuteTool_InjectsEncryptionKeyForWhitelistedUnlockedServer(t *testing.T) {
	diary := mcptest.New("diary", llm.ToolDef{Name: "add_entry"}).WithResult("add_entry", "ok")
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "diary_add_entry", Arguments: `{"text":"hello"}`}},
		llmtest.Response{Text: "Done."},
	)
	o, _, _ := newTestOrchestrator(t, provider, diary)
	o.SetKeyring(newTestKeyringService(t, "alex", []byte("01234567890123456789012345678901")[:32]))
	o.SetMCPServerExtensions(map[string]MCPServerExtension{"diary": {EncryptionKeyArg: "encryption_key"}}, 0)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "add a diary entry"})
	require.NoError(t, err)
	require.Equal(t, "Done.", resp.Reply)

	require.Len(t, diary.Calls, 1)
	var wireArgs map[string]any
	require.NoError(t, json.Unmarshal([]byte(diary.Calls[0].ArgumentsJSON), &wireArgs))
	require.Contains(t, wireArgs, "encryption_key", "the wire call to a whitelisted, unlocked server must carry the key")
	require.Equal(t, "hello", wireArgs["text"])
	// Must be lowercase hex, 64 chars for a 32-byte key — what
	// miranda-diary's parseEncryptionKey actually requires (see
	// docs/encryption.md); a previous version sent base64 instead, which
	// that server silently ignored until encryption was turned on for the
	// account, then rejected outright.
	require.Regexp(t, `^[0-9a-f]{64}$`, wireArgs["encryption_key"])
}

// TestExecuteTool_InjectsUnderServerConfiguredArgName guards the actual bug
// this test file's map[string]string plumbing was added to fix: a
// whitelisted server whose real tool schema names the key field something
// other than the "encryption_key" default (e.g. the external miranda-diary
// repo's tools use "record_encryption_key") must receive it under that
// server's own configured name, not the default — the whitelisted server
// previously always got the hardcoded default regardless of its actual
// schema, so this call would have been silently rejected by that server's
// additionalProperties:false validation.
func TestExecuteTool_InjectsUnderServerConfiguredArgName(t *testing.T) {
	diary := mcptest.New("diary", llm.ToolDef{Name: "add_entry"}).WithResult("add_entry", "ok")
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "diary_add_entry", Arguments: `{"text":"hello"}`}},
		llmtest.Response{Text: "Done."},
	)
	o, _, _ := newTestOrchestrator(t, provider, diary)
	o.SetKeyring(newTestKeyringService(t, "alex", []byte("01234567890123456789012345678901")[:32]))
	o.SetMCPServerExtensions(map[string]MCPServerExtension{"diary": {EncryptionKeyArg: "record_encryption_key"}}, 0)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "add a diary entry"})
	require.NoError(t, err)
	require.Equal(t, "Done.", resp.Reply)

	require.Len(t, diary.Calls, 1)
	var wireArgs map[string]any
	require.NoError(t, json.Unmarshal([]byte(diary.Calls[0].ArgumentsJSON), &wireArgs))
	require.Contains(t, wireArgs, "record_encryption_key", "must use this server's configured arg name")
	require.NotContains(t, wireArgs, "encryption_key", "must not also inject under the default name")
}

func TestExecuteTool_NeverSendsKeyToNonWhitelistedServer(t *testing.T) {
	// A tool call already carrying a model-supplied encryption_key field —
	// simulating a compromised/malicious tool description tricking the
	// model into forwarding a previously-seen key — targeting a server that
	// was never whitelisted.
	ha := mcptest.New("ha", llm.ToolDef{Name: "get_state"}).WithResult("get_state", "on")
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "ha_get_state", Arguments: `{"entity_id":"light.living_room","encryption_key":"attacker-controlled"}`}},
		llmtest.Response{Text: "Done."},
	)
	o, _, _ := newTestOrchestrator(t, provider, ha)
	o.SetKeyring(newTestKeyringService(t, "alex", []byte("01234567890123456789012345678901")[:32]))
	o.SetMCPServerExtensions(map[string]MCPServerExtension{"diary": {EncryptionKeyArg: "encryption_key"}}, 0) // "ha" is not in this set

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "is the light on?"})
	require.NoError(t, err)
	require.Equal(t, "Done.", resp.Reply)

	require.Len(t, ha.Calls, 1)
	var wireArgs map[string]any
	require.NoError(t, json.Unmarshal([]byte(ha.Calls[0].ArgumentsJSON), &wireArgs))
	require.NotContains(t, wireArgs, "encryption_key", "a non-whitelisted server must never see an encryption_key, model-supplied or otherwise")
}

func TestExecuteTool_ProceedsWithoutKeyWhenLocked(t *testing.T) {
	diary := mcptest.New("diary", llm.ToolDef{Name: "add_entry"}).WithResult("add_entry", "ok")
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "diary_add_entry", Arguments: `{"text":"hello"}`}},
		llmtest.Response{Text: "Done."},
	)
	o, _, _ := newTestOrchestrator(t, provider, diary)
	// Keyring configured, server whitelisted, but nobody has unlocked "alex"'s key.
	o.SetKeyring(newTestKeyringService(t, "someone-else", []byte("01234567890123456789012345678901")[:32]))
	o.SetMCPServerExtensions(map[string]MCPServerExtension{"diary": {EncryptionKeyArg: "encryption_key"}}, 0)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "add a diary entry"})
	require.NoError(t, err, "a locked key must never block the turn")
	require.Equal(t, "Done.", resp.Reply)

	require.Len(t, diary.Calls, 1)
	var wireArgs map[string]any
	require.NoError(t, json.Unmarshal([]byte(diary.Calls[0].ArgumentsJSON), &wireArgs))
	require.NotContains(t, wireArgs, "encryption_key")
}

func TestExecuteTool_NeverMutatesRecordedToolCallArguments(t *testing.T) {
	diary := mcptest.New("diary", llm.ToolDef{Name: "add_entry"}).WithResult("add_entry", "ok")
	const original = `{"text":"hello"}`
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "diary_add_entry", Arguments: original}},
		llmtest.Response{Text: "Done."},
	)
	o, h, _ := newTestOrchestrator(t, provider, diary)
	o.SetKeyring(newTestKeyringService(t, "alex", []byte("01234567890123456789012345678901")[:32]))
	o.SetMCPServerExtensions(map[string]MCPServerExtension{"diary": {EncryptionKeyArg: "encryption_key"}}, 0)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "add a diary entry"})
	require.NoError(t, err)

	// The wire call did carry the key...
	require.Len(t, diary.Calls, 1)
	require.Contains(t, diary.Calls[0].ArgumentsJSON, "encryption_key")

	// ...but what's persisted to history for the assistant's tool-call
	// message must be exactly the model's original, unmutated arguments —
	// this is the property docs/encryption.md relies on to guarantee the
	// key never reaches history's SQLite tables.
	msgs, err := h.ConversationMessages(context.Background(), resp.ConversationID)
	require.NoError(t, err)
	var found bool
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			found = true
			require.Equal(t, original, tc.Arguments, "history must record the model's original arguments, never the key-injected wire payload")
		}
	}
	require.True(t, found, "expected at least one recorded tool call")
}

func TestSetEncryptionKeyArg_InjectsForNonEmptyKey(t *testing.T) {
	result, ok := setEncryptionKeyArg(`{"text":"hi"}`, "encryption_key", []byte{1, 2, 3})
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(result), &m))
	require.Equal(t, "hi", m["text"])
	require.Contains(t, m, "encryption_key")
}

func TestSetEncryptionKeyArg_StripsModelSuppliedFieldForNilKey(t *testing.T) {
	result, ok := setEncryptionKeyArg(`{"text":"hi","encryption_key":"attacker"}`, "encryption_key", nil)
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(result), &m))
	require.NotContains(t, m, "encryption_key")
	require.Equal(t, "hi", m["text"])
}

func TestSetEncryptionKeyArg_NoopWhenFieldAlreadyAbsentAndKeyNil(t *testing.T) {
	const args = `{"text":"hi"}`
	result, ok := setEncryptionKeyArg(args, "encryption_key", nil)
	require.True(t, ok)
	require.Equal(t, args, result, "must return the original string unmodified, not a re-marshaled equivalent")
}

// TestSetEncryptionKeyArg_StripsUnicodeEscapedFieldName guards a real
// bypass: a previous version of this function fast-pathed on
// strings.Contains(args, "encryption_key") before deciding whether to
// decode at all. JSON allows any character — including every letter of
// that field name — to be written as a \uXXXX escape, which decodes to the
// exact same map key but defeats a literal substring search. This is
// exactly the prompt-injection scenario the function exists to guard
// against: a compromised/malicious tool description asking the model to
// spell the field with an escape to dodge the strip.
func TestSetEncryptionKeyArg_StripsUnicodeEscapedFieldName(t *testing.T) {
	// "encryption_key" with the underscore written as _.
	args := "{\"note\":\"hi\",\"encryption\\u005fkey\":\"leaked-base64-key-material\"}"
	result, ok := setEncryptionKeyArg(args, "encryption_key", nil)
	require.True(t, ok)

	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(result), &m))
	require.NotContains(t, m, "encryption_key", "a \\u-escaped field name must still be found and stripped")
}

func TestSetEncryptionKeyArg_ReturnsNotOKOnInvalidJSON(t *testing.T) {
	result, ok := setEncryptionKeyArg("not json", "encryption_key", []byte{1, 2, 3})
	require.False(t, ok)
	require.Equal(t, "not json", result)
}
