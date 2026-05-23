// Package secrets resolves the iOS backup password.
//
// Resolution order, mirroring the existing Python `whatskept.secrets` semantics:
//  1. BACKUP_PASSWORD environment variable.
//  2. A `.env` file (KEY=VALUE per line) found by walking from a given start
//     directory toward the filesystem root. First match wins.
//
// Never prompts interactively. Returns ErrNotFound if nothing yields a
// non-empty BACKUP_PASSWORD.
package secrets

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	envKey      = "BACKUP_PASSWORD"
	envFileName = ".env"
)

// ErrNotFound is returned when no backup password can be located.
var ErrNotFound = errors.New("BACKUP_PASSWORD not found")

// GetBackupPassword returns the backup password, or an error.
//
// `searchFrom` is the directory at which the upward `.env` search begins.
// Pass an empty string to default to the current working directory.
func GetBackupPassword(searchFrom string) (string, error) {
	if v := os.Getenv(envKey); v != "" {
		return v, nil
	}

	start := searchFrom
	if start == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getwd: %w", err)
		}
		start = cwd
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("abs %q: %w", start, err)
	}

	envPath := findEnvFile(abs)
	if envPath != "" {
		parsed, err := parseEnvFile(envPath)
		if err == nil {
			if v, ok := parsed[envKey]; ok && v != "" {
				return v, nil
			}
		}
	}

	return "", fmt.Errorf(
		"%w\n  Looked at: $%s environment variable, and a `.env` file in %s "+
			"and every parent directory.\n"+
			"  Fix: create a `.env` file containing `%s=<your-backup-password>` "+
			"in your workspace, or set $%s in your shell.",
		ErrNotFound, envKey, abs, envKey, envKey,
	)
}

// findEnvFile walks from `start` toward `/` looking for the first `.env` file.
// Returns "" if none is found.
func findEnvFile(start string) string {
	cur := start
	for {
		candidate := filepath.Join(cur, envFileName)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

// parseEnvFile is a minimal KEY=VALUE parser. Lines starting with `#` are
// comments. Surrounding balanced single/double quotes on the value are
// stripped. Lines without `=` are silently ignored.
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		value := strings.TrimSpace(line[eq+1:])
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if first == last && (first == '\'' || first == '"') {
				value = value[1 : len(value)-1]
			}
		}
		if key != "" {
			out[key] = value
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
