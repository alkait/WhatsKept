//go:build windows

package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// detectGUIAgent finds a GUI editor on Windows via the CLI shim it installs
// on PATH (VS Code → code.cmd, Cursor → cursor.cmd, Windsurf → windsurf.cmd).
// exec.LookPath resolves these through PATHEXT. Returns the shim path, which
// buildAgentCmd uses to open the workspace folder.
func detectGUIAgent(spec agentSpec) (bool, string) {
	if spec.WinLauncher == "" {
		return false, ""
	}
	if p, err := exec.LookPath(spec.WinLauncher); err == nil {
		return true, p
	}
	return false, ""
}

// detectCLIAgent resolves a CLI agent's executable: $PATH first (LookPath
// honours PATHEXT, so claude.cmd / opencode.exe both resolve), then the common
// Windows install locations for node/npm- and curl-installed CLIs that don't
// always land on PATH.
func detectCLIAgent(spec agentSpec) (bool, string) {
	name := spec.Binary
	if name == "" {
		return false, ""
	}
	if p, err := exec.LookPath(name); err == nil {
		return true, p
	}
	var candidates []string
	if appData := os.Getenv("APPDATA"); appData != "" {
		// `npm install -g` drops <name>.cmd here.
		candidates = append(candidates,
			filepath.Join(appData, "npm", name+".cmd"),
			filepath.Join(appData, "npm", name+".exe"),
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin", name+".exe"), // native installers
			filepath.Join(home, ".bun", "bin", name+".exe"),   // bun install -g
		)
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return true, p
		}
	}
	return false, ""
}

// buildAgentCmd launches a GUI editor on its workspace folder, or a CLI agent
// in a fresh terminal window cd'd into the workspace.
//
// NOTE: the exact launch invocations are best-effort and worth confirming in
// the VM — Windows console/quoting behaviour is finicky. Detection (above) is
// what drives the "Installed" badge; these only run on the explicit "Open".
func buildAgentCmd(spec agentSpec, resolved, target string) (*exec.Cmd, error) {
	if spec.AppName != "" || spec.WinLauncher != "" {
		// GUI: the resolved shim (code.cmd etc.) opens the folder and returns.
		// .cmd files can't be CreateProcess'd directly, so go through cmd /c.
		return exec.Command("cmd", "/c", resolved, target), nil
	}

	// CLI: open a terminal in the workspace running the agent. Claude Code
	// gets --dangerously-skip-permissions so it can read the workspace + run
	// sqlite without prompting (local, read-only data the user already trusts).
	inner := winQuote(resolved)
	if spec.ID == "claude-code" || spec.Binary == "claude" {
		inner += " --dangerously-skip-permissions"
	}
	if wt, err := exec.LookPath("wt"); err == nil {
		// Windows Terminal: -d sets the working dir; `cmd /k` keeps the pane
		// open with the agent running.
		return exec.Command(wt, "-d", target, "cmd", "/k", inner), nil
	}
	// Fallback to a classic console window. `start "" /d <dir>` sets the
	// working directory; `cmd /k` keeps it open running the agent.
	return exec.Command("cmd", "/c", "start", "", "/d", target, "cmd", "/k", inner), nil
}

// buildTerminalCmd opens a terminal in the workspace so the user can run
// whatever CLI agent they prefer. Prefers Windows Terminal, falls back to cmd.
func buildTerminalCmd(target string) (*exec.Cmd, error) {
	if wt, err := exec.LookPath("wt"); err == nil {
		return exec.Command(wt, "-d", target), nil
	}
	return exec.Command("cmd", "/c", "start", "", "/d", target, "cmd"), nil
}

// winQuote wraps s in double quotes, escaping embedded double quotes by
// doubling them — sufficient for the simple paths we pass to cmd.
func winQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
