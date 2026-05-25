package postprocess

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPruneOrphanFiles covers the four shapes pruneOrphanFiles must
// handle correctly. The disk-walk is the only piece of pruneOrphans
// that's testable without spinning up a full sqlite + ZWAMESSAGE
// fixture, but it's also the piece most likely to drift (suffix
// matching, rowid parsing, foreign-suffix leave-alone behaviour).
func TestPruneOrphanFiles(t *testing.T) {
	dir := t.TempDir()

	// "alive" rowids = {1, 2}. Files for any other rowid should be
	// deleted; files whose names aren't <int><suffix> shape should be
	// left strictly alone.
	files := map[string]string{
		"1.opus":         "keep — alive",
		"2.opus":         "keep — alive",
		"3.opus":         "delete — orphan",
		"99.opus":        "delete — orphan",
		"hello.opus":     "keep — not <int>.opus",
		"1.jpg":          "keep — wrong suffix",
		"3.opus.tmp":     "keep — wrong suffix (trailing)",
		"README.md":      "keep — wrong suffix",
		"4_partial.opus": "keep — has extra suffix chars",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	alive := map[int64]struct{}{1: {}, 2: {}}
	var deleted, failed int
	if err := pruneOrphanFiles(dir, ".opus", alive, &deleted, &failed); err != nil {
		t.Fatalf("pruneOrphanFiles: %v", err)
	}

	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}

	// Verify the survivors are exactly the keep-list above.
	wantSurvivors := map[string]bool{
		"1.opus":         true,
		"2.opus":         true,
		"hello.opus":     true,
		"1.jpg":          true,
		"3.opus.tmp":     true,
		"README.md":      true,
		"4_partial.opus": true,
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for name := range wantSurvivors {
		if !got[name] {
			t.Errorf("missing survivor %q", name)
		}
	}
	for name := range got {
		if !wantSurvivors[name] {
			t.Errorf("unexpected file %q survived", name)
		}
	}
}

// TestPruneOrphanFilesEmptyDir verifies the two quiet-no-op fast
// paths: empty string dir and nonexistent dir. Both must succeed
// without touching counters.
func TestPruneOrphanFilesEmptyDir(t *testing.T) {
	alive := map[int64]struct{}{1: {}}
	var deleted, failed int

	if err := pruneOrphanFiles("", ".opus", alive, &deleted, &failed); err != nil {
		t.Fatalf("empty string: %v", err)
	}
	if err := pruneOrphanFiles("/does/not/exist/whatskept-prune-test", ".opus", alive, &deleted, &failed); err != nil {
		t.Fatalf("nonexistent: %v", err)
	}
	if deleted != 0 || failed != 0 {
		t.Errorf("counters bumped: deleted=%d failed=%d", deleted, failed)
	}
}
