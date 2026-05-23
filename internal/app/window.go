package app

import (
	"fmt"
	"runtime"

	webview "github.com/webview/webview_go"
)

// windowOptions controls the native window the app shell launches.
type windowOptions struct {
	Title     string
	URL       string
	Width     int
	Height    int
	Resizable bool
}

// pickFolderFunc is the type used to bind a native folder-picker into
// the JS runtime. The Go-side implementation lives outside this file
// because folder pickers are platform-specific (we use AppleScript on
// macOS to avoid an additional cgo dep).
type pickFolderFunc func() (string, error)

// runWindow opens the native macOS window pointing at `opts.URL`.
//
// This MUST be called from the OS main thread on macOS — webview_go's
// underlying WKWebView/NSApplication are main-thread-only. The caller
// (cmd/whatskept/app.go) is responsible for invoking runtime.LockOSThread
// at startup; we re-assert it here for safety.
//
// Blocks until the user closes the window. Returns nil on clean close.
func runWindow(opts windowOptions, pickFolder pickFolderFunc) error {
	runtime.LockOSThread()

	debug := false // toggle to enable WebKit dev tools
	w := webview.New(debug)
	if w == nil {
		return fmt.Errorf("webview: could not create window (is WebKit available?)")
	}
	defer w.Destroy()

	w.SetTitle(opts.Title)
	var hint webview.Hint = webview.HintNone
	if !opts.Resizable {
		hint = webview.HintFixed
	}
	w.SetSize(opts.Width, opts.Height, hint)

	// Bind the native folder picker. Returning an error from the bound
	// function surfaces it as a rejected promise on the JS side, which
	// the existing React code already handles (it just leaves the
	// selection unchanged).
	if pickFolder != nil {
		err := w.Bind("_wkpPickFolder", func() (string, error) {
			return pickFolder()
		})
		if err != nil {
			return fmt.Errorf("bind pickFolder: %w", err)
		}
	}

	w.Navigate(opts.URL)
	w.Run() // blocks
	return nil
}
