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
