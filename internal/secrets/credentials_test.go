package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRouterKeyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(configDirEnv, dir)

	const ws = "/work/space-a"

	// Absent on a fresh dir; an empty workspace is always absent.
	if _, ok := LoadOpenRouterKey(ws); ok {
		t.Fatal("expected no key on a fresh config dir")
	}
	if _, ok := LoadOpenRouterKey(""); ok {
		t.Fatal("empty workspace must never report a key")
	}

	// Save → load.
	if err := SaveOpenRouterKey(ws, "sk-or-abc"); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok := LoadOpenRouterKey(ws)
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
	if err := SaveOpenRouterKey(ws, "sk-or-xyz"); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	if got, _ := LoadOpenRouterKey(ws); got != "sk-or-xyz" {
		t.Errorf("after overwrite = %q, want sk-or-xyz", got)
	}

	// Empty workspace is rejected on save.
	if err := SaveOpenRouterKey("", "sk-or-nope"); err == nil {
		t.Error("expected an error saving with an empty workspace")
	}

	// Delete → absent, and the file is gone (nothing else in it).
	if err := DeleteOpenRouterKey(ws); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := LoadOpenRouterKey(ws); ok {
		t.Error("expected no key after delete")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("expected credentials file removed, stat err = %v", err)
	}

	// Delete on a missing file is a no-op.
	if err := DeleteOpenRouterKey(ws); err != nil {
		t.Errorf("delete on missing file: %v", err)
	}
}

func TestOpenRouterKeyPerWorkspace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(configDirEnv, dir)

	const wsA, wsB = "/work/a", "/work/b"

	if err := SaveOpenRouterKey(wsA, "sk-a"); err != nil {
		t.Fatalf("save A: %v", err)
	}
	if err := SaveOpenRouterKey(wsB, "sk-b"); err != nil {
		t.Fatalf("save B: %v", err)
	}
	if got, ok := LoadOpenRouterKey(wsA); !ok || got != "sk-a" {
		t.Errorf("load A = %q,%v; want sk-a,true", got, ok)
	}
	if got, ok := LoadOpenRouterKey(wsB); !ok || got != "sk-b" {
		t.Errorf("load B = %q,%v; want sk-b,true", got, ok)
	}

	// Deleting one workspace's key leaves the other intact.
	if err := DeleteOpenRouterKey(wsA); err != nil {
		t.Fatalf("delete A: %v", err)
	}
	if _, ok := LoadOpenRouterKey(wsA); ok {
		t.Error("expected A gone after delete")
	}
	if got, ok := LoadOpenRouterKey(wsB); !ok || got != "sk-b" {
		t.Errorf("B clobbered by A delete: %q,%v", got, ok)
	}
}

func TestMigrateLegacyOpenRouterKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(configDirEnv, dir)

	// No legacy key → nothing to migrate.
	if _, ok := MigrateLegacyOpenRouterKey("/work/a"); ok {
		t.Fatal("expected no migration on a fresh dir")
	}

	// Seed a legacy global key the way an older version wrote it.
	if err := saveCredentials(credentials{OpenRouterAPIKey: "sk-legacy"}); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	// The first workspace opened adopts it...
	got, ok := MigrateLegacyOpenRouterKey("/work/a")
	if !ok || got != "sk-legacy" {
		t.Fatalf("migrate = %q,%v; want sk-legacy,true", got, ok)
	}
	if got, ok := LoadOpenRouterKey("/work/a"); !ok || got != "sk-legacy" {
		t.Errorf("workspace A should now hold the key: %q,%v", got, ok)
	}

	// ...and the global copy is consumed, so a second workspace doesn't inherit it.
	if _, ok := MigrateLegacyOpenRouterKey("/work/b"); ok {
		t.Error("legacy key should be consumed after the first migration")
	}
	if _, ok := LoadOpenRouterKey("/work/b"); ok {
		t.Error("workspace B must not inherit the migrated key")
	}
}

func TestBackupPasswordPerDevice(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(configDirEnv, dir)
	p := filepath.Join(dir, credentialsFileName)

	const ws = "/work/space"

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

	// Coexists with an OpenRouter key in the same file.
	if err := SaveOpenRouterKey(ws, "sk-or-key"); err != nil {
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
	if got, ok := LoadOpenRouterKey(ws); !ok || got != "sk-or-key" {
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
	if err := DeleteOpenRouterKey(ws); err != nil {
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
