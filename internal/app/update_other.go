//go:build !darwin && !windows

package app

import "errors"

// applyUpdate has no implementation off macOS/Windows (Linux ships no GUI
// build today). handleUpdateCheck never reports an update on these platforms
// in practice, so this is only a defensive stub.
func (s *server) applyUpdate() error {
	return errors.New("in-app update is only supported on macOS and Windows")
}

// cleanupStaleUpdate is a no-op off macOS/Windows.
func cleanupStaleUpdate() {}
