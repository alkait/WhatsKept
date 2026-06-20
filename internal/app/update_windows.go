//go:build windows

package app

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// winUpdateAsset is the stable, unversioned Windows zip alias on every release.
// /releases/latest/download/<file> always resolves to the newest tagged
// release's copy, so this URL never goes stale.
const winUpdateAsset = "https://github.com/" + repoSlug + "/releases/latest/download/WhatsKept-windows-amd64.zip"

// applyUpdate (Windows) downloads the latest release's whatskept.exe and swaps
// it in for the running binary, then schedules a relaunch.
//
// Windows won't let us overwrite or delete a running .exe, but it DOES allow
// renaming one. So the swap is: rename the running exe to <name>.exe.old,
// write the freshly downloaded exe to the original path, then spawn a detached
// helper that waits for us to exit (freeing the localhost port) and relaunches
// the new exe. The UI quits this process immediately after we return. On its
// next launch the new exe sweeps the leftover .old (see cleanupStaleUpdate).
//
// Integrity rests on HTTPS to github.com plus "the download is a valid zip
// containing whatskept.exe"; the published SHA256SUMS doesn't yet cover the
// Windows asset. If the exe lives somewhere we can't write (a read-only or
// network path), we fail before touching anything and tell the user to update
// manually.
func (s *server) applyUpdate() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the running program: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	dir := filepath.Dir(exePath)

	// Fail fast if the install directory isn't writable (e.g. running from the
	// Parallels Mac share or a protected location) before we download anything.
	probe, err := os.CreateTemp(dir, ".wk-update-*")
	if err != nil {
		return fmt.Errorf("can't update in place — %s isn't writable. Download the latest release manually and replace whatskept.exe.", dir)
	}
	probeName := probe.Name()
	probe.Close()
	os.Remove(probeName)

	// Download the release zip into memory (~42 MB).
	zipBytes, err := httpGet(winUpdateAsset, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("download update: %w", err)
	}
	exeBytes, err := extractExeFromZip(zipBytes)
	if err != nil {
		return fmt.Errorf("read update archive: %w", err)
	}

	// Stage the new exe alongside the current one (same volume, so the renames
	// below are atomic moves, not cross-device copies).
	newPath := filepath.Join(dir, ".whatskept-new.exe")
	if err := os.WriteFile(newPath, exeBytes, 0o755); err != nil {
		return fmt.Errorf("stage new version: %w", err)
	}

	oldPath := exePath + ".old"
	_ = os.Remove(oldPath) // clear any leftover from a previous update
	if err := os.Rename(exePath, oldPath); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("move the running program aside: %w", err)
	}
	if err := os.Rename(newPath, exePath); err != nil {
		os.Rename(oldPath, exePath) // roll back so we're not left without an exe
		os.Remove(newPath)
		return fmt.Errorf("install the new version: %w", err)
	}

	// Schedule the relaunch via a throwaway .bat: wait ~2s for this process to
	// exit (the UI quits us next, freeing the localhost port), start the new exe
	// in GUI mode, then delete the script. We run a .bat BY PATH rather than an
	// inline `cmd /c "<command>"` string because the layered quoting (Go's arg
	// wrapping + cmd's own /c quote-stripping + start's "" title quotes) mangles
	// the exe path otherwise. Inside a .bat, `start "" "<path>" app` parses
	// cleanly.
	bat := "@echo off\r\n" +
		"ping -n 3 127.0.0.1 >nul\r\n" +
		"start \"\" \"" + exePath + "\" app\r\n" +
		"del \"%~f0\"\r\n"
	batPath := filepath.Join(os.TempDir(), "whatskept-relaunch.bat")
	if err := os.WriteFile(batPath, []byte(bat), 0o644); err != nil {
		return fmt.Errorf("write relauncher: %w", err)
	}
	relaunch := exec.Command("cmd", "/c", batPath)
	relaunch.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := relaunch.Start(); err != nil {
		return fmt.Errorf("schedule relaunch: %w", err)
	}
	return nil
}

// cleanupStaleUpdate deletes the <exe>.old left by a prior in-place update.
// Best-effort: if the old process hasn't fully exited yet the delete may fail,
// and the next launch retries.
func cleanupStaleUpdate() {
	if exe, err := os.Executable(); err == nil {
		_ = os.Remove(exe + ".old")
	}
}

// httpGet fetches url and returns the body, following redirects (GitHub's
// /latest/download/ 302s to the asset). Fails on any non-200.
func httpGet(url string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned %s", res.Status)
	}
	return io.ReadAll(res.Body)
}

// extractExeFromZip returns the bytes of whatskept.exe inside the release zip.
func extractExeFromZip(b []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, fmt.Errorf("not a valid zip: %w", err)
	}
	for _, f := range zr.File {
		if strings.EqualFold(filepath.Base(f.Name), "whatskept.exe") {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, errors.New("whatskept.exe not found in the update archive")
}
