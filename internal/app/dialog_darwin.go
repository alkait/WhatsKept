//go:build darwin

package app

import (
	"errors"
	"os/exec"
	"strings"
)

// pickFolderNative shows a native macOS folder-chooser via AppleScript.
//
// Why AppleScript? webview_go does not expose a native file-dialog API,
// and pulling in a separate cgo binding (e.g. github.com/sqweek/dialog)
// just for one dialog is unjustified weight. AppleScript's
// `choose folder` is already on every macOS install, returns immediately,
// and gives the standard system folder picker.
//
// Returns ("", nil) if the user cancels — the React code treats that as
// "leave the existing value unchanged".
func pickFolderNative() (string, error) {
	const script = `POSIX path of (choose folder with prompt "Select a folder for your WhatsKept workspace")`
	cmd := exec.Command("osascript", "-e", script)
	out, err := cmd.Output()
	if err != nil {
		// User-cancel emits a non-zero exit. Distinguish from real
		// errors by looking at stderr for "User canceled".
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if strings.Contains(string(exitErr.Stderr), "User canceled") {
				return "", nil
			}
		}
		return "", err
	}
	picked := strings.TrimSpace(string(out))
	// AppleScript returns paths with a trailing slash; strip it for
	// consistency with what the user typed in the workspace picker.
	picked = strings.TrimSuffix(picked, "/")
	return picked, nil
}
