//go:build windows

package app

import (
	"os/exec"
	"strings"
)

// pickFolderNative shows the native Windows folder chooser.
//
// Why PowerShell? webview_go exposes no native file-dialog API, and pulling in
// a cgo COM binding just for one dialog is unjustified weight — the same
// reasoning that led the macOS path to shell out to AppleScript. Windows
// PowerShell 5.1 ships on every Win10/11 install at a fixed System32 path, and
// its System.Windows.Forms.FolderBrowserDialog is the standard system picker.
//
// The dialog requires a single-threaded apartment, hence -STA. On OK we write
// the selected path to stdout; on Cancel we write nothing, so the caller sees
// ("", nil) and the React code leaves the existing selection unchanged — the
// same contract as the macOS implementation.
func pickFolderNative() (string, error) {
	const script = `Add-Type -AssemblyName System.Windows.Forms;` +
		`$d = New-Object System.Windows.Forms.FolderBrowserDialog;` +
		`$d.Description = 'Select a folder for your WhatsKept workspace';` +
		`$d.ShowNewFolderButton = $true;` +
		`if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Out.Write($d.SelectedPath) }`

	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-Command", script)
	out, err := cmd.Output()
	if err != nil {
		// A user-cancel writes nothing and exits 0, so a non-nil error here
		// is a genuine failure to launch PowerShell, not a cancellation.
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
