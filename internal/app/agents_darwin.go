//go:build darwin

package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// detectGUIAgent walks /Applications then ~/Applications, mirroring
// Spotlight's lookup order. Returns the .app bundle path.
func detectGUIAgent(spec agentSpec) (bool, string) {
	candidates := []string{
		filepath.Join("/Applications", spec.AppName+".app"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "Applications", spec.AppName+".app"))
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return true, p
		}
	}
	return false, ""
}

// detectCLIAgent walks the common macOS install locations for a
// developer-installed binary, then falls back to $PATH. We can't rely solely
// on exec.LookPath because when whatskept is launched from Finder its PATH is
// the minimal LaunchServices default — it doesn't include /opt/homebrew/bin,
// ~/.npm-global/bin, or ~/.claude/local where `claude` actually lives.
func detectCLIAgent(spec agentSpec) (bool, string) {
	name := spec.Binary
	var candidates []string
	if p, err := exec.LookPath(name); err == nil {
		candidates = append(candidates, p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".claude", "local", name), // Anthropic's native installer
			filepath.Join(home, ".opencode", "bin", name), // opencode's `curl … | bash` installer
			filepath.Join(home, ".npm-global", "bin", name),
			filepath.Join(home, ".local", "bin", name),
			filepath.Join(home, ".volta", "bin", name),
			filepath.Join(home, ".bun", "bin", name),   // `bun install -g …` (e.g. opencode-ai)
			filepath.Join(home, ".deno", "bin", name),  // `deno install …`
			filepath.Join(home, ".cargo", "bin", name), // `cargo install …`
		)
	}
	candidates = append(candidates,
		"/opt/homebrew/bin/"+name,
		"/usr/local/bin/"+name,
		"/usr/bin/"+name,
	)
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return true, p
		}
	}
	return false, ""
}

// buildAgentCmd launches a GUI editor via LaunchServices (`open -a`) or a CLI
// agent in a fresh Terminal window via AppleScript.
func buildAgentCmd(spec agentSpec, resolved, target string) (*exec.Cmd, error) {
	if spec.AppName != "" {
		// GUI: `open -a` goes through macOS LaunchServices, which (a) brings
		// the app to focus if it's already running, (b) handles single-
		// instance apps correctly, and (c) doesn't tie the agent's lifetime
		// to whatskept. A bare exec on the bundle's MacOS binary would launch
		// a *new* instance every time and inherit our environment, both wrong
		// for an editor.
		return exec.Command("/usr/bin/open", "-a", resolved, target), nil
	}

	// CLI: launch a new Terminal window cd'd into the workspace and run the
	// binary. We use AppleScript instead of `open -a Terminal` because we need
	// to chain `cd <workspace> && exec <binary>` — `open -a` only opens the
	// directory, it doesn't run a command.
	//
	// Cold-launch quirk: if Terminal isn't already running, the `tell` block
	// launches it and Terminal honours its "New window with default profile"
	// startup preference by opening an empty window. A subsequent bare `do
	// script` then opens a *second* window, leaving the first empty. We reuse
	// window 1 when we detect a cold launch; if startup-open is "No window",
	// we fall back to a plain `do script`.
	//
	// Claude Code is launched with --dangerously-skip-permissions so it can
	// read the workspace + run sqlite3 / open without prompting on every call
	// (the workspace is local, read-only data the user already trusts).
	launch := shellQuote(resolved)
	if spec.ID == "claude-code" || spec.Binary == "claude" {
		launch += " --dangerously-skip-permissions"
	}
	shellCmd := fmt.Sprintf("cd %s && clear && exec %s", shellQuote(target), launch)
	quoted := appleScriptQuote(shellCmd)
	ascript := "tell application \"Terminal\"\n" +
		"set wasRunning to running\n" +
		"activate\n" +
		"if wasRunning then\n" +
		"do script " + quoted + "\n" +
		"else\n" +
		"repeat 20 times\n" +
		"if (count of windows) > 0 then exit repeat\n" +
		"delay 0.05\n" +
		"end repeat\n" +
		"if (count of windows) > 0 then\n" +
		"do script " + quoted + " in window 1\n" +
		"else\n" +
		"do script " + quoted + "\n" +
		"end if\n" +
		"end if\n" +
		"end tell"
	return exec.Command("/usr/bin/osascript", "-e", ascript), nil
}

// buildTerminalCmd opens the workspace directory in macOS Terminal so the user
// can pick whatever CLI agent they prefer. Terminal.app ships with macOS, so
// no detection step is needed.
func buildTerminalCmd(target string) (*exec.Cmd, error) {
	return exec.Command("/usr/bin/open", "-a", "Terminal", target), nil
}

// shellQuote wraps s in single quotes, escaping any embedded single quotes by
// closing the quoted run, emitting an escaped quote, and reopening — the
// standard POSIX-shell idiom.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
