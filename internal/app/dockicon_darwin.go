package app

// On macOS, a bare Mach-O launched from the terminal (e.g. `make app`,
// which runs `dist/whatskept app` directly rather than the .app bundle)
// shows the generic green executable icon in the Dock and ⌘-Tab app
// switcher. There's no Info.plist / CFBundleIconFile to read an icon
// from. So we set it programmatically at runtime via
// -[NSApplication setApplicationIconImage:], which works for both the
// bare binary and the bundle. The bundle additionally ships an .icns
// (see build/make-bundle.sh) so Finder/Dock show the icon before launch.
//
// The Cocoa call lives in dockicon_darwin.m — cgo compiles the preamble
// of a .go file as plain C, so the Objective-C must sit in a .m file and
// is reached here through a C prototype.

/*
#cgo darwin LDFLAGS: -framework Cocoa
void wkpSetDockIcon(const void *data, int len);
*/
import "C"

import (
	_ "embed"
	"unsafe"
)

//go:embed web/logo.png
var appIconPNG []byte

// setDockIcon sets the running app's Dock / app-switcher icon from the
// embedded PNG. Must be called on the OS main thread after the shared
// NSApplication exists (webview.New initialises it), which runWindow
// guarantees. A no-op if the icon fails to decode.
func setDockIcon() {
	if len(appIconPNG) == 0 {
		return
	}
	C.wkpSetDockIcon(unsafe.Pointer(&appIconPNG[0]), C.int(len(appIconPNG)))
}
