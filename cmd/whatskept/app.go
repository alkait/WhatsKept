package main

import (
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"whatskept/internal/app"
)

// init() runs before main() and locks this goroutine to the OS main
// thread. macOS requires WKWebView and NSApplication to live on the
// main thread; without LockOSThread the Go runtime is free to migrate
// our goroutine and the window will refuse to launch (or worse,
// crash with "Cocoa not on main thread" panics).
func init() {
	runtime.LockOSThread()
}

func newAppCmd() *cobra.Command {
	var (
		width     int
		height    int
		resizable bool
	)

	cmd := &cobra.Command{
		Use:   "app",
		Short: "Open the WhatsKept GUI",
		Long: `Opens the WhatsKept native window.

The window hosts a small React UI backed by an internal HTTP server on
localhost. No external services are contacted; nothing leaves the
machine. Closing the window terminates the server.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Drop the stray console window on a Windows double-click launch
			// (no-op elsewhere, and when run from a real terminal). Must
			// happen before the window opens.
			hideConsoleIfOwned()

			// Single-instance (Windows): if a copy is already running, bring
			// its window to the front and exit instead of opening a duplicate.
			// No-op elsewhere (macOS uses LaunchServices).
			if !ensureSingleInstance() {
				return nil
			}

			// Title shows the running version so a user with two
			// builds open (e.g. a tag release alongside a fresh dev
			// preview) can tell them apart at a glance. Em dash so
			// it visually separates from the brand without looking
			// like a hyphenated word.
			//
			// Tag pushes (refs/tags/v0.1.0) feed Version through with
			// a leading 'v', branch pushes feed semver without one.
			// We always render exactly one 'v' regardless of input.
			return app.Run(app.RunOptions{
				Title:     "WhatsKept — v" + strings.TrimPrefix(Version, "v"),
				Version:   Version,
				Width:     width,
				Height:    height,
				Resizable: resizable,
			})
		},
	}

	cmd.Flags().IntVar(&width, "width", 980, "Initial window width")
	cmd.Flags().IntVar(&height, "height", 700, "Initial window height")
	cmd.Flags().BoolVar(&resizable, "resizable", true, "Allow window resize")
	return cmd
}
