//go:build darwin

package backup

import (
	"os"
	"path/filepath"
)

// candidateBackupRoots returns the one canonical macOS iOS-backup location,
// ~/Library/Application Support/MobileSync/Backup. Returning a single entry
// preserves the pre-port behaviour exactly: DefaultRoot() resolves to this
// path whether or not it exists yet (idevicebackup2 creates it on first run).
func candidateBackupRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, "Library", "Application Support", "MobileSync", "Backup")}
}
