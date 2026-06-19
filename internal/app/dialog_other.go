//go:build !darwin && !windows

package app

import "errors"

// pickFolderNative is a no-op off macOS/Windows (e.g. Linux), where the app
// ships no native picker. The UI's text field still lets the user type a path;
// this stub keeps the package compilable for `go vet ./...` sanity checks.
func pickFolderNative() (string, error) {
	return "", errors.New("native folder picker is only implemented on macOS and Windows")
}
