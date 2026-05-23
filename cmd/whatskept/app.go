package main

import (
	"runtime"

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
		width      int
		height     int
		resizable  bool
	)

	cmd := &cobra.Command{
		Use:   "app",
		Short: "Open the WhatsKept GUI",
		Long: `Opens the WhatsKept native window.

The window hosts a small React UI backed by an internal HTTP server on
localhost. No external services are contacted; nothing leaves the
machine. Closing the window terminates the server.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.Run(app.RunOptions{
				Title:     "WhatsKept",
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
