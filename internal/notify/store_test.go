package notify

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "notify.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestListForUser_NewestFirstAndScopedToUser(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	_, err := s.CreateNotification(ctx, "alice", "t1", "b1", "reminder")
	require.NoError(t, err)
	_, err = s.CreateNotification(ctx, "bob", "other", "other-body", "tool")
	require.NoError(t, err)
	second, err := s.CreateNotification(ctx, "alice", "t2", "b2", "tool")
	require.NoError(t, err)

	got, err := s.ListForUser(ctx, "alice", 0)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, second.ID, got[0].ID, "newest notification must come first")
	require.Equal(t, "t1", got[1].Title)
}

func TestListForUser_EmptyForUnknownUser(t *testing.T) {
	s := openTestStore(t)
	got, err := s.ListForUser(context.Background(), "nobody", 0)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestListForUser_RespectsLimit(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	for i := 0; i < 5; i++ {
		_, err := s.CreateNotification(ctx, "alice", "t", "b", "tool")
		require.NoError(t, err)
	}
	got, err := s.ListForUser(ctx, "alice", 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
}

func TestUnreadCountAndMarkAllRead(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	_, err := s.CreateNotification(ctx, "alice", "t1", "b1", "reminder")
	require.NoError(t, err)
	_, err = s.CreateNotification(ctx, "alice", "t2", "b2", "tool")
	require.NoError(t, err)
	_, err = s.CreateNotification(ctx, "bob", "t3", "b3", "tool")
	require.NoError(t, err)

	count, err := s.UnreadCount(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, 2, count)

	require.NoError(t, s.MarkAllRead(ctx, "alice"))

	count, err = s.UnreadCount(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, 0, count, "alice's notifications must all be read now")

	bobCount, err := s.UnreadCount(ctx, "bob")
	require.NoError(t, err)
	require.Equal(t, 1, bobCount, "marking alice's read must not touch bob's")

	got, err := s.ListForUser(ctx, "alice", 0)
	require.NoError(t, err)
	for _, n := range got {
		require.NotNil(t, n.ReadAt)
	}

	// Idempotent: a second call with nothing left unread must not error.
	require.NoError(t, s.MarkAllRead(ctx, "alice"))
}

func TestUpsertSubscription_ReplacesOnSameEndpoint(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	sub := Subscription{Endpoint: "https://push.example/1", UserID: "alice", P256dh: "p1", Auth: "a1"}
	require.NoError(t, s.UpsertSubscription(ctx, sub))

	sub.P256dh = "p2" // simulate the browser's keys rotating on re-subscribe
	require.NoError(t, s.UpsertSubscription(ctx, sub))

	got, err := s.SubscriptionsForUser(ctx, "alice")
	require.NoError(t, err)
	require.Len(t, got, 1, "re-subscribing the same endpoint must upsert, not duplicate")
	require.Equal(t, "p2", got[0].P256dh)
}

func TestDeleteSubscription_OwnershipScoped(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	sub := Subscription{Endpoint: "https://push.example/1", UserID: "alice", P256dh: "p1", Auth: "a1"}
	require.NoError(t, s.UpsertSubscription(ctx, sub))

	err := s.DeleteSubscription(ctx, "https://push.example/1", "bob")
	require.ErrorIs(t, err, ErrNotFound, "deleting another user's subscription must fail as not-found, not succeed")

	got, err := s.SubscriptionsForUser(ctx, "alice")
	require.NoError(t, err)
	require.Len(t, got, 1, "bob's failed delete must not remove alice's subscription")

	require.NoError(t, s.DeleteSubscription(ctx, "https://push.example/1", "alice"))
	got, err = s.SubscriptionsForUser(ctx, "alice")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestDeleteSubscription_UnknownEndpoint(t *testing.T) {
	s := openTestStore(t)
	err := s.DeleteSubscription(context.Background(), "https://push.example/does-not-exist", "alice")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDeleteSubscriptionByEndpoint_PrunesRegardlessOfOwner(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	sub := Subscription{Endpoint: "https://push.example/1", UserID: "alice", P256dh: "p1", Auth: "a1"}
	require.NoError(t, s.UpsertSubscription(ctx, sub))

	require.NoError(t, s.deleteSubscriptionByEndpoint(ctx, "https://push.example/1"))

	got, err := s.SubscriptionsForUser(ctx, "alice")
	require.NoError(t, err)
	require.Empty(t, got)
}
