//go:build darwin

package app

import "os/exec"

// buildAgentCmd launches a GUI editor via LaunchServices (`open -a`). URL-scheme
// apps (Claude, Codex) are handled by the caller via openURLCmd, so this only
// covers AppName-based GUI editors (VS Code, Cursor).
//
// `open -a <AppName>` resolves the app by name through LaunchServices, brings an
// already-running instance to focus, and detaches the app from our process. A
// bare exec on the bundle's MacOS binary would spawn a duplicate and inherit
// our minimal environment. When the app isn't installed, `open` exits non-zero
// with "Unable to find application named '<AppName>'" — which the caller
// surfaces to the user.
func buildAgentCmd(spec agentSpec, target string) (*exec.Cmd, error) {
	return exec.Command("/usr/bin/open", "-a", spec.AppName, target), nil
}

// buildTerminalCmd opens the workspace directory in macOS Terminal so the user
// can run whatever CLI agent they prefer. Terminal.app ships with macOS, so no
// detection step is needed.
func buildTerminalCmd(target string) (*exec.Cmd, error) {
	return exec.Command("/usr/bin/open", "-a", "Terminal", target), nil
}

// deepLinkAvailable is a no-op on macOS: `open <url>` already exits non-zero for
// an unregistered scheme, so the caller learns of a failure from the launch
// itself and doesn't need a pre-check.
func deepLinkAvailable(scheme string) bool { return true }
