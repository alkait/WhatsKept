package app

import (
	"fmt"
	"os/exec"
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

	// webview.New initialised the shared NSApplication; set our Dock /
	// app-switcher icon now so a bare-binary launch (`make app`) shows
	// the brand icon instead of the generic executable glyph.
	setDockIcon()

	w.SetTitle(opts.Title)
	var hint webview.Hint = webview.HintNone
	if !opts.Resizable {
		hint = webview.HintFixed
	}
	w.SetSize(opts.Width, opts.Height, hint)

	// Give the window/taskbar the app icon from the exe resources (Windows;
	// no-op elsewhere). webview doesn't assign one, so the taskbar would
	// otherwise show a generic glyph.
	applyWindowIcon(w.Window())

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

	// Cmd+Q shim: webview_go does not install an NSApplication main
	// menu, so macOS has no menu item to dispatch ⌘Q to and the
	// keystroke is silently swallowed. We work around that by listening
	// for ⌘Q in the React app (see web/index.html) and calling this
	// bound function to request a clean shutdown. w.Terminate() makes
	// w.Run() return below, after which Run() in run.go does the
	// graceful HTTP-server shutdown — same path as closing the window
	// with the red dot, so no on-close cleanup is skipped.
	if err := w.Bind("_wkpQuit", func() {
		w.Terminate()
	}); err != nil {
		return fmt.Errorf("bind quit: %w", err)
	}

	// Open the Full Disk Access pane in System Settings. iOS backups live
	// in a protected directory, so without FDA we cannot read the backup
	// root; this deep-link drops the user straight onto the right pane.
	if err := w.Bind("_wkpOpenFullDiskAccess", func() {
		_ = exec.Command("/usr/bin/open", "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles").Start()
	}); err != nil {
		return fmt.Errorf("bind openFullDiskAccess: %w", err)
	}

	w.Navigate(opts.URL)
	w.Run() // blocks
	return nil
}
