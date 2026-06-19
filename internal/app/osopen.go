package app

import (
	"os/exec"
	"runtime"
)

// fileManagerCmd builds a detached command that opens `target` in the OS
// file manager. When reveal is true the item is highlighted inside its parent
// folder (Finder's -R / Explorer's /select); otherwise `target` (a directory)
// is opened directly.
//
// Note: explorer.exe is known to exit non-zero even on success, so callers
// must treat only Start() failures as errors and ignore Wait()'s result —
// which the existing handlers already do (detached `go cmd.Wait()`).
func fileManagerCmd(target string, reveal bool) *exec.Cmd {
	switch runtime.GOOS {
	case "windows":
		if reveal {
			// `explorer /select,<path>` opens the folder with the item
			// highlighted. The comma form is a single argv token.
			return exec.Command("explorer", "/select,"+target)
		}
		return exec.Command("explorer", target)
	case "darwin":
		if reveal {
			return exec.Command("/usr/bin/open", "-R", target)
		}
		return exec.Command("/usr/bin/open", target)
	default:
		// Linux/BSD: best-effort; xdg-open has no reveal semantics, so we
		// open the parent-or-target directory either way.
		return exec.Command("xdg-open", target)
	}
}

// openURLCmd builds a command that opens an https URL in the user's default
// browser. Used by the "Get a key" / "What's new" links, which must not
// navigate the WKWebView/WebView2 away from the app itself.
func openURLCmd(url string) *exec.Cmd {
	switch runtime.GOOS {
	case "windows":
		// rundll32 …FileProtocolHandler is the canonical default-browser
		// launcher and, unlike `cmd /c start`, doesn't mangle URLs that
		// contain & or other cmd metacharacters.
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		return exec.Command("/usr/bin/open", url)
	default:
		return exec.Command("xdg-open", url)
	}
}
