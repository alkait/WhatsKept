//go:build !darwin

package app

// setDockIcon is a no-op off macOS; the Dock icon API is Cocoa-specific.
func setDockIcon() {}
