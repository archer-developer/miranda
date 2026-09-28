// Package notify implements Miranda's native, in-app/browser notification
// channel — a persisted, per-user feed (the web UI's bell icon) with an
// optional Web Push delivery leg on top, so a notification can reach a
// household member's phone as a real OS notification even when no Miranda
// tab is open. It replaces Telegram as the *default* auto-alert channel
// (reminders, the send_notification tool) — see
// ../../docs/adr/native-notifications.md for the full rationale; Telegram
// itself is untouched and still used when a user explicitly asks for it.
//
// This package knows nothing about internal/hub or internal/agent_loop —
// same separation internal/telegram keeps — so it can't publish a live
// "a new notification arrived" event itself; the caller (Orchestrator)
// does that after Notify returns, the same way it already publishes
// ChatEvents for other things.
package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// ErrNotFound is returned by DeleteSubscription when endpoint doesn't exist
// or doesn't belong to the given userID — see that method's doc comment.
var ErrNotFound = errors.New("notify: subscription not found")

// Notification is one row in the persisted feed. JSON tags matter here —
// this is marshaled straight out over both GET /api/notifications and the
// live ChatEvent{Type: "notification"} WS payload (see Orchestrator.notifyUser),
// and the web UI's screens/notifications.js / notify-badge.js read these
// exact lowercase/snake_case field names.
type Notification struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	// Source is "reminder" (fired by deliverReminder) or "tool" (the
	// send_notification tool) — purely informational, not branched on.
	Source    string     `json:"source"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
}

// Subscription is one browser's Web Push registration (PushSubscription.toJSON()
// from the client), keyed by Endpoint — globally unique per the Push API spec,
// so upserting by endpoint naturally dedupes a browser re-subscribing. Not
// itself serialized back out to any client today (webui.notifications.go
// builds one from its own request struct), but tagged anyway to match every
// other JSON-facing type in this package rather than relying on Go's default
// capitalized field names if that ever changes.
type Subscription struct {
	Endpoint  string    `json:"endpoint"`
	UserID    string    `json:"user_id"`
	P256dh    string    `json:"p256dh"`
	Auth      string    `json:"auth"`
	UserAgent string    `json:"user_agent"`
	CreatedAt time.Time `json:"created_at"`
}

// Store is a SQLite-backed store for both the notification feed and Web
// Push subscriptions — one file (Storage.NotifySQLitePath), since these are
// one feature, not two independent infra pieces the way Telegram's webhook
// plumbing is isolated from everything else.
type Store struct {
	db *sql.DB
}

// Open creates (if needed) and opens the SQLite database at path, applying
// the schema. Mirrors internal/schedule.Open/internal/webauthn.Open's shape.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("notify: create dir %s: %w", dir, err)
		}
	}

	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("notify: open %s: %w", path, err)
	}
	// SQLite only supports one writer at a time; a single connection avoids
	// SQLITE_BUSY errors from this process racing against itself.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS notifications (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL,
			title      TEXT NOT NULL,
			body       TEXT NOT NULL,
			source     TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			read_at    DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS idx_notifications_user ON notifications(user_id, created_at DESC)`,
		// endpoint is the PRIMARY KEY (not a separate id) so re-subscribing
		// the same browser (endpoint unchanged, keys sometimes rotate) is a
		// plain upsert rather than accumulating duplicate rows.
		`CREATE TABLE IF NOT EXISTS webpush_subscriptions (
			endpoint   TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL,
			p256dh     TEXT NOT NULL,
			auth       TEXT NOT NULL,
			user_agent TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_webpush_subscriptions_user ON webpush_subscriptions(user_id)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("notify: migrate: %w", err)
		}
	}
	return nil
}

// CreateNotification inserts a new notification and returns it with its
// generated ID/CreatedAt filled in — the caller (Service.Notify) needs the
// full row to publish a live event carrying it.
func (s *Store) CreateNotification(ctx context.Context, userID, title, body, source string) (Notification, error) {
	n := Notification{
		ID:        uuid.NewString(),
		UserID:    userID,
		Title:     title,
		Body:      body,
		Source:    source,
		CreatedAt: time.Now(),
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO notifications (id, user_id, title, body, source, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		n.ID, n.UserID, n.Title, n.Body, n.Source, n.CreatedAt,
	)
	if err != nil {
		return Notification{}, fmt.Errorf("notify: create notification: %w", err)
	}
	return n, nil
}

// ListForUser returns userID's notifications newest-first, capped at limit
// (0 means no cap).
func (s *Store) ListForUser(ctx context.Context, userID string, limit int) ([]Notification, error) {
	query := `SELECT id, user_id, title, body, source, created_at, read_at FROM notifications WHERE user_id = ? ORDER BY created_at DESC`
	args := []any{userID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("notify: list notifications: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Notification
	for rows.Next() {
		var n Notification
		var readAt sql.NullTime
		if err := rows.Scan(&n.ID, &n.UserID, &n.Title, &n.Body, &n.Source, &n.CreatedAt, &readAt); err != nil {
			return nil, fmt.Errorf("notify: scan notification: %w", err)
		}
		if readAt.Valid {
			n.ReadAt = &readAt.Time
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notify: read notifications: %w", err)
	}
	return out, nil
}

// UnreadCount returns how many of userID's notifications have no read_at yet.
func (s *Store) UnreadCount(ctx context.Context, userID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id = ? AND read_at IS NULL`, userID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("notify: unread count: %w", err)
	}
	return count, nil
}

