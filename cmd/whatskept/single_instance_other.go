//go:build !windows

package main

// ensureSingleInstance is a no-op off Windows. On macOS the .app already gets
// single-instance behaviour from LaunchServices (a second `open` activates the
// running copy rather than launching another). Always returns true: proceed.
func ensureSingleInstance() bool { return true }
