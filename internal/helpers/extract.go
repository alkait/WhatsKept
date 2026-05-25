package helpers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
)

const (
	cacheSubpath    = "Library/Caches/whatskept/bin"
	hashMarker      = ".bundle-hash"
	embedRoot       = "bundle"
	IdeviceID       = "idevice_id"
	IdeviceBackup   = "idevicebackup2"
	WhatskeptVision = "whatskept-vision" // Swift wrapper over Vision.framework (OCR + classify)
	WhisperCli      = "whisper-cli"      // ggerganov/whisper.cpp CLI (Metal-accelerated)
)

// extractMu serialises Path() callers across goroutines so concurrent
// HTTP handlers don't race during the first cold extract.
var extractMu sync.Mutex

// cachedDir memoises the extracted-bundle path after a successful
// extract or up-to-date check, avoiding redundant hash work on each
// subsequent helpers.Path() call within a single process.
var cachedDir string

// Path returns the directory containing the runtime-extracted helper
// binaries (idevice_id, idevicebackup2, and dylibs).
//
// On first call per process, the embedded bundle is hashed and either
// extracted (cold cache or stale) or accepted (hash matches marker).
// On subsequent calls it returns the memoised path.
func Path() (string, error) {
	extractMu.Lock()
	defer extractMu.Unlock()

	if cachedDir != "" {
		return cachedDir, nil
	}

	dir, err := cacheDir()
	if err != nil {
		return "", err
	}

	want, err := bundleHash()
	if err != nil {
		return "", fmt.Errorf("hash embedded bundle: %w", err)
	}

	got, _ := os.ReadFile(filepath.Join(dir, hashMarker))
	if string(got) == want {
		cachedDir = dir
		return dir, nil
	}

	if err := extract(dir, want); err != nil {
		return "", fmt.Errorf("extract helpers bundle to %s: %w", dir, err)
	}
	cachedDir = dir
	return dir, nil
}

// Command builds an *exec.Cmd that invokes one of the embedded helper
// tools by basename (e.g. helpers.IdeviceID, helpers.IdeviceBackup).
// The bundle is extracted on demand if needed.
//
// The returned command has its Env preset so that PATH is prepended
// with the bundle directory — useful in case the tool internally
// shells out to another helper that lives alongside it.
func Command(ctx context.Context, tool string, args ...string) (*exec.Cmd, error) {
	dir, err := Path()
	if err != nil {
		return nil, err
	}
	bin := filepath.Join(dir, tool)
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("helper %q not in bundle: %w", tool, err)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = prependPath(os.Environ(), dir)
	return cmd, nil
}

// prependPath returns a copy of env with PATH prefixed by dir.
func prependPath(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	prefixed := false
	for _, kv := range env {
		if len(kv) > 5 && kv[:5] == "PATH=" {
			out = append(out, "PATH="+dir+":"+kv[5:])
			prefixed = true
			continue
		}
		out = append(out, kv)
	}
	if !prefixed {
		out = append(out, "PATH="+dir)
	}
	return out
}

// cacheDir returns the absolute path to ~/Library/Caches/whatskept/bin.
func cacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	return filepath.Join(home, cacheSubpath), nil
}

// bundleHash returns a deterministic SHA-256 of the embedded bundle.
// Files are visited in sorted order so the output is stable across
// builds with the same content.
func bundleHash() (string, error) {
	sub, err := fs.Sub(bundleFS, embedRoot)
	if err != nil {
		return "", err
	}

	type entry struct{ name string }
	var files []entry
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, entry{name: p})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })

	h := sha256.New()
	for _, e := range files {
		f, err := sub.Open(e.name)
		if err != nil {
			return "", err
		}
		// Hash the relative path AND its bytes so a rename alone
		// invalidates the cache.
		_, _ = h.Write([]byte(e.name + "\x00"))
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extract wipes the cache dir and writes the embedded bundle into it.
// All files are mode 0o755 so binaries (and dylibs, harmless) are
// executable. After extract, a hash-marker is written so subsequent
// runs short-circuit.
func extract(dir, hash string) error {
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	sub, err := fs.Sub(bundleFS, embedRoot)
	if err != nil {
		return err
	}

	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		src, err := sub.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()

		dst := filepath.Join(dir, p)
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, src)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, hashMarker), []byte(hash), 0o644)
}
