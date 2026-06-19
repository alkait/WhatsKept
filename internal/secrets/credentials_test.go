package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRouterKeyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(configDirEnv, dir)

	// Absent on a fresh dir.
	if _, ok := LoadOpenRouterKey(); ok {
		t.Fatal("expected no key on a fresh config dir")
	}

	// Save → load.
	if err := SaveOpenRouterKey("sk-or-abc"); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok := LoadOpenRouterKey()
	if !ok || got != "sk-or-abc" {
		t.Fatalf("load = %q,%v; want sk-or-abc,true", got, ok)
	}

	// File is 0600.
	p := filepath.Join(dir, credentialsFileName)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}

	// Overwrite.
	if err := SaveOpenRouterKey("sk-or-xyz"); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	if got, _ := LoadOpenRouterKey(); got != "sk-or-xyz" {
		t.Errorf("after overwrite = %q, want sk-or-xyz", got)
	}

	// Delete → absent, and the file is gone (nothing else in it).
	if err := DeleteOpenRouterKey(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := LoadOpenRouterKey(); ok {
		t.Error("expected no key after delete")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("expected credentials file removed, stat err = %v", err)
	}

	// Delete on a missing file is a no-op.
	if err := DeleteOpenRouterKey(); err != nil {
		t.Errorf("delete on missing file: %v", err)
	}
}

func TestBackupPasswordPerDevice(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(configDirEnv, dir)
	p := filepath.Join(dir, credentialsFileName)

	// Absent on a fresh dir; empty udid is always absent.
	if _, ok := LoadBackupPassword("UDID-A"); ok {
		t.Fatal("expected no password on a fresh config dir")
	}
	if _, ok := LoadBackupPassword(""); ok {
		t.Fatal("empty udid must never report a password")
	}

	// Two devices keep independent passwords.
	if err := SaveBackupPassword("UDID-A", "pw-a"); err != nil {
		t.Fatalf("save A: %v", err)
	}
	if err := SaveBackupPassword("UDID-B", "pw-b"); err != nil {
		t.Fatalf("save B: %v", err)
	}
	if got, ok := LoadBackupPassword("UDID-A"); !ok || got != "pw-a" {
		t.Errorf("load A = %q,%v; want pw-a,true", got, ok)
	}
	if got, ok := LoadBackupPassword("UDID-B"); !ok || got != "pw-b" {
		t.Errorf("load B = %q,%v; want pw-b,true", got, ok)
	}

	// Coexists with the OpenRouter key in the same file.
	if err := SaveOpenRouterKey("sk-or-key"); err != nil {
		t.Fatalf("save key: %v", err)
	}

	// Deleting one device leaves the other and the key intact.
	if err := DeleteBackupPassword("UDID-A"); err != nil {
		t.Fatalf("delete A: %v", err)
	}
	if _, ok := LoadBackupPassword("UDID-A"); ok {
		t.Error("expected A gone after delete")
	}
	if got, ok := LoadBackupPassword("UDID-B"); !ok || got != "pw-b" {
		t.Errorf("B clobbered by A delete: %q,%v", got, ok)
	}
	if got, ok := LoadOpenRouterKey(); !ok || got != "sk-or-key" {
		t.Errorf("key clobbered by A delete: %q,%v", got, ok)
	}

	// Removing the last password but leaving the key keeps the file.
	if err := DeleteBackupPassword("UDID-B"); err != nil {
		t.Fatalf("delete B: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("file should remain (key still present): %v", err)
	}

	// Dropping the key too removes the now-empty file.
	if err := DeleteOpenRouterKey(); err != nil {
		t.Fatalf("delete key: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("expected empty credentials file removed, stat err = %v", err)
	}

	// Delete on absent entry / missing file is a no-op.
	if err := DeleteBackupPassword("UDID-A"); err != nil {
		t.Errorf("delete absent: %v", err)
	}
}
