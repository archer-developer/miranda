// Package backup periodically snapshots every SQLite store under
// config.StorageConfig, plus config.yaml's *.yaml files and .env, into a
// single timestamped tar.gz archive that's copied atomically into
// config.BackupConfig.Dir. It owns no ticker or scheduling logic of its
// own: cmd/miranda drives it on a ticker the same way it drives
// internal/history's idle sweep and internal/schedule's due-task sweep.
//
// Everything is staged on local disk first (BackupConfig.TempDir,
// defaulting to os.TempDir()) and only ever reaches Dir as one finished,
// already-verified archive file, written as a ".part" and renamed into
// place. This two-step design exists because Dir is routinely a network
// mount (CIFS/SMB to a NAS, as in production) and SQLite's VACUUM INTO
// cannot take the lock it needs on a destination file that lives on such a
// filesystem — attempting it fails with "database is locked (5)
// (SQLITE_BUSY)" even though the exact same VACUUM INTO against a local
// path succeeds immediately. This is a limitation of SQLite and SMB, not a
// bug in either: nothing SQLite-shaped is ever written directly into Dir
// any more, only a plain file copy is, and a plain copy (unlike a
// database's own locking) works fine over CIFS.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/archer-developer/miranda/internal/config"
)

// configSubdir is the name of the directory inside the staged archive that
// config.yaml's *.yaml files are copied into.
const configSubdir = "config"

// archivePrefix/archiveSuffix name the finished archive file;
// archivePrefix also doubles as the glob prune matches against, so a
// deployment's backup.dir can hold unrelated files without prune touching
// them. partSuffix is the in-progress name used while copying toward
// backup.dir — see uploadAtomic.
const (
	archivePrefix = "miranda-"
	archiveSuffix = ".tar.gz"
	partSuffix    = ".part"
)

// stalePartAge is how old an orphaned "*.tar.gz.part" file (left behind by
// a run that crashed mid-upload) must be before prune deletes it. A
// generous window: it only needs to be longer than any single backup run
// could plausibly take.
const stalePartAge = 24 * time.Hour

// vacuumBusyTimeoutMS bounds how long a backup connection waits for the
// application's own connection to release its lock, mirroring the
// busy_timeout every other store in this codebase opens with (see e.g.
// internal/history.Open) rather than failing immediately on
// "database is locked".
const vacuumBusyTimeoutMS = 5000

// source is one SQLite store to snapshot: label becomes its filename
// (label + ".db") inside the staged archive.
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

// timestampFormat names each archive so lexicographic sort order equals
// chronological order — prune relies on this to find the oldest archives
// without parsing each name back into a time.Time.
const timestampFormat = "20060102-150405"

// manifest is written as manifest.json at the root of every archive, so a
// restore (or a human with `tar tzf`) can tell what's inside and verify it
// without needing the original config.BackupConfig that produced it.
type manifest struct {
	GeneratedAt time.Time       `json:"generated_at"`
	App         string          `json:"app"`
	Version     string          `json:"version,omitempty"`
	Databases   []manifestEntry `json:"databases,omitempty"`
	Config      bool            `json:"config"`
	Env         bool            `json:"env"`
}

