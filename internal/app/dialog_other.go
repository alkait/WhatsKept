//go:build !darwin

package app

import "errors"

// pickFolderNative is a no-op on non-Darwin builds. The Go port currently
// targets macOS; this stub keeps the package compilable in case someone
// runs `go vet ./...` on Linux for sanity checks.
func pickFolderNative() (string, error) {
	return "", errors.New("native folder picker is only implemented on macOS")
}
