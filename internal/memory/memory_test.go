package memory

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRead_MissingUserReturnsEmpty(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)

	content, err := s.Read("nobody")
	require.NoError(t, err)
	require.Empty(t, content)
}

func TestReadShared_MissingFileReturnsEmpty(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)

	content, err := s.ReadShared()
	require.NoError(t, err)
	require.Empty(t, content)
}

func TestReadShared_ReturnsSharedFileContent(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(dir+"/shared.md", []byte("## Household\n- has two cats\n"), 0o644))

	content, err := s.ReadShared()
	require.NoError(t, err)
	require.Equal(t, "## Household\n- has two cats\n", content)
}

func TestRemember_AppendsUnderRememberedSection(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, s.Remember("alex", "prefers dim lighting after 9pm"))
	require.NoError(t, s.Remember("alex", "allergic to cats"))

	content, err := s.Read("alex")
	require.NoError(t, err)
	require.Contains(t, content, "## Remembered")
	require.Contains(t, content, "prefers dim lighting after 9pm")
	require.Contains(t, content, "allergic to cats")

	// Second fact must be appended, not overwrite the first.
	firstIdx := indexOf(content, "prefers dim lighting")
	secondIdx := indexOf(content, "allergic to cats")
	require.Less(t, firstIdx, secondIdx)
}

// TestRemember_StripsFactsOwnLeadingDateToAvoidDoubleTimestamp guards a real
// bug: a summarization pass shown existing dated "(YYYY-MM-DD) ..." bullets
// as context sometimes echoes that style back into a new fact it proposes,
// which used to produce a visibly broken entry like
// "- (2026-08-15) (2026-08-16) fact" once Remember/RememberShared prepended
// their own timestamp on top of the model's.
func TestRemember_StripsFactsOwnLeadingDateToAvoidDoubleTimestamp(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, s.Remember("alex", "(2026-08-16) went to the dacha"))

	content, err := s.Read("alex")
	require.NoError(t, err)
	require.Contains(t, content, "went to the dacha")
	require.NotContains(t, content, "2026-08-16")
}

func TestRememberShared_StripsFactsOwnLeadingDateToAvoidDoubleTimestamp(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, s.RememberShared("(2026-08-16) went to the dacha"))

	content, err := s.ReadShared()
	require.NoError(t, err)
	require.Contains(t, content, "went to the dacha")
	require.NotContains(t, content, "2026-08-16")
}

func TestReplaceSection_OverwritesOnlyThatSectionAndKeepsOthers(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, s.Remember("alex", "allergic to cats"))
	require.NoError(t, s.ReplaceSection("alex", "Preferences", "- likes jazz\n- dislikes small talk"))
	require.NoError(t, s.ReplaceSection("alex", "Preferences", "- likes jazz\n- likes long walks"))

	content, err := s.Read("alex")
	require.NoError(t, err)

	// Old summarization content must be gone, new one present.
	require.NotContains(t, content, "dislikes small talk")
	require.Contains(t, content, "likes long walks")

	// The append-only Remembered section from before must survive untouched.
	require.Contains(t, content, "allergic to cats")
}

func TestRememberShared_AppendsUnderRememberedSection(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, s.RememberShared("у нас живёт кот Барсик"))
	require.NoError(t, s.RememberShared("wifi пароль: hunter2"))

	content, err := s.ReadShared()
	require.NoError(t, err)
	require.Contains(t, content, "## Remembered")
	require.Contains(t, content, "кот Барсик")
	require.Contains(t, content, "wifi пароль")

	// Second fact must be appended, not overwrite the first.
	firstIdx := indexOf(content, "кот Барсик")
	secondIdx := indexOf(content, "wifi пароль")
	require.Less(t, firstIdx, secondIdx)
}

func TestRememberShared_DoesNotTouchPersonalMemory(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, s.Remember("alex", "personal fact"))
	require.NoError(t, s.RememberShared("shared fact"))

	shared, err := s.ReadShared()
	require.NoError(t, err)
	require.Contains(t, shared, "shared fact")
	require.NotContains(t, shared, "personal fact")

	personal, err := s.Read("alex")
	require.NoError(t, err)
	require.Contains(t, personal, "personal fact")
	require.NotContains(t, personal, "shared fact")
}

func TestRememberShared_IsSafeForConcurrentUse(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)

	done := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() {
			done <- s.RememberShared("household fact")
		}()
	}
	for i := 0; i < 20; i++ {
		require.NoError(t, <-done)
	}

	content, err := s.ReadShared()
	require.NoError(t, err)
	require.Equal(t, 20, countOccurrences(content, "- ("))
}

func TestRemember_IsSafeForConcurrentUse(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)

	done := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func(i int) {
			done <- s.Remember("alex", "fact")
		}(i)
	}
	for i := 0; i < 20; i++ {
		require.NoError(t, <-done)
	}

	content, err := s.Read("alex")
	require.NoError(t, err)
	require.Equal(t, 20, countOccurrences(content, "- ("))
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func countOccurrences(s, substr string) int {
	count := 0
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			count++
		}
	}
	return count
}
