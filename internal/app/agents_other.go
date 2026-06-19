//go:build !darwin && !windows

package app

import (
	"errors"
	"os/exec"
)

// On platforms other than macOS/Windows the app ships no agent integration.
// These stubs keep the package compilable (e.g. `go vet ./...` on Linux);
// detection reports "not installed" and launches return a clear error.

func detectGUIAgent(spec agentSpec) (bool, string) { return false, "" }

func detectCLIAgent(spec agentSpec) (bool, string) { return false, "" }

func buildAgentCmd(spec agentSpec, resolved, target string) (*exec.Cmd, error) {
	return nil, errors.New("launching agents is only supported on macOS and Windows")
}

func buildTerminalCmd(target string) (*exec.Cmd, error) {
	return nil, errors.New("opening a terminal is only supported on macOS and Windows")
}
