package helpers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// This file owns the on-disk lifecycle of large ML model files that
// are too big to embed into the Go binary. The Whisper speech model
// is 574 MB; embedding it would inflate every dev build, every CI
// artifact, and every release ZIP by that much, while also forcing
// users to re-download it on every app update. Instead the model
// lives in ~/Library/Application Support/whatskept/models/ and is
// fetched from HuggingFace on first use, persisting across upgrades.
//
// The download itself is performed by internal/postprocess/voice.go;
// this file only owns specs (URL, sha256, expected size) and path
// resolution.

// ModelSpec describes one downloadable ML model.
//
// Source is fixed to HuggingFace right now; if we ever add a mirror
// we'll plumb a list of URLs here. SHA256 is the authoritative
// integrity check — we deliberately do NOT trust the server's
// Content-Length alone, since CDNs occasionally serve truncated
// files when their backend storage is in trouble.
type ModelSpec struct {
	Name    string // basename used on disk and in UI ("ggml-large-v3-turbo-q5_0.bin")
	Display string // human-readable name shown to the user
	URL     string // HTTPS source
	SHA256  string // lower-hex; used to verify post-download
	Bytes   int64  // expected file size in bytes
}

// WhisperModel is the speech model used by `whatskept voice-index`.
//
// We default to the "large-v3-turbo" weights at q5_0 quantization,
// which match the quality we measured in the Phase 0b POC at 13.9×
// realtime on M-series Macs. Switching to a different size (medium,
// large) is a one-constant change here plus a new `make fetch-model`
// invocation; the caller code is model-agnostic.
var WhisperModel = ModelSpec{
	Name:    "ggml-large-v3-turbo-q5_0.bin",
	Display: "Whisper Large-v3-Turbo (q5_0, multilingual)",
	URL:     "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-large-v3-turbo-q5_0.bin",
	SHA256:  "394221709cd5ad1f40c46e6031ca61bce88931e6e088c188294c6d5a55ffa7e2",
	Bytes:   574041195,
}

// AppSupportDir returns ~/Library/Application Support/whatskept,
// creating it if it doesn't exist. This is where persistent app
// data (recent_workspaces.json, models, etc) lives — distinct from
// the binary-extract cache under ~/Library/Caches/whatskept which
// macOS can purge.
func AppSupportDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	dir := filepath.Join(home, "Library", "Application Support", "whatskept")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// ModelDir returns ~/Library/Application Support/whatskept/models,
// creating it if missing. Caller-agnostic to the specific model.
func ModelDir() (string, error) {
	root, err := AppSupportDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "models")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// ModelPath returns the absolute path the named model would live at.
// The file may or may not exist; check with ModelStatus.
func ModelPath(spec ModelSpec) (string, error) {
	dir, err := ModelDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, spec.Name), nil
}

// ModelStatus describes the state of a model on disk.
type ModelStatus int

const (
	// ModelMissing — file does not exist (clean state, never downloaded
	// or user deleted it).
	ModelMissing ModelStatus = iota

	// ModelPartial — file exists but its size is less than spec.Bytes.
	// Indicates an interrupted download. Range-resume is safe.
	ModelPartial

	// ModelOversized — file exists but is larger than spec.Bytes.
	// Probably a different model with the same name; refuse to use.
	// Caller should delete + re-fetch.
	ModelOversized

	// ModelPresent — file exists and matches expected size. SHA-verify
	// is a separate explicit step (slow on first call, ~0.5s for 574MB).
	ModelPresent

	// ModelVerified — file exists, size matches AND sha256 matches.
	// Strongest possible status without rehashing.
	ModelVerified
)

func (s ModelStatus) String() string {
	switch s {
	case ModelMissing:
		return "missing"
	case ModelPartial:
		return "partial"
	case ModelOversized:
		return "oversized"
	case ModelPresent:
		return "present"
	case ModelVerified:
		return "verified"
	default:
		return "unknown"
	}
}

// CheckModel returns the on-disk status of a model spec. The
// `verify` flag controls whether to recompute sha256 (slow but
// authoritative); without it the function only checks size.
//
// We don't keep a sidecar .sha256 file as a "verified once, trust
// forever" marker — disk corruption is rare enough that an explicit
// per-call decision is cleaner than a stale-marker hazard.
func CheckModel(spec ModelSpec, verify bool) (ModelStatus, int64, error) {
	path, err := ModelPath(spec)
	if err != nil {
		return ModelMissing, 0, err
	}
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ModelMissing, 0, nil
	}
	if err != nil {
		return ModelMissing, 0, fmt.Errorf("stat %s: %w", path, err)
	}
	size := st.Size()
	switch {
	case size < spec.Bytes:
		return ModelPartial, size, nil
	case size > spec.Bytes:
		return ModelOversized, size, nil
	}

	if !verify {
		return ModelPresent, size, nil
	}
	got, err := fileSHA256(path)
	if err != nil {
		return ModelPresent, size, fmt.Errorf("sha256 %s: %w", path, err)
	}
	if got != spec.SHA256 {
		// Same size, different content — corrupted or maliciously
		// substituted. Caller should delete + re-fetch.
		return ModelOversized, size, fmt.Errorf("sha256 mismatch: got %s, want %s", got, spec.SHA256)
	}
	return ModelVerified, size, nil
}

// fileSHA256 returns the lower-hex sha256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
