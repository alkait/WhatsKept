//go:build windows

package app

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// buildAgentCmd launches a GUI editor via the CLI shim it installs on PATH
// (VS Code → code.cmd, Cursor → cursor.cmd). URL-scheme apps (Claude, Codex)
// are handled by the caller via openURLCmd, so this only covers WinLauncher-
// based GUI editors.
//
// .cmd shims can't be CreateProcess'd directly, so we go through `cmd /c`. The
// shim opens the folder and returns. When the editor isn't installed / not on
// PATH, cmd exits non-zero with "'code' is not recognized…", which the caller
// surfaces to the user.
func buildAgentCmd(spec agentSpec, target string) (*exec.Cmd, error) {
	return exec.Command("cmd", "/c", spec.WinLauncher, target), nil
}

// buildTerminalCmd opens a terminal in the workspace so the user can run
// whatever CLI agent they prefer. Prefers Windows Terminal, falls back to cmd.
func buildTerminalCmd(target string) (*exec.Cmd, error) {
	if wt, err := exec.LookPath("wt"); err == nil {
		return exec.Command(wt, "-d", target), nil
	}
	return exec.Command("cmd", "/c", "start", "", "/d", target, "cmd"), nil
}

// deepLinkAvailable reports whether a custom URL scheme (codex, claude) has a
// live handler on this machine. The Windows deep-link opener (rundll32) always
// exits 0, so the launch itself can't tell us a scheme is unhandled — we check
// before launching so the caller can show a friendly "is it installed?" error.
//
// A bare protocol key is NOT proof of installation: an uninstall leaves
// HKCR\<scheme> behind. We require a *live* handler — either a classic
// shell\open\command whose exe exists, or an installed Store/MSIX package whose
// name contains the scheme (e.g. "codex" → "OpenAI.Codex").
func deepLinkAvailable(scheme string) bool {
	if scheme == "" {
		return false
	}
	return classicHandlerExeExists(scheme) || appxPackageInstalled(scheme)
}

// classicHandlerExeExists checks HKCR\<scheme>\shell\open\command and confirms
// the executable it names is actually on disk (a stale registration left by an
// uninstall has no command, or points to a file that's gone).
func classicHandlerExeExists(scheme string) bool {
	out, err := hidden(exec.Command("reg", "query", `HKCR\`+scheme+`\shell\open\command`, "/ve")).Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		i := strings.Index(line, "REG_SZ")
		if i < 0 {
			continue
		}
		exe := firstToken(strings.TrimSpace(line[i+len("REG_SZ"):]))
		if exe == "" {
			return false
		}
		st, statErr := os.Stat(exe)
		return statErr == nil && !st.IsDir()
	}
	return false
}

// appxPackageInstalled reports whether a Store/MSIX package whose name contains
// the scheme is installed for the current user — the reliable signal for Store
// apps, which register the protocol via package activation, not a command.
func appxPackageInstalled(scheme string) bool {
	script := fmt.Sprintf(
		`if (Get-AppxPackage | Where-Object { $_.Name -match '%s' }) { exit 0 } else { exit 1 }`, scheme)
	return hidden(exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)).Run() == nil
}

// firstToken extracts the leading executable path from a shell command string
// like `"C:\Apps\App.exe" "%1"` or `C:\Apps\App.exe %1`.
func firstToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if s[0] == '"' {
		if end := strings.IndexByte(s[1:], '"'); end >= 0 {
			return s[1 : 1+end]
		}
		return strings.TrimPrefix(s, `"`)
	}
	if sp := strings.IndexByte(s, ' '); sp >= 0 {
		return s[:sp]
	}
	return s
}

// hidden tags a command so it spawns with no console window — without this,
// reg.exe / powershell flash a console on a GUI app.
func hidden(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return cmd
}
