package memory

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeRedactor stands in for internal/redact — see the note on
// history.fakeRedactor for why the engine itself is not re-tested here.
type fakeRedactor struct{}

func (fakeRedactor) Redact(s string) string {
	return strings.ReplaceAll(s, "665533", "******")
}

func newRedactingStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	require.NoError(t, err)
	s.SetRedactor(fakeRedactor{})
	return s
}

// Every one of these goes through writeFile, which is the single choke point
// redaction is applied at. They are enumerated anyway because "all four write
// paths funnel through one function" is exactly the kind of invariant that
// quietly stops being true.

func TestRemember_Redacts(t *testing.T) {
	s := newRedactingStore(t)

	require.NoError(t, s.Remember("alex", "пин-код от телефона Ани 665533"))

	content, err := s.Read("alex")
	require.NoError(t, err)
	require.Contains(t, content, "пин-код от телефона Ани ******")
	require.NotContains(t, content, "665533")
}

func TestRememberShared_Redacts(t *testing.T) {
	s := newRedactingStore(t)

	require.NoError(t, s.RememberShared("код от домофона 665533"))

	content, err := s.ReadShared()
	require.NoError(t, err)
	require.NotContains(t, content, "665533")
}

// TestWrite_Redacts covers the web UI's PUT /api/memory — the one path that
// overwrites a memory file wholesale, and the only one where the text comes
// straight from a browser rather than from the model.
func TestWrite_Redacts(t *testing.T) {
	s := newRedactingStore(t)

	require.NoError(t, s.Write("alex", "## Remembered\n- пин 665533\n"))

	content, err := s.Read("alex")
	require.NoError(t, err)
	require.Equal(t, "## Remembered\n- пин ******\n", content)
}

// TestReplaceSection_Redacts covers the summarization pass, which is how a
// secret would otherwise get promoted from one conversation into permanent
// memory.
func TestReplaceSection_Redacts(t *testing.T) {
	s := newRedactingStore(t)

	require.NoError(t, s.ReplaceSection("alex", "Preferences", "- пин Ани 665533"))

	content, err := s.Read("alex")
	require.NoError(t, err)
	require.Contains(t, content, "- пин Ани ******")
	require.NotContains(t, content, "665533")
}

// TestWriteFile_RemasksExistingContent — because the whole document is
// re-redacted on every write, a fact stored before redaction was switched on
// gets masked the next time anything touches the file.
func TestWriteFile_RemasksExistingContent(t *testing.T) {
	dir := t.TempDir()

	plain, err := New(dir)
	require.NoError(t, err)
	require.NoError(t, plain.Remember("alex", "пин 665533"))

	content, err := plain.Read("alex")
	require.NoError(t, err)
	require.Contains(t, content, "665533", "precondition: stored unmasked")

	redacting, err := New(dir)
	require.NoError(t, err)
	redacting.SetRedactor(fakeRedactor{})
	require.NoError(t, redacting.Remember("alex", "любит чай"))

	content, err = redacting.Read("alex")
	require.NoError(t, err)
	require.NotContains(t, content, "665533", "the pre-existing fact should have been masked too")
	require.Contains(t, content, "любит чай")
}

func TestStore_WithoutRedactorWritesVerbatim(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, s.Remember("alex", "пин 665533"))

	content, err := s.Read("alex")
	require.NoError(t, err)
	require.Contains(t, content, "665533")
}
