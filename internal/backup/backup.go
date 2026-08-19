// Package backup periodically snapshots every SQLite store under
// config.StorageConfig, plus config.yaml's *.yaml files and .env, into
// timestamped subdirectories on disk. SQLite stores are copied via SQLite's
// own VACUUM INTO rather than a raw file copy — see Run for why. It owns no
// ticker or scheduling logic of its own: cmd/miranda drives it on a ticker
// the same way it drives internal/history's idle sweep and
// internal/schedule's due-task sweep.
package backup

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/archer-developer/miranda/internal/config"
)

// configSubdir is the name of the directory inside a run's timestamped
// backup directory that config.yaml's *.yaml files are copied into.
const configSubdir = "config"

// vacuumBusyTimeoutMS bounds how long a backup connection waits for the
// application's own connection to release its lock, mirroring the
// busy_timeout every other store in this codebase opens with (see e.g.
// internal/history.Open) rather than failing immediately on
// "database is locked".
const vacuumBusyTimeoutMS = 5000

// source is one SQLite store to snapshot: label becomes its filename
// (label + ".db") inside a run's timestamped subdirectory.
type source struct {
	label string
	path  string
}

// sources lists every SQLite store StorageConfig knows about. A store whose
// feature is disabled (e.g. WebAuthn.Enabled == false) simply has no file on
// disk yet and is skipped by Run — this list doesn't need to know which
// features are currently on, only where each store's file would live.
func sources(cfg config.StorageConfig) []source {
	return []source{
		{label: "miranda", path: cfg.SQLitePath},
		{label: "webauthn", path: cfg.WebAuthnSQLitePath},
		{label: "schedule", path: cfg.ScheduleSQLitePath},
		{label: "keyring", path: cfg.KeyringSQLitePath},
		{label: "oauth", path: cfg.OAuthSQLitePath},
	}
}

// timestampFormat names each run's subdirectory so lexicographic sort order
// equals chronological order — Prune relies on this to find the oldest runs
// without parsing each name back into a time.Time.
const timestampFormat = "20060102-150405"

// Run performs one full backup cycle into a new
// backupCfg.Dir/<UTC timestamp>/ subdirectory:
//
//   - VACUUM INTO a fresh copy of every SQLite store that currently exists
//     on disk (see sources), as <label>.db.
//   - Copy configDir's *.yaml files (config.yaml's own source files —
//     without them a restored backup can't even start) into a config/
//     subdirectory, if configDir exists.
//   - Copy envPath (the .env file holding API keys/tokens) alongside them,
//     if it exists — deliberately included despite being sensitive, since
//     oauth.db and keyring.db are encrypted under keys that live only in
//     .env; without it those two stores' backups can never be decrypted.
//
// Old run subdirectories beyond backupCfg.RetentionCount are pruned after a
// successful run.
//
// VACUUM INTO (rather than copying the SQLite file bytes directly) is what
// makes backing up the SQLite stores safe to run while the application's
// own connection is open and possibly mid-write: SQLite computes a
// transactionally consistent snapshot as it streams pages into the
// destination file, the same guarantee a raw `cp` of a live database file
// does not have — a copy caught mid-write can capture a torn page or, if
// the source is in WAL mode, miss data still sitting in the -wal file.
// config.yaml's *.yaml files and .env don't need this treatment: they're
// edited by hand, not written to by the running process.
//
// Any input that doesn't exist (a SQLite store whose feature has never
// been used, configDir, or envPath) is skipped rather than treated as an
// error. If nothing exists at all, Run returns nil without creating an
// (empty) timestamped subdirectory.
func Run(ctx context.Context, backupCfg config.BackupConfig, storageCfg config.StorageConfig, configDir, envPath string, logger *slog.Logger) error {
	var dbSources []source
	for _, s := range sources(storageCfg) {
		if _, err := os.Stat(s.path); err != nil {
			continue
		}
		dbSources = append(dbSources, s)
	}
	haveConfigDir := isDir(configDir)
	haveEnv := isFile(envPath)

	if len(dbSources) == 0 && !haveConfigDir && !haveEnv {
		return nil
	}

	runDir := filepath.Join(backupCfg.Dir, time.Now().UTC().Format(timestampFormat))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("backup: create %s: %w", runDir, err)
	}

	for _, s := range dbSources {
		dst := filepath.Join(runDir, s.label+".db")
		if err := snapshot(ctx, s.path, dst); err != nil {
			// A partial backup set is worse than none: retention counts
			// runDir as one of the kept snapshots, and a restore built from
			// an incomplete set silently loses whichever store failed. Tear
			// it down rather than leave it to be counted as a good backup.
			_ = os.RemoveAll(runDir)
			return fmt.Errorf("backup: snapshot %s: %w", s.label, err)
		}
	}
	if haveConfigDir {
		if err := copyDir(configDir, filepath.Join(runDir, configSubdir)); err != nil {
			_ = os.RemoveAll(runDir)
			return fmt.Errorf("backup: copy config dir %s: %w", configDir, err)
		}
	}
	if haveEnv {
		if err := copyFile(envPath, filepath.Join(runDir, filepath.Base(envPath))); err != nil {
			_ = os.RemoveAll(runDir)
			return fmt.Errorf("backup: copy %s: %w", envPath, err)
		}
	}
	logger.Info("database backup complete", "dir", runDir, "stores", len(dbSources), "config", haveConfigDir, "env", haveEnv)

	if err := prune(backupCfg.Dir, backupCfg.RetentionCount); err != nil {
		return fmt.Errorf("backup: prune %s: %w", backupCfg.Dir, err)
	}
	return nil
}

func isDir(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func isFile(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// snapshot opens its own short-lived connection to src (independent of any
// connection the running application already holds open on the same file)
// and runs VACUUM INTO dst on it.
func snapshot(ctx context.Context, src, dst string) error {
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(%d)", src, vacuumBusyTimeoutMS)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("vacuum %s into %s: %w", src, dst, err)
	}
	return nil
}

// copyFile copies src to dst, preserving src's file mode — notably so a
// restrictively-permissioned .env (holding API keys/tokens) doesn't end up
// world-readable at its backup destination.
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return out.Close()
}

// copyDir recursively copies every file under srcDir into dstDir, preserving
// the relative directory structure and each file's mode (see copyFile).
func copyDir(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

// prune deletes the oldest timestamped run subdirectories of dir beyond the
// most recent keep. keep == 0 means keep all of them (same convention as
// config.LoggingConfig.MaxBackups).
func prune(dir string, keep int) error {
	if keep <= 0 {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}

	var runs []string
	for _, e := range entries {
		if e.IsDir() {
			runs = append(runs, e.Name())
		}
	}
	if len(runs) <= keep {
		return nil
	}

	sort.Strings(runs) // timestampFormat sorts lexicographically == chronologically
	for _, name := range runs[:len(runs)-keep] {
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
	return nil
}
