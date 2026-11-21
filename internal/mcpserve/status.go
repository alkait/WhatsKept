package mcpserve

// The status note: one line attached to search and query results while
// the history is known to be incomplete, so the client can say why an
// answer may be missing something rather than presenting it as whole.
// Two sources, both beside the database:
//
//   - wa_history_sync, the row live keeps current while a pairing's
//     history sync arrives;
//   - the .unenriched/ queue: media whose description, transcript or
//     text isn't searchable yet, with an ETA from enrich.log's recent
//     pace when enrichment is running.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"whatskept/internal/backup"
	"whatskept/internal/enrich"
)

// now is the clock the note reads; a var so tests can pin it.
var now = time.Now

const (
	// A sync with no chunk for this long is no longer arriving.
	syncQuiet = 10 * time.Minute
	// An unfinished sync is mentioned for this long after its last chunk.
	syncStaleFor = 24 * time.Hour
	// Enrichment counts as running when a file was enriched this recently.
	enrichActive = 10 * time.Minute
	// The ETA is paced over this many most-recent enrichments.
	enrichPaceOver = 50
)

const tsLayout = "2006-01-02 15:04:05"

// statusNote returns the note for the current state, or "" when there
// is nothing to report. Best effort: a status that can't be read is no
// note, never an error on the caller's query.
func (s *server) statusNote(ctx context.Context, db *sql.DB) string {
	var parts []string
	if n := historyNote(ctx, db); n != "" {
		parts = append(parts, n)
	}
	if n := enrichNote(filepath.Join(filepath.Dir(s.dbPath), backup.UnenrichedDir)); n != "" {
		parts = append(parts, n)
	}
	return strings.Join(parts, "; ")
}

func historyNote(ctx context.Context, db *sql.DB) string {
	has, err := existingTables(ctx, db, []string{"wa_history_sync"})
	if err != nil || !has["wa_history_sync"] {
		return ""
	}
	var started, last string
	var progress, done int
	if err := db.QueryRowContext(ctx,
		`SELECT started_at, last_chunk_at, progress, done FROM wa_history_sync WHERE id = 1`).
		Scan(&started, &last, &progress, &done); err != nil || done != 0 {
		return ""
	}
	lastAt, err := time.Parse(tsLayout, last)
	if err != nil {
		return ""
	}
	since := now().Sub(lastAt)
	switch {
	case since < syncQuiet:
		return fmt.Sprintf("history sync in progress (%d%%, started %s UTC, last chunk %s ago) — "+
			"older messages are still arriving; results may be incomplete", progress, started, roundAgo(since))
	case since < syncStaleFor:
		return fmt.Sprintf("history sync stopped at %d%% (last chunk %s UTC) — "+
			"messages from before linking may be partial; re-pairing live completes it", progress, last)
	}
	return ""
}

// enrichNote counts the queued files and, when enrichment is running,
// estimates the time left from its recent pace.
func enrichNote(unenriched string) string {
	pending := 0
	for _, kind := range []string{"media", "voice", "documents"} {
		entries, err := os.ReadDir(filepath.Join(unenriched, kind))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				pending++
			}
		}
	}
	if pending == 0 {
		return ""
	}
	note := fmt.Sprintf("%d media files not yet searchable (their descriptions/transcripts are pending)", pending)
	times := recentEnrichments(filepath.Join(unenriched, enrich.LogName))
	if len(times) == 0 || now().Sub(times[len(times)-1]) > enrichActive {
		return note + " — enrichment is not running"
	}
	if len(times) < 2 {
		return note + " — enrichment is running"
	}
	span := times[len(times)-1].Sub(times[0])
	if span <= 0 {
		return note + " — enrichment is running"
	}
	perFile := span / time.Duration(len(times)-1)
	return note + fmt.Sprintf(" — enrichment is running, about %s left", roundAgo(perFile*time.Duration(pending)))
}

// recentEnrichments returns the timestamps of the last enrichPaceOver
// "enriched" lines in enrich.log, oldest first. Only the file's tail is
// read: the log grows for the life of the workspace.
func recentEnrichments(path string) []time.Time {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const tail = 64 << 10
	if info, err := f.Stat(); err == nil && info.Size() > tail {
		if _, err := f.Seek(-tail, io.SeekEnd); err != nil {
			return nil
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	var out []time.Time
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		// "<RFC3339> <kind> <rowid> enriched[ (live)] …"; the
		// "already in DB" leftovers cost no time and don't count.
		f := strings.Fields(sc.Text())
		if len(f) < 4 || f[3] != "enriched" || strings.Contains(sc.Text(), "already in DB") {
			continue
		}
		if t, err := time.Parse(time.RFC3339, f[0]); err == nil {
			out = append(out, t)
		}
	}
	if len(out) > enrichPaceOver {
		out = out[len(out)-enrichPaceOver:]
	}
	return out
}

// roundAgo renders a duration coarsely: seconds under a minute, minutes
// under an hour, then hours.
func roundAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	default:
		return d.Round(time.Hour).String()
	}
}
