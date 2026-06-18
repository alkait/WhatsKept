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
