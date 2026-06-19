//go:build windows

package backup

import (
	"os"
	"path/filepath"
)

// candidateBackupRoots lists the known iOS-backup locations on Windows, most
// modern first. Which one is used depends on how the user's iTunes/Apple
// software was installed:
//
//   - Apple Devices app (Microsoft Store) and Store-distributed iTunes write
//     to  %USERPROFILE%\Apple\MobileSync\Backup
//   - Legacy desktop iTunes (the installer from apple.com) writes to
//     %APPDATA%\Apple Computer\MobileSync\Backup
//   - Some Store variants have also been seen at
//     %APPDATA%\Apple\MobileSync\Backup
//
// DefaultRoot() picks the first of these that exists, so a user who backed up
// with the recommended Apple Devices app is found without configuration.
func candidateBackupRoots() []string {
	var roots []string
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, "Apple", "MobileSync", "Backup"))
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		roots = append(roots,
			filepath.Join(appData, "Apple Computer", "MobileSync", "Backup"),
			filepath.Join(appData, "Apple", "MobileSync", "Backup"),
		)
	}
	return roots
}
