package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"whatskept/internal/binding"
)

// recentMax is the max number of entries we keep in the recent-workspaces list.
const recentMax = 10

// recentFileName lives under the macOS-idiomatic prefs directory.
const recentFileName = "recent_workspaces.json"

// prefsDir returns the directory where whatskept stores user prefs (recent
// workspaces, last-used backup root, etc). On macOS:
//
//	~/Library/Application Support/whatskept/
//
// The directory is created on demand by callers.
func prefsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "whatskept"), nil
}

// workspaceState holds the active workspace path, behind a mutex so the
// HTTP handlers can be invoked concurrently from the webview's network
// stack without racing.
type workspaceState struct {
	mu     sync.RWMutex
	active string // empty = no workspace open
}

func newWorkspaceState() *workspaceState { return &workspaceState{} }

func (w *workspaceState) get() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.active
}

func (w *workspaceState) set(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.active = path
}

// workspaceInfo is the JSON shape returned by /api/workspace/* routes.
// Binding carries the device + WhatsApp account identity the workspace
// is tied to (nil for never-synced workspaces). The frontend uses it
// for the header label, the bound-device backup badge, and the
// Run Backup picker filter.
type workspaceInfo struct {
	Path    string           `json:"path"`
	Exists  bool             `json:"exists"`
	HasChat bool             `json:"has_chat"`
	Binding *binding.Binding `json:"binding,omitempty"`
}

func describeWorkspace(path string) workspaceInfo {
	info := workspaceInfo{Path: path}
	st, err := os.Stat(path)
	info.Exists = err == nil && st.IsDir()
	if info.Exists {
		_, err := os.Stat(filepath.Join(path, "ChatStorage.sqlite"))
		info.HasChat = err == nil
		// Binding errors are non-fatal (corrupt or missing file just
		// means the workspace is treated as unbound).
		if b, _ := binding.Load(path); b != nil {
			info.Binding = b
		}
	}
	return info
}

// loadRecent reads the recent-workspaces list. Missing or malformed file
// produces an empty list (no error).
func loadRecent() []string {
	dir, err := prefsDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, recentFileName))
	if err != nil {
		return nil
	}
	var out []string
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

// saveRecent persists the recent-workspaces list. Errors are best-effort.
func saveRecent(paths []string) error {
	dir, err := prefsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	data, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, recentFileName), data, 0o644)
}

// addRecent inserts `path` at the front of the recent list, dedupes, and
// caps to recentMax entries. Persists on success.
func addRecent(path string) {
	paths := loadRecent()
	out := make([]string, 0, len(paths)+1)
	out = append(out, path)
	for _, p := range paths {
		if p == path {
			continue
		}
		out = append(out, p)
		if len(out) >= recentMax {
			break
		}
	}
	_ = saveRecent(out)
}

// removeRecent drops `path` from the recent-workspaces list. No-op if
// the path isn't in the list. Called when the user explicitly deletes
// a workspace so its now-stale entry doesn't linger in the picker.
func removeRecent(path string) {
	paths := loadRecent()
	out := paths[:0]
	for _, p := range paths {
		if p != path {
			out = append(out, p)
		}
	}
	_ = saveRecent(out)
}

// writeBackupPasswordEnv writes a `.env` file containing
// `BACKUP_PASSWORD=<password>` into the workspace directory. The file is
// created with mode 0600 so it isn't world-readable. Mirrors the Python
// `create_workspace` behaviour.
func writeBackupPasswordEnv(workspace, password string) error {
	if password == "" {
		return errors.New("password is empty")
	}
	envPath := filepath.Join(workspace, ".env")
	content := fmt.Sprintf("BACKUP_PASSWORD=%s\n", password)
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write .env: %w", err)
	}
	return nil
}
