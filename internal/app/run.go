// Package app implements the `whatskept app` GUI: a localhost HTTP
// server backing an embedded React UI rendered inside a native macOS
// window via WKWebView (github.com/webview/webview_go).
//
// The split mirrors the Python implementation (FastAPI + pywebview)
// closely enough that the existing `index.html` works unchanged — only
// the native bridge differs (see web/index.html for the shim).
package app

import (
	"context"
	"fmt"
	"os"
	"time"
)

// RunOptions are passed by `cmd/whatskept/app.go` to configure the
// window. Defaults are applied for any zero-value fields.
type RunOptions struct {
	Title string
	// Version is the build-time version string (e.g. "v0.1.0" or
	// "0.0.0-dev.12+abc1234"). Surfaced to the UI via /api/meta so the
	// header can show it and compare against the latest GitHub release.
	Version   string
	Width     int
	Height    int
	Resizable bool
}

// Run starts the HTTP server and opens the native window. Blocks until
// the user closes the window, then gracefully shuts the server down.
//
// MUST be called from the OS main thread on macOS — see runWindow().
func Run(opts RunOptions) error {
	if opts.Title == "" {
		opts.Title = "WhatsKept"
	}
	if opts.Width == 0 {
		opts.Width = 980
	}
	if opts.Height == 0 {
		opts.Height = 700
	}

	srv, err := newServer(opts.Version)
	if err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	srv.Start()
	fmt.Fprintf(os.Stderr, "whatskept app: serving %s\n", srv.URL())

	// Brief settle so the listener is fully ready before WebKit fetches
	// the initial page. 50ms is plenty on localhost.
	time.Sleep(50 * time.Millisecond)

	winErr := runWindow(windowOptions{
		Title:     opts.Title,
		URL:       srv.URL(),
		Width:     opts.Width,
		Height:    opts.Height,
		Resizable: opts.Resizable,
	}, pickFolderNative)

	// Always shut the server down, even if the window failed to open.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)

	return winErr
}
