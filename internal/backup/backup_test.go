package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/archer-developer/miranda/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedSQLite creates a tiny SQLite database at path with one row, so tests
// can assert a backup file is a real, independently readable snapshot
// rather than just checking it exists.
func seedSQLite(t *testing.T, path string, value string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec("CREATE TABLE t (v TEXT)")
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO t VALUES (?)", value)
	require.NoError(t, err)
}

func readSQLiteValue(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var v string
	require.NoError(t, db.QueryRow("SELECT v FROM t").Scan(&v))
	return v
}

// soleArchive asserts backupDir contains exactly one finished archive and
// returns its path.
func soleArchive(t *testing.T, backupDir string) string {
	t.Helper()
	entries, err := os.ReadDir(backupDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "exactly one finished archive")
	name := entries[0].Name()
	require.True(t, len(name) > len(archivePrefix)+len(archiveSuffix), "unexpected entry %q", name)
	return filepath.Join(backupDir, name)
}

// extractArchive unpacks a tar.gz archive into a fresh directory under
// t.TempDir() and returns that directory's path.
func extractArchive(t *testing.T, archivePath string) string {
	t.Helper()
	dst := t.TempDir()

	f, err := os.Open(archivePath)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		target := filepath.Join(dst, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			require.NoError(t, os.MkdirAll(target, 0o755))
		case tar.TypeReg:
			require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode))
			require.NoError(t, err)
			_, err = io.Copy(out, tr)
			require.NoError(t, out.Close())
			require.NoError(t, err)
		}
	}
	return dst
}

func TestRun_SnapshotsExistingSourcesOnly(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backups")

	storageCfg := config.StorageConfig{
		SQLitePath:         filepath.Join(dataDir, "miranda.db"),
		WebAuthnSQLitePath: filepath.Join(dataDir, "webauthn.db"), // never created — feature off
		ScheduleSQLitePath: filepath.Join(dataDir, "schedule.db"),
		KeyringSQLitePath:  filepath.Join(dataDir, "keyring.db"),
		OAuthSQLitePath:    filepath.Join(dataDir, "oauth.db"), // never created — feature off
	}
	seedSQLite(t, storageCfg.SQLitePath, "miranda-data")
	seedSQLite(t, storageCfg.ScheduleSQLitePath, "schedule-data")
	seedSQLite(t, storageCfg.KeyringSQLitePath, "keyring-data")

	backupCfg := config.BackupConfig{Dir: backupDir, TempDir: t.TempDir(), RetentionCount: 7}
	require.NoError(t, Run(context.Background(), backupCfg, storageCfg, "", "", testLogger()))

	archivePath := soleArchive(t, backupDir)
	extracted := extractArchive(t, archivePath)

	require.Equal(t, "miranda-data", readSQLiteValue(t, filepath.Join(extracted, "miranda.db")))
	require.Equal(t, "schedule-data", readSQLiteValue(t, filepath.Join(extracted, "schedule.db")))
	require.Equal(t, "keyring-data", readSQLiteValue(t, filepath.Join(extracted, "keyring.db")))

	require.NoFileExists(t, filepath.Join(extracted, "webauthn.db"))
	require.NoFileExists(t, filepath.Join(extracted, "oauth.db"))

	require.FileExists(t, filepath.Join(extracted, "manifest.json"))
}

func TestRun_NoSourcesExist_NoOp(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backups")

	storageCfg := config.StorageConfig{
		SQLitePath:         filepath.Join(dataDir, "miranda.db"),
		WebAuthnSQLitePath: filepath.Join(dataDir, "webauthn.db"),
		ScheduleSQLitePath: filepath.Join(dataDir, "schedule.db"),
		KeyringSQLitePath:  filepath.Join(dataDir, "keyring.db"),
		OAuthSQLitePath:    filepath.Join(dataDir, "oauth.db"),
	}

	backupCfg := config.BackupConfig{Dir: backupDir, TempDir: t.TempDir(), RetentionCount: 7}
	require.NoError(t, Run(context.Background(), backupCfg, storageCfg, "", "", testLogger()))

	_, err := os.Stat(backupDir)
	require.True(t, os.IsNotExist(err), "backup dir should never be created when nothing exists to back up")
}

func TestRun_FailedIntegrityCheckAbortsRun(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backups")

	dbPath := filepath.Join(dataDir, "miranda.db")
	// A file that merely looks like it might be a SQLite database (wrong
	// magic header) fails VACUUM INTO's own open, which exercises the same
	// "abort rather than ship something broken" path as a genuine
	// integrity_check failure would, without needing to hand-craft a
	// corrupt-but-openable database file.
	require.NoError(t, os.WriteFile(dbPath, []byte("not a sqlite database"), 0o644))

	storageCfg := config.StorageConfig{SQLitePath: dbPath}
	backupCfg := config.BackupConfig{Dir: backupDir, TempDir: t.TempDir(), RetentionCount: 7}

	err := Run(context.Background(), backupCfg, storageCfg, "", "", testLogger())
	require.Error(t, err)

	_, statErr := os.Stat(backupDir)
	require.True(t, os.IsNotExist(statErr), "no archive should ever reach backup.dir on a failed run")
}

func TestRun_PruneKeepsOnlyMostRecentRetentionCount(t *testing.T) {
	backupDir := t.TempDir()

	// Fabricate five already-"uploaded" archive files with distinct,
	// chronologically-ordered timestamp names, rather than calling Run
	// five times (which would need real sleeps between ticks to get
	// distinct second-resolution timestamps).
	var names []string
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		name := archivePrefix + base.Add(time.Duration(i)*time.Hour).Format(timestampFormat) + archiveSuffix
		require.NoError(t, os.WriteFile(filepath.Join(backupDir, name), []byte("x"), 0o644))
		names = append(names, name)
	}

	require.NoError(t, prune(backupDir, 2))

	entries, err := os.ReadDir(backupDir)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	var kept []string
	for _, e := range entries {
		kept = append(kept, e.Name())
	}
	require.ElementsMatch(t, names[3:], kept, "only the two newest archives survive")
}

