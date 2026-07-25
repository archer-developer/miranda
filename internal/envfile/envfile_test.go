package envfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestLoad_SetsUnsetVariablesFromFile(t *testing.T) {
	os.Unsetenv("ENVFILE_TEST_NEW_VAR")
	t.Cleanup(func() { os.Unsetenv("ENVFILE_TEST_NEW_VAR") })

	path := writeEnvFile(t, "ENVFILE_TEST_NEW_VAR=from-file\n")
	require.NoError(t, Load(path))

	require.Equal(t, "from-file", os.Getenv("ENVFILE_TEST_NEW_VAR"))
}

func TestLoad_DoesNotOverrideExistingEnvironmentVariable(t *testing.T) {
	t.Setenv("ENVFILE_TEST_EXISTING_VAR", "from-real-env")

	path := writeEnvFile(t, "ENVFILE_TEST_EXISTING_VAR=from-file\n")
	require.NoError(t, Load(path))

	require.Equal(t, "from-real-env", os.Getenv("ENVFILE_TEST_EXISTING_VAR"))
}

func TestLoad_MissingFileIsNotAnError(t *testing.T) {
	require.NoError(t, Load(filepath.Join(t.TempDir(), "does-not-exist.env")))
}

func TestLoad_SkipsBlankLinesAndComments(t *testing.T) {
	os.Unsetenv("ENVFILE_TEST_COMMENTED")
	t.Cleanup(func() { os.Unsetenv("ENVFILE_TEST_COMMENTED") })

	path := writeEnvFile(t, "\n# a comment\n  \nENVFILE_TEST_COMMENTED=value\n# ENVFILE_TEST_COMMENTED=ignored\n")
	require.NoError(t, Load(path))

	require.Equal(t, "value", os.Getenv("ENVFILE_TEST_COMMENTED"))
}

func TestLoad_StripsQuotesAndExportPrefix(t *testing.T) {
	os.Unsetenv("ENVFILE_TEST_QUOTED")
	os.Unsetenv("ENVFILE_TEST_EXPORTED")
	t.Cleanup(func() {
		os.Unsetenv("ENVFILE_TEST_QUOTED")
		os.Unsetenv("ENVFILE_TEST_EXPORTED")
	})

	path := writeEnvFile(t, `ENVFILE_TEST_QUOTED="quoted value"`+"\n"+`export ENVFILE_TEST_EXPORTED=exported-value`+"\n")
	require.NoError(t, Load(path))

	require.Equal(t, "quoted value", os.Getenv("ENVFILE_TEST_QUOTED"))
	require.Equal(t, "exported-value", os.Getenv("ENVFILE_TEST_EXPORTED"))
}

func TestLoad_IgnoresLineWithNoEquals(t *testing.T) {
	os.Unsetenv("NOT_A_VARIABLE")
	path := writeEnvFile(t, "this line has no equals sign\n")
	require.NoError(t, Load(path))
}
