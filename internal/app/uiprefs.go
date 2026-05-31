package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// uiPrefsFileName holds small, machine-local UI preferences that must
// survive app restarts. It can't live in the browser's localStorage:
// the server binds a fresh random port on every launch (see newServer),
// so each restart is a new origin and localStorage starts empty.
const uiPrefsFileName = "ui_prefs.json"

// uiPrefs is the JSON shape persisted under prefsDir()/ui_prefs.json and
// returned/accepted by /api/prefs/ui.
type uiPrefs struct {
	// AgentChoice is the id of the last agent picked in the Agents tab
	// dropdown ("__terminal__" for the Terminal escape hatch). Empty
	// means "no choice yet" — the UI falls back to the first installed.
	AgentChoice string `json:"agent_choice,omitempty"`
}

// loadUIPrefs reads the prefs file. Missing or malformed file yields a
// zero-value uiPrefs (no error) so the UI just uses its defaults.
func loadUIPrefs() uiPrefs {
	dir, err := prefsDir()
	if err != nil {
		return uiPrefs{}
	}
	data, err := os.ReadFile(filepath.Join(dir, uiPrefsFileName))
	if err != nil {
		return uiPrefs{}
	}
	var p uiPrefs
	if err := json.Unmarshal(data, &p); err != nil {
		return uiPrefs{}
	}
	return p
}

// saveUIPrefs persists the prefs file, creating prefsDir() on demand.
func saveUIPrefs(p uiPrefs) error {
	dir, err := prefsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, uiPrefsFileName), data, 0o644)
}