type manifestEntry struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Run performs one full backup cycle:
//
//  1. Stage everything under a fresh temporary directory (BackupConfig.TempDir,
//     or os.TempDir() if empty) on local disk:
//     - VACUUM INTO a fresh copy of every SQLite store that currently
//     exists on disk (see sources), as <label>.db. VACUUM INTO is what
//     makes this safe to run while the application's own connection is
//     open and possibly mid-write: SQLite computes a transactionally
//     consistent snapshot as it streams pages into the destination file,
//     the same guarantee a raw `cp` of a live database file does not
//     have — a copy caught mid-write can capture a torn page or, if the
//     source is in WAL mode, miss data still sitting in the -wal file.
//     - Run PRAGMA integrity_check against each snapshot; anything other
//     than "ok" fails the run rather than shipping a corrupt backup.
//     - Copy configDir's *.yaml files into a config/ subdirectory, and
//     envPath (the .env file holding API keys/tokens) alongside them — both
//     are edited by hand, not written to by the running process, so they
//     don't need VACUUM INTO's treatment, a plain copy is fine.
//  2. Write a manifest.json describing what's inside, then pack the whole
//     staging directory into <TempDir>/miranda-<UTC timestamp>.tar.gz.
//  3. Copy that one finished archive into backupCfg.Dir atomically: write
//     it as "<name>.tar.gz.part", fsync, verify its size and SHA-256 match
//     what was staged, then os.Rename into "<name>.tar.gz". A rename within
//     one directory is atomic even on CIFS/SMB, so backup.dir never shows a
//     half-written archive.
//  4. Prune old archives beyond backupCfg.RetentionCount, and delete any
//     stale ".part" file left over from a previous crashed run.
//
// Any input that doesn't exist (a SQLite store whose feature has never
// been used, configDir, or envPath) is skipped rather than treated as an
// error. If nothing exists at all, Run returns nil without creating an
// (empty) archive.
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

	stageDir, err := os.MkdirTemp(backupCfg.TempDir, "miranda-backup-*")
	if err != nil {
		logger.Error("backup failed", "step", "stage", "error", err)
		return fmt.Errorf("backup: create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	contentDir := filepath.Join(stageDir, "content")
	if err := os.MkdirAll(contentDir, 0o755); err != nil {
		logger.Error("backup failed", "step", "stage", "error", err)
		return fmt.Errorf("backup: create content dir: %w", err)
	}

	for _, s := range dbSources {
		dst := filepath.Join(contentDir, s.label+".db")
		if err := snapshot(ctx, s.path, dst); err != nil {
			logger.Error("backup failed", "step", "snapshot", "source", s.label, "error", err)
			return fmt.Errorf("backup: snapshot %s: %w", s.label, err)
		}
		if err := integrityCheck(ctx, dst); err != nil {
			logger.Error("backup failed", "step", "verify", "source", s.label, "error", err)
			return fmt.Errorf("backup: verify %s: %w", s.label, err)
		}
	}
	if haveConfigDir {
		if err := copyDir(configDir, filepath.Join(contentDir, configSubdir)); err != nil {
			logger.Error("backup failed", "step", "stage", "error", err)
			return fmt.Errorf("backup: copy config dir %s: %w", configDir, err)
		}
	}
	if haveEnv {
		if err := copyFile(envPath, filepath.Join(contentDir, filepath.Base(envPath))); err != nil {
			logger.Error("backup failed", "step", "stage", "error", err)
			return fmt.Errorf("backup: copy %s: %w", envPath, err)
		}
	}

	m := manifest{
		GeneratedAt: time.Now().UTC(),
		App:         "miranda",
		Version:     buildVersion(),
		Config:      haveConfigDir,
		Env:         haveEnv,
	}
	for _, s := range dbSources {
		sum, size, err := hashFile(filepath.Join(contentDir, s.label+".db"))
		if err != nil {
			logger.Error("backup failed", "step", "archive", "error", err)
			return fmt.Errorf("backup: hash %s: %w", s.label, err)
		}
		m.Databases = append(m.Databases, manifestEntry{Name: s.label + ".db", Bytes: size, SHA256: sum})
	}

	ts := m.GeneratedAt.Format(timestampFormat)
	archiveName := archivePrefix + ts + archiveSuffix
	archivePath := filepath.Join(stageDir, archiveName)
	sum, size, err := writeArchive(contentDir, m, archivePath)
	if err != nil {
		logger.Error("backup failed", "step", "archive", "error", err)
		return fmt.Errorf("backup: build archive: %w", err)
	}

	if err := uploadAtomic(archivePath, backupCfg.Dir, archiveName, sum, size); err != nil {
		logger.Error("backup failed", "step", "upload", "error", err)
		return fmt.Errorf("backup: upload %s: %w", archiveName, err)
	}
	logger.Info("backup ok", "path", filepath.Join(backupCfg.Dir, archiveName), "bytes", size, "sha256", sum,
		"stores", len(dbSources), "config", haveConfigDir, "env", haveEnv)

	if err := prune(backupCfg.Dir, backupCfg.RetentionCount); err != nil {
		logger.Error("backup failed", "step", "prune", "error", err)
		return fmt.Errorf("backup: prune %s: %w", backupCfg.Dir, err)
	}
	return nil
}

// buildVersion returns the running binary's VCS revision (if built with Go's
// automatic VCS stamping from a git checkout), for the manifest's benefit
// when comparing a restored backup against the code that produced it. Best
// effort: an empty string (e.g. for a `go test` binary) is not an error.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
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
// and runs VACUUM INTO dst on it. dst is always a path on local disk (see
// Run), so this never hits the network-filesystem locking failure that
// motivated this package's staging design.
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

// integrityCheck opens path (expected to be a freshly VACUUM INTO'd
// snapshot, not the live database) and runs PRAGMA integrity_check,
// treating anything other than a single "ok" row as a failed backup — a
// backup nobody can restore from is worse than no backup at all, since it
// hides the fact that there is no good copy.
func integrityCheck(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()

	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("integrity_check %s: %w", path, err)
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check %s: %s", path, result)
	}
	return nil
}