// MarkAllRead stamps read_at on every currently-unread notification for
// userID. Idempotent — a second call with nothing left unread is a no-op.
func (s *Store) MarkAllRead(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE notifications SET read_at = ? WHERE user_id = ? AND read_at IS NULL`,
		time.Now(), userID,
	)
	if err != nil {
		return fmt.Errorf("notify: mark all read: %w", err)
	}
	return nil
}

// UpsertSubscription persists a browser's Web Push registration, replacing
// any existing row for the same endpoint (a browser re-subscribing, or its
// keys rotating) — see Subscription's doc comment.
func (s *Store) UpsertSubscription(ctx context.Context, sub Subscription) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO webpush_subscriptions (endpoint, user_id, p256dh, auth, user_agent, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(endpoint) DO UPDATE SET user_id = excluded.user_id, p256dh = excluded.p256dh, auth = excluded.auth, user_agent = excluded.user_agent`,
		sub.Endpoint, sub.UserID, sub.P256dh, sub.Auth, sub.UserAgent, time.Now(),
	)
	if err != nil {
		return fmt.Errorf("notify: upsert subscription: %w", err)
	}
	return nil
}

// DeleteSubscription removes a subscription by endpoint if it belongs to
// userID — returns ErrNotFound whether the endpoint doesn't exist or
// belongs to someone else, the same "doesn't reveal which" contract
// schedule.Store.Delete already uses.
func (s *Store) DeleteSubscription(ctx context.Context, endpoint, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM webpush_subscriptions WHERE endpoint = ? AND user_id = ?`, endpoint, userID,
	)
	if err != nil {
		return fmt.Errorf("notify: delete subscription: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("notify: delete subscription: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// deleteSubscriptionByEndpoint removes a dead subscription regardless of
// owner — used when a push service itself reports the endpoint gone
// (404/410), where the caller (the webpush send path) has no userID
// ambiguity to worry about: it already looked the row up by endpoint.
func (s *Store) deleteSubscriptionByEndpoint(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM webpush_subscriptions WHERE endpoint = ?`, endpoint)
	if err != nil {
		return fmt.Errorf("notify: delete dead subscription: %w", err)
	}
	return nil
}

// SubscriptionsForUser returns every Web Push subscription registered for userID.
func (s *Store) SubscriptionsForUser(ctx context.Context, userID string) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT endpoint, user_id, p256dh, auth, user_agent, created_at FROM webpush_subscriptions WHERE user_id = ?`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("notify: list subscriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.Endpoint, &sub.UserID, &sub.P256dh, &sub.Auth, &sub.UserAgent, &sub.CreatedAt); err != nil {
			return nil, fmt.Errorf("notify: scan subscription: %w", err)
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notify: read subscriptions: %w", err)
	}
	return out, nil
}
