//go:build !darwin && !windows

package app

import (
	"errors"
	"os/exec"
)

// On platforms other than macOS/Windows the app ships no agent integration.
// These stubs keep the package compilable (e.g. `go vet ./...` on Linux);
// launches return a clear error.

func buildAgentCmd(spec agentSpec, target string) (*exec.Cmd, error) {
	return nil, errors.New("launching agents is only supported on macOS and Windows")
}

func buildTerminalCmd(target string) (*exec.Cmd, error) {
	return nil, errors.New("opening a terminal is only supported on macOS and Windows")
}

func deepLinkAvailable(scheme string) bool { return true }
