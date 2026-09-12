// Package sqliteutil holds small helpers shared by this project's several
// independent SQLite-backed store packages (internal/history,
// internal/schedule, ...) — each opens its own database and owns its own
// schema, but they share the same modernc.org/sqlite driver quirks.
package sqliteutil

import (
	"context"
	"database/sql"
	"fmt"
)

// EnsureColumn adds column to table if it isn't already there. SQLite's
// CREATE TABLE IF NOT EXISTS is a no-op on an already-migrated database, so
// a column added after a store's initial release needs this explicit
// existence check instead — ALTER TABLE ADD COLUMN has no IF NOT EXISTS
// form in the SQLite versions modernc.org/sqlite has historically tracked.
func EnsureColumn(ctx context.Context, db *sql.DB, table, column, sqlType string) error {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("sqliteutil: inspect %s columns: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("sqliteutil: scan %s column info: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqliteutil: read %s column info: %w", table, err)
	}

	if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, sqlType)); err != nil {
		return fmt.Errorf("sqliteutil: add column %s.%s: %w", table, column, err)
	}
	return nil
}
