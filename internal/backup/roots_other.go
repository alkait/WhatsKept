//go:build !darwin && !windows

package backup

import (
	"os"
	"path/filepath"
)

// candidateBackupRoots has no canonical OS location off macOS/Windows; mirror
// the macOS layout under $HOME so the package stays compilable and usable with
// an explicitly-passed root (e.g. `go vet ./...` on Linux, or tests).
func candidateBackupRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, "Library", "Application Support", "MobileSync", "Backup")}
}
