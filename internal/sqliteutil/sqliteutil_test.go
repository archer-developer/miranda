package sqliteutil

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestEnsureColumn_AddsMissingColumn(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	_, err := db.ExecContext(ctx, `CREATE TABLE widgets (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)

	require.NoError(t, EnsureColumn(ctx, db, "widgets", "kind", `TEXT NOT NULL DEFAULT 'x'`))

	_, err = db.ExecContext(ctx, `INSERT INTO widgets (id) VALUES ('w1')`)
	require.NoError(t, err)

	var kind string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT kind FROM widgets WHERE id = 'w1'`).Scan(&kind))
	require.Equal(t, "x", kind, "ALTER TABLE's DEFAULT must apply to a row inserted after the column was added")
}

func TestEnsureColumn_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	_, err := db.ExecContext(ctx, `CREATE TABLE widgets (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)

	require.NoError(t, EnsureColumn(ctx, db, "widgets", "kind", `TEXT NOT NULL DEFAULT 'x'`))
	// A second call must be a no-op, not fail with "duplicate column name".
	require.NoError(t, EnsureColumn(ctx, db, "widgets", "kind", `TEXT NOT NULL DEFAULT 'x'`))
}

func TestEnsureColumn_BackfillsExistingRows(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	_, err := db.ExecContext(ctx, `CREATE TABLE widgets (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO widgets (id) VALUES ('pre-existing')`)
	require.NoError(t, err)

	require.NoError(t, EnsureColumn(ctx, db, "widgets", "kind", `TEXT NOT NULL DEFAULT 'x'`))

	var kind string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT kind FROM widgets WHERE id = 'pre-existing'`).Scan(&kind))
	require.Equal(t, "x", kind, "a row inserted before the column existed must read back the ALTER TABLE default, not an empty string")
}
