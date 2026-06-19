//go:build !windows

package main

// hideConsoleIfOwned is a no-op off Windows: macOS/Linux GUI launches don't
// allocate a stray console window.
func hideConsoleIfOwned() {}
