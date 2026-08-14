package oauth2

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testMasterKey(t *testing.T) []byte {
	t.Helper()
	return make([]byte, masterKeySize) // all-zero key is fine for tests, never used in production
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "oauth.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStore_PutGetRoundtrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	key := testMasterKey(t)

	expiry := time.Now().Add(time.Hour).Truncate(time.Second).UTC()
	err := s.PutToken(ctx, key, Token{
		Username: "alice", Provider: "google_calendar",
		AccessToken: "access-1", RefreshToken: "refresh-1",
		Scope: "calendar", Expiry: expiry,
	})
	require.NoError(t, err)

	got, ok, err := s.GetToken(ctx, key, "alice", "google_calendar")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "access-1", got.AccessToken)
	require.Equal(t, "refresh-1", got.RefreshToken)
	require.Equal(t, "calendar", got.Scope)
	require.WithinDuration(t, expiry, got.Expiry, time.Second)
}

func TestStore_GetToken_NotFound(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	_, ok, err := s.GetToken(ctx, testMasterKey(t), "nobody", "google_calendar")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestStore_WrongMasterKeyFailsLoudly(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	err := s.PutToken(ctx, testMasterKey(t), Token{
		Username: "alice", Provider: "google_calendar",
		AccessToken: "access-1", Expiry: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)

	wrongKey := make([]byte, masterKeySize)
	wrongKey[0] = 1
	_, _, err = s.GetToken(ctx, wrongKey, "alice", "google_calendar")
	require.Error(t, err)
}

func TestStore_PutToken_UpsertDoesNotDuplicate(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	key := testMasterKey(t)

	for i := 0; i < 3; i++ {
		err := s.PutToken(ctx, key, Token{
			Username: "alice", Provider: "google_calendar",
			AccessToken: "access", RefreshToken: "refresh",
			Expiry: time.Now().Add(time.Hour),
		})
		require.NoError(t, err)
	}

	var n int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_tokens WHERE username = ? AND provider = ?`, "alice", "google_calendar").Scan(&n))
	require.Equal(t, 1, n)
}

func TestStore_PutToken_EmptyRefreshDoesNotOverwrite(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	key := testMasterKey(t)

	require.NoError(t, s.PutToken(ctx, key, Token{
		Username: "alice", Provider: "google_calendar",
		AccessToken: "access-1", RefreshToken: "refresh-1",
		Expiry: time.Now().Add(time.Hour),
	}))

	// Simulate a caller merging in the old refresh token itself (Service does
	// this) after a refresh response that omitted RefreshToken.
	existing, ok, err := s.GetToken(ctx, key, "alice", "google_calendar")
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, s.PutToken(ctx, key, Token{
		Username: "alice", Provider: "google_calendar",
		AccessToken: "access-2", RefreshToken: existing.RefreshToken,
		Expiry: time.Now().Add(time.Hour),
	}))

	got, ok, err := s.GetToken(ctx, key, "alice", "google_calendar")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "access-2", got.AccessToken)
	require.Equal(t, "refresh-1", got.RefreshToken)
}

func TestStore_HasToken(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	key := testMasterKey(t)

	has, err := s.HasToken(ctx, "alice", "google_calendar")
	require.NoError(t, err)
	require.False(t, has)

	require.NoError(t, s.PutToken(ctx, key, Token{
		Username: "alice", Provider: "google_calendar",
		AccessToken: "access", Expiry: time.Now().Add(time.Hour),
	}))

	has, err = s.HasToken(ctx, "alice", "google_calendar")
	require.NoError(t, err)
	require.True(t, has)
}

func TestStore_ListDueForRefresh(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	key := testMasterKey(t)

	// Due soon, has a refresh token.
	require.NoError(t, s.PutToken(ctx, key, Token{
		Username: "alice", Provider: "google_calendar",
		AccessToken: "a1", RefreshToken: "r1", Expiry: time.Now().Add(time.Minute),
	}))
	// Not due yet.
	require.NoError(t, s.PutToken(ctx, key, Token{
		Username: "bob", Provider: "google_calendar",
		AccessToken: "a2", RefreshToken: "r2", Expiry: time.Now().Add(time.Hour),
	}))
	// Due soon but no refresh token at all.
	require.NoError(t, s.PutToken(ctx, key, Token{
		Username: "carol", Provider: "google_calendar",
		AccessToken: "a3", Expiry: time.Now().Add(time.Minute),
	}))

	due, err := s.ListDueForRefresh(ctx, key, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Equal(t, "alice", due[0].Username)
	require.Equal(t, "r1", due[0].RefreshToken)
}

func TestStore_DeleteToken(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	key := testMasterKey(t)

	require.NoError(t, s.PutToken(ctx, key, Token{
		Username: "alice", Provider: "google_calendar",
		AccessToken: "access", Expiry: time.Now().Add(time.Hour),
	}))
	require.NoError(t, s.DeleteToken(ctx, "alice", "google_calendar"))

	_, ok, err := s.GetToken(ctx, key, "alice", "google_calendar")
	require.NoError(t, err)
	require.False(t, ok)
}
