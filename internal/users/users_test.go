package users

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
)

func mustHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := HashPassword(password)
	require.NoError(t, err)
	return hash
}

func TestAuthenticate_CorrectAndIncorrectPassword(t *testing.T) {
	r, err := NewRegistry([]config.UserConfig{
		{Username: "alex", PasswordHash: mustHash(t, "555")},
	})
	require.NoError(t, err)

	_, ok := r.Authenticate("alex", "555")
	require.True(t, ok)

	_, ok = r.Authenticate("alex", "wrong")
	require.False(t, ok)

	_, ok = r.Authenticate("nobody", "555")
	require.False(t, ok)
}

func TestNewRegistry_RejectsDuplicateUsername(t *testing.T) {
	_, err := NewRegistry([]config.UserConfig{
		{Username: "alex", PasswordHash: mustHash(t, "555")},
		{Username: "alex", PasswordHash: mustHash(t, "other")},
	})
	require.Error(t, err)
}

func TestNewRegistry_RejectsEmptyUsername(t *testing.T) {
	_, err := NewRegistry([]config.UserConfig{{Username: "", PasswordHash: "x"}})
	require.Error(t, err)
}

func TestResolveUserID_MapsHAUserIDToCanonicalUsername(t *testing.T) {
	r, err := NewRegistry([]config.UserConfig{
		{Username: "alex", PasswordHash: mustHash(t, "555"), HAUserID: "ha-uuid-alex"},
		{Username: "anna", PasswordHash: mustHash(t, "555"), HAUserID: "ha-uuid-anna"},
	})
	require.NoError(t, err)

	require.Equal(t, "alex", r.ResolveUserID(SourceHAAssist, "ha-uuid-alex"))
	require.Equal(t, "anna", r.ResolveUserID(SourceHAAssist, "ha-uuid-anna"))
}

func TestResolveUserID_UnmatchedOrNonHASourcePassesThrough(t *testing.T) {
	r, err := NewRegistry([]config.UserConfig{
		{Username: "alex", PasswordHash: mustHash(t, "555"), HAUserID: "ha-uuid-alex"},
	})
	require.NoError(t, err)

	// Unknown HA id: pass through unchanged rather than erroring, so an
	// unrecognized speaker still gets their own (separate) memory file.
	require.Equal(t, "some-unknown-uuid", r.ResolveUserID(SourceHAAssist, "some-unknown-uuid"))

	// Non-HA sources (cli/api/web_ui) are never remapped here.
	require.Equal(t, "ha-uuid-alex", r.ResolveUserID("cli", "ha-uuid-alex"))
}

func TestResolveByTelegramName_MatchesRegardlessOfAtPrefixOrCase(t *testing.T) {
	r, err := NewRegistry([]config.UserConfig{
		{Username: "alex", PasswordHash: mustHash(t, "555"), TelegramName: "@AlexTG"},
	})
	require.NoError(t, err)

	u, ok := r.ResolveByTelegramName("alextg")
	require.True(t, ok)
	require.Equal(t, "alex", u.Username)

	u, ok = r.ResolveByTelegramName("@AlexTG")
	require.True(t, ok)
	require.Equal(t, "alex", u.Username)

	_, ok = r.ResolveByTelegramName("someone_else")
	require.False(t, ok)

	_, ok = r.ResolveByTelegramName("")
	require.False(t, ok)
}

func TestAll_SortedByUsernameRegardlessOfConfigOrder(t *testing.T) {
	r, err := NewRegistry([]config.UserConfig{
		{Username: "sasha", PasswordHash: "x", FullName: "Саша"},
		{Username: "anna", PasswordHash: "x", FullName: "Аня"},
	})
	require.NoError(t, err)

	all := r.All()
	require.Len(t, all, 2)
	require.Equal(t, "anna", all[0].Username)
	require.Equal(t, "sasha", all[1].Username)
}

func TestResolveByDisplayName_MatchesUsernameOrFullName(t *testing.T) {
	r, err := NewRegistry([]config.UserConfig{
		{Username: "alex", PasswordHash: mustHash(t, "555"), FullName: "Alex Smith"},
		{Username: "anna", PasswordHash: mustHash(t, "555"), FullName: "Аня"},
	})
	require.NoError(t, err)

	u, ok := r.ResolveByDisplayName("аня")
	require.True(t, ok)
	require.Equal(t, "anna", u.Username)

	u, ok = r.ResolveByDisplayName("ALEX")
	require.True(t, ok)
	require.Equal(t, "alex", u.Username)

	_, ok = r.ResolveByDisplayName("nobody")
	require.False(t, ok)
}

func TestDisplayName_FallsBackToUsername(t *testing.T) {
	require.Equal(t, "alex", User{Username: "alex"}.DisplayName())
	require.Equal(t, "Alex Smith", User{Username: "alex", FullName: "Alex Smith"}.DisplayName())
}

func TestEmpty(t *testing.T) {
	r, err := NewRegistry(nil)
	require.NoError(t, err)
	require.True(t, r.Empty())

	r2, err := NewRegistry([]config.UserConfig{{Username: "alex", PasswordHash: mustHash(t, "555")}})
	require.NoError(t, err)
	require.False(t, r2.Empty())
}