// hashFile returns path's size and lowercase-hex SHA-256 digest.
func hashFile(path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// writeArchive writes m as contentDir/manifest.json, then packs every file
// under contentDir (including that manifest) into a gzip-compressed tar at
// archivePath. It returns archivePath's own size and SHA-256 digest, which
// uploadAtomic uses afterward to confirm the copy into backupCfg.Dir landed
// intact.
func writeArchive(contentDir string, m manifest, archivePath string) (sum string, size int64, err error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", 0, fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(contentDir, "manifest.json"), data, 0o644); err != nil {
		return "", 0, fmt.Errorf("write manifest: %w", err)
	}

	out, err := os.Create(archivePath)
	if err != nil {
		return "", 0, fmt.Errorf("create %s: %w", archivePath, err)
	}
	defer func() { _ = out.Close() }() // safety net on an early-return path above out.Close() below

	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)

	walkErr := filepath.WalkDir(contentDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(contentDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	})
	if walkErr != nil {
		return "", 0, fmt.Errorf("write tar entries: %w", walkErr)
	}
	if err := tw.Close(); err != nil {
		return "", 0, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", 0, fmt.Errorf("close gzip: %w", err)
	}
	if err := out.Sync(); err != nil {
		return "", 0, fmt.Errorf("sync %s: %w", archivePath, err)
	}
	if err := out.Close(); err != nil {
		return "", 0, fmt.Errorf("close %s: %w", archivePath, err)
	}

	return hashFile(archivePath)
}

// uploadAtomic copies the already-finished, already-verified archive at
// localArchive into destDir/name, going through a "<name>.part" sibling so a
// crash or network hiccup mid-copy never leaves a truncated file at the
// final name. Unlike VACUUM INTO, a plain io.Copy + rename has no locking
// requirement destDir's filesystem needs to satisfy, so this step works
// identically whether destDir is local disk or a CIFS/SMB mount — see this
// package's doc comment for why that distinction matters.
//
// After the copy, the ".part" file's own size and SHA-256 are recomputed
// and compared against wantSize/wantSum (computed from localArchive by the
// caller) before the rename — catching silent corruption introduced by the
// network copy itself, not just a short write.
func uploadAtomic(localArchive, destDir, name, wantSum string, wantSize int64) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", destDir, err)
	}

	partPath := filepath.Join(destDir, name+partSuffix)
	finalPath := filepath.Join(destDir, name)

	in, err := os.Open(localArchive)
	if err != nil {
		return fmt.Errorf("open %s: %w", localArchive, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(partPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", partPath, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(partPath)
		return fmt.Errorf("copy to %s: %w", partPath, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(partPath)
		return fmt.Errorf("sync %s: %w", partPath, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(partPath)
		return fmt.Errorf("close %s: %w", partPath, err)
	}

	gotSum, gotSize, err := hashFile(partPath)
	if err != nil {
		_ = os.Remove(partPath)
		return fmt.Errorf("verify %s: %w", partPath, err)
	}
	if gotSize != wantSize || gotSum != wantSum {
		_ = os.Remove(partPath)
		return fmt.Errorf("verify %s: size/sha256 mismatch after copy (want %d/%s, got %d/%s)",
			partPath, wantSize, wantSum, gotSize, gotSum)
	}

	if err := os.Rename(partPath, finalPath); err != nil {
		_ = os.Remove(partPath)
		return fmt.Errorf("rename %s to %s: %w", partPath, finalPath, err)
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

// prune deletes the oldest backup archives in dir beyond the most recent
// keep, and any "*.tar.gz.part" file older than stalePartAge (a leftover
// from a run that crashed mid-upload — see uploadAtomic). keep <= 0 means
// keep every archive (same convention as config.LoggingConfig.MaxBackups),
// but stale-.part cleanup still always runs.
func prune(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}

	now := time.Now()
	var archives []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, partSuffix) {
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > stalePartAge {
				_ = os.Remove(filepath.Join(dir, name))
			}
			continue
		}
		if strings.HasPrefix(name, archivePrefix) && strings.HasSuffix(name, archiveSuffix) {
			archives = append(archives, name)
		}
	}

	if keep <= 0 || len(archives) <= keep {
		return nil
	}

	sort.Strings(archives) // timestampFormat sorts lexicographically == chronologically
	for _, name := range archives[:len(archives)-keep] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
	return nil
}