func TestRun_PrunesOldRunsAfterEachSuccessfulBackup(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backups")
	storageCfg := config.StorageConfig{SQLitePath: filepath.Join(dataDir, "miranda.db")}
	seedSQLite(t, storageCfg.SQLitePath, "v")

	backupCfg := config.BackupConfig{Dir: backupDir, TempDir: t.TempDir(), RetentionCount: 2}

	for i := 0; i < 3; i++ {
		require.NoError(t, Run(context.Background(), backupCfg, storageCfg, "", "", testLogger()))
		time.Sleep(time.Second) // force a distinct second-resolution archive name
	}

	entries, err := os.ReadDir(backupDir)
	require.NoError(t, err)
	require.Len(t, entries, 2, "retention should have pruned down to 2 archives")
}

func TestRun_StalePartFileIsCleanedUp(t *testing.T) {
	backupDir := t.TempDir()

	stale := filepath.Join(backupDir, archivePrefix+"20260101-000000"+archiveSuffix+partSuffix)
	require.NoError(t, os.WriteFile(stale, []byte("leftover"), 0o644))
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(stale, old, old))

	fresh := filepath.Join(backupDir, archivePrefix+"20260102-000000"+archiveSuffix+partSuffix)
	require.NoError(t, os.WriteFile(fresh, []byte("in-progress"), 0o644))

	require.NoError(t, prune(backupDir, 7))

	require.NoFileExists(t, stale, "a .part file older than stalePartAge must be removed")
	require.FileExists(t, fresh, "a recent .part file must be left alone — it may belong to a run in flight")
}

func TestRun_CopiesConfigDirAndEnvFile(t *testing.T) {
	backupDir := filepath.Join(t.TempDir(), "backups")

	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "llm.yaml"), []byte("llm: {}\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "sub", "nested.yaml"), []byte("a: 1\n"), 0o644))

	envDir := t.TempDir()
	envPath := filepath.Join(envDir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte("ANTHROPIC_API_KEY=secret\n"), 0o600))

	storageCfg := config.StorageConfig{} // no SQLite stores exist — only config/.env this run
	backupCfg := config.BackupConfig{Dir: backupDir, TempDir: t.TempDir(), RetentionCount: 7}
	require.NoError(t, Run(context.Background(), backupCfg, storageCfg, configDir, envPath, testLogger()))

	extracted := extractArchive(t, soleArchive(t, backupDir))

	got, err := os.ReadFile(filepath.Join(extracted, "config", "llm.yaml"))
	require.NoError(t, err)
	require.Equal(t, "llm: {}\n", string(got))

	gotNested, err := os.ReadFile(filepath.Join(extracted, "config", "sub", "nested.yaml"))
	require.NoError(t, err)
	require.Equal(t, "a: 1\n", string(gotNested))

	gotEnvPath := filepath.Join(extracted, ".env")
	gotEnv, err := os.ReadFile(gotEnvPath)
	require.NoError(t, err)
	require.Equal(t, "ANTHROPIC_API_KEY=secret\n", string(gotEnv))

	fi, err := os.Stat(gotEnvPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "restrictive .env permissions must survive the copy")
}

func TestRun_MissingConfigDirAndEnv_SkippedNotErrored(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backups")
	storageCfg := config.StorageConfig{SQLitePath: filepath.Join(dataDir, "miranda.db")}
	seedSQLite(t, storageCfg.SQLitePath, "v")

	backupCfg := config.BackupConfig{Dir: backupDir, TempDir: t.TempDir(), RetentionCount: 7}
	require.NoError(t, Run(context.Background(), backupCfg, storageCfg,
		filepath.Join(dataDir, "no-such-config-dir"), filepath.Join(dataDir, "no-such-.env"), testLogger()))

	extracted := extractArchive(t, soleArchive(t, backupDir))

	require.NoDirExists(t, filepath.Join(extracted, "config"))
	require.NoFileExists(t, filepath.Join(extracted, ".env"))
}

func TestRun_RetentionZeroKeepsEverything(t *testing.T) {
	backupDir := t.TempDir()
	for i := 0; i < 3; i++ {
		name := archivePrefix + time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC).Format(timestampFormat) + archiveSuffix
		require.NoError(t, os.WriteFile(filepath.Join(backupDir, name), []byte("x"), 0o644))
	}

	require.NoError(t, prune(backupDir, 0))

	entries, err := os.ReadDir(backupDir)
	require.NoError(t, err)
	require.Len(t, entries, 3)
}

func TestRun_UploadVerifiesIntegrityAfterCopy(t *testing.T) {
	// A direct test of uploadAtomic's own round-trip check: if the bytes
	// that land at the destination don't match what was staged, the
	// ".part" file must be removed and the final name must never appear.
	dataDir := t.TempDir()
	destDir := filepath.Join(t.TempDir(), "backups")

	localArchive := filepath.Join(dataDir, "miranda-20260101-000000.tar.gz")
	require.NoError(t, os.WriteFile(localArchive, []byte("archive-bytes"), 0o644))

	err := uploadAtomic(localArchive, destDir, "miranda-20260101-000000.tar.gz", "wrong-sha256-does-not-matter", 999)
	require.Error(t, err)

	require.NoFileExists(t, filepath.Join(destDir, "miranda-20260101-000000.tar.gz"))
	require.NoFileExists(t, filepath.Join(destDir, "miranda-20260101-000000.tar.gz.part"))
}
