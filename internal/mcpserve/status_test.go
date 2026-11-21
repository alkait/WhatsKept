package mcpserve

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pinNow(t *testing.T, at time.Time) {
	t.Helper()
	prev := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = prev })
}

// writeSyncRow adds live's wa_history_sync row to a fixture database.
func writeSyncRow(t *testing.T, dbPath, last string, progress, done int) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS wa_history_sync (id INTEGER PRIMARY KEY,
		started_at TEXT, last_chunk_at TEXT, progress INTEGER, inserted INTEGER, done INTEGER);
		DELETE FROM wa_history_sync;`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO wa_history_sync VALUES (1, '2026-11-21 10:00:00', ?, ?, 10, ?)`,
		last, progress, done); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryNote(t *testing.T) {
	path := writeFixtureDB(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	pinNow(t, time.Date(2026, 11, 21, 10, 5, 0, 0, time.UTC))

	if n := historyNote(ctx, db); n != "" {
		t.Errorf("no table: note = %q", n)
	}
	for _, c := range []struct {
		last       string
		progress   int
		done       int
		wantPrefix string
	}{
		{"2026-11-21 10:04:30", 42, 0, "history sync in progress (42%, started 2026-11-21 10:00:00 UTC, last activity 30s ago)"},
		// A chunk type can report 100% with more to come: still active.
		{"2026-11-21 10:04:30", 100, 1, "history sync in progress (started 2026-11-21 10:00:00 UTC, last activity 30s ago)"},
		{"2026-11-21 09:00:00", 42, 0, "history sync stopped at 42% (last chunk 2026-11-21 09:00:00 UTC)"},
		{"2026-11-21 09:00:00", 100, 1, ""}, // quiet and complete
		{"2026-11-19 09:00:00", 42, 0, ""},  // long stale: no longer mentioned
	} {
		writeSyncRow(t, path, c.last, c.progress, c.done)
		n := historyNote(ctx, db)
		if c.wantPrefix == "" && n != "" || !strings.HasPrefix(n, c.wantPrefix) {
			t.Errorf("last=%s progress=%d done=%d: note = %q, want prefix %q", c.last, c.progress, c.done, n, c.wantPrefix)
		}
	}
}

func TestEnrichNote(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	pinNow(t, at)

	if n := enrichNote(dir); n != "" {
		t.Errorf("empty queue: note = %q", n)
	}
	for i, name := range []string{"media/1.jpg", "media/2.jpg", "voice/3.opus", "media/.4.jpg.part"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte{byte(i)}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if n := enrichNote(dir); n != "3 media files not yet searchable (their descriptions/transcripts are pending) — enrichment is not running" {
		t.Errorf("not running: note = %q", n)
	}

	// One file enriched every 10s, the last one a minute ago: 3 files
	// left at that pace is 30s.
	var log strings.Builder
	for i := 0; i < 5; i++ {
		ts := at.Add(-time.Minute - time.Duration(4-i)*10*time.Second)
		fmt.Fprintf(&log, "%s images %d enriched (live) cost_usd=0.0010\n", ts.Format(time.RFC3339), 100+i)
		fmt.Fprintf(&log, "%s images %d enriched (already in DB; leftover file removed)\n", ts.Format(time.RFC3339), 200+i)
	}
	if err := os.WriteFile(filepath.Join(dir, "enrich.log"), []byte(log.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := enrichNote(dir); !strings.HasSuffix(n, "— enrichment is running, about 30s left") {
		t.Errorf("running: note = %q", n)
	}
}

// The note reaches the client on both read tools, and is absent when
// there is nothing to say.
func TestNoteOnResults(t *testing.T) {
	path := writeFixtureDB(t)
	pinNow(t, time.Date(2026, 11, 21, 10, 5, 0, 0, time.UTC))
	sess := connect(t, path)

	var q queryOut
	call(t, sess, "query", map[string]any{"sql": "SELECT 1"}, &q)
	if q.Note != "" {
		t.Errorf("idle: query note = %q", q.Note)
	}

	writeSyncRow(t, path, "2026-11-21 10:04:00", 40, 0)
	q = queryOut{}
	call(t, sess, "query", map[string]any{"sql": "SELECT 1"}, &q)
	if !strings.HasPrefix(q.Note, "history sync in progress (40%") {
		t.Errorf("query note = %q", q.Note)
	}
	var s searchOut
	call(t, sess, "search", map[string]any{"query": "lunch"}, &s)
	if len(s.Hits) != 1 || !strings.HasPrefix(s.Note, "history sync in progress (40%") {
		t.Errorf("search: hits=%d note=%q", len(s.Hits), s.Note)
	}
}
