// Package envfile loads KEY=VALUE pairs from a .env-style file into the
// process environment, purely for local-development convenience. Real
// environment variables always take priority: a value already set in the
// environment is never overwritten by the file, so the exact same binary
// invocation works unchanged wherever secrets come from the real environment
// instead of a checked-out .env (systemd EnvironmentFile, a process
// supervisor, CI, etc.).
package envfile

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Load reads path and, for every KEY=VALUE line, sets the process
// environment variable KEY to VALUE unless KEY is already set. A missing
// file is not an error — it just means there's nothing to load, mirroring
// config.Load's behavior for an optional config.yaml.
func Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("envfile: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := parseLine(scanner.Text())
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("envfile: set %s: %w", key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("envfile: read %s: %w", path, err)
	}
	return nil
}

// parseLine extracts a KEY=VALUE pair from one line, or reports ok=false for
// blank lines, comments ("#..."), and lines with no "=". A leading "export "
// (common when a line is copied straight from a shell script) is stripped,
// and a value wrapped in matching single or double quotes has them removed
// so both `KEY=value` and `KEY="value"` styles work as expected.
func parseLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")

	key, value, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" {
		return "", "", false
	}

	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
	}
	return key, value, true
}
