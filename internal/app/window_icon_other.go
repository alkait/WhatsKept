//go:build !windows

package app

import "unsafe"

// applyWindowIcon is a no-op off Windows. macOS sets its Dock/app-switcher
// icon via setDockIcon (dockicon_darwin.go); the exe-embedded-resource icon
// path is Windows-specific.
func applyWindowIcon(win unsafe.Pointer) {}
