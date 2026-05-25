package postprocess

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// This file owns three concerns that all rotate around the same axis:
// the "sidecar" tables (wa_image_text, media_index, wa_voice_text,
// voice_index) that hold derived data WhatsKept generates and that
// must survive across re-extracts of ChatStorage.sqlite from a fresh
// iOS backup.
//
//   1. mergeSidecarsForward — copy rows from a previous live DB into a
//      freshly-decrypted staging DB before we promote staging → live,
//      so OCR / transcript work from earlier syncs isn't lost.
//   2. pruneOrphans — after promotion, delete sidecar rows whose
//      `rowid` (= ZWAMESSAGE.Z_PK) is no longer in the new DB, and
//      remove the matching ./media/<rowid>.jpg files from disk. The
//      workspace's ./media folder must mirror device state.
//   3. rebuildFTS — repopulate messages_fts dynamically, JOINing in
//      wa_image_text / wa_voice_text / wa_document whenever those
//      tables exist, so the FTS surface grows automatically as
//      sidecar indexers run.
//
// All three are idempotent and safe to call on a workspace that has
// never seen any of these tables; missing sidecars are silently
// skipped (cheap COUNT-of-tables probe per call).

// SidecarMergeStats summarises mergeSidecarsForward's work. Surfaced
// in SyncResult so the GUI/CLI can show "preserved 4,210 OCR rows".
type SidecarMergeStats struct {
	MessagesBefore    int `json:"messages_before"`
	MessagesAfter     int `json:"messages_after"`
	MessagesNew       int `json:"messages_new"`
	MessagesDropped   int `json:"messages_dropped"`
	OCRPreserved      int `json:"ocr_preserved"`
	OCRDropped        int `json:"ocr_dropped"`
	MediaIndexCarried int `json:"media_index_carried"`
	VoicePreserved    int `json:"voice_preserved"`
	VoiceDropped      int `json:"voice_dropped"`
	VoiceIndexCarried int `json:"voice_index_carried"`
}

// OrphanPruneStats summarises pruneOrphans's work.
type OrphanPruneStats struct {
	OCRRowsDeleted        int `json:"ocr_rows_deleted"`
	MediaIndexRowsDeleted int `json:"media_index_rows_deleted"`
	VoiceRowsDeleted      int `json:"voice_rows_deleted"`
	VoiceIndexRowsDeleted int `json:"voice_index_rows_deleted"`
	MediaFilesDeleted     int `json:"media_files_deleted"`
	MediaFilesFailed      int `json:"media_files_failed"`
	VoiceFilesDeleted     int `json:"voice_files_deleted"`
	VoiceFilesFailed      int `json:"voice_files_failed"`
}

// tableExists returns true if a regular table or virtual table with
// the given name is present in the connected database under the
// `main` schema. Used to fail-soft on a fresh DB that has never seen
// any sidecar tables.
func tableExists(db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master
		 WHERE type IN ('table','virtual') AND name = ?`,
		name,
	).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// attachedTableExists is the ATTACH-aware variant used during
// mergeSidecarsForward — we need to probe `old.sqlite_master` to know
// whether the *previous* DB had a sidecar table, not `main`.
func attachedTableExists(db *sql.DB, schema, name string) (bool, error) {
	q := fmt.Sprintf(
		`SELECT COUNT(*) FROM %s.sqlite_master
		 WHERE type IN ('table','virtual') AND name = ?`,
		schema,
	)
	var n int
	if err := db.QueryRow(q, name).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// mergeSidecarsForward copies sidecar rows from oldDB into newDB, but
// only for messages that still exist in newDB. Called from
// SyncMessages between "decrypt to staging file" and "rename staging
// over live", so that re-extracting from a newer backup doesn't
// destroy OCR / transcript work from earlier syncs.
//
// oldDB and newDB must be DIFFERENT files on disk; passing the same
// path will produce a self-ATTACH error from SQLite. The newDB is
// opened R/W; oldDB is opened read-only via ATTACH. No transactions
// are opened explicitly — the single INSERT OR REPLACE statements
// per table run as implicit transactions, which is exactly what we
// want (each carry-forward is atomic).
//
// Returns the per-stage counts. Errors abort the merge but leave
// newDB in a consistent state (whatever managed to commit, stays).
func mergeSidecarsForward(oldDB, newDB string) (*SidecarMergeStats, error) {
	if oldDB == newDB {
		return nil, fmt.Errorf("mergeSidecarsForward: oldDB and newDB must differ (%s)", newDB)
	}
	db, err := sql.Open("sqlite3", newDB)
	if err != nil {
		return nil, fmt.Errorf("open new db: %w", err)
	}
	defer db.Close()

	// SQLite literal needs escaped single quotes. oldDB is a local
	// filesystem path under our control (never user-shellable), but
	// belt-and-suspenders.
	attachLit := strings.ReplaceAll(oldDB, "'", "''")
	if _, err := db.Exec("ATTACH '" + attachLit + "' AS old"); err != nil {
		return nil, fmt.Errorf("attach old db: %w", err)
	}
	defer db.Exec("DETACH old") //nolint:errcheck // best-effort cleanup

	stats := &SidecarMergeStats{}

	// Delta counts: how many messages came and went between the two
	// extracts. Useful both for the user-visible "+1,234 new / -56
	// deleted" log line and for the pruneOrphans loop to know up
	// front if it needs to scan ./media at all.
	if err := db.QueryRow(`SELECT COUNT(*) FROM main.ZWAMESSAGE`).Scan(&stats.MessagesAfter); err != nil {
		return nil, fmt.Errorf("count main messages: %w", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM old.ZWAMESSAGE`).Scan(&stats.MessagesBefore); err != nil {
		return nil, fmt.Errorf("count old messages: %w", err)
	}
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM main.ZWAMESSAGE
		 WHERE Z_PK NOT IN (SELECT Z_PK FROM old.ZWAMESSAGE)`,
	).Scan(&stats.MessagesNew); err != nil {
		return nil, fmt.Errorf("count new messages: %w", err)
	}
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM old.ZWAMESSAGE
		 WHERE Z_PK NOT IN (SELECT Z_PK FROM main.ZWAMESSAGE)`,
	).Scan(&stats.MessagesDropped); err != nil {
		return nil, fmt.Errorf("count dropped messages: %w", err)
	}

	// wa_image_text + media_index. If the old DB had these (because
	// the user previously ran media-index) the new staging DB
	// definitely doesn't — staging is freshly extracted, devoid of
	// any sidecar tables. Re-create the tables here, then copy rows.
	hadOCR, err := attachedTableExists(db, "old", "wa_image_text")
	if err != nil {
		return nil, fmt.Errorf("probe old.wa_image_text: %w", err)
	}
	if hadOCR {
		if _, err := db.Exec(createImageSidecarsSQL); err != nil {
			return nil, fmt.Errorf("create image sidecar tables: %w", err)
		}
		res, err := db.Exec(
			`INSERT OR REPLACE INTO main.wa_image_text
			 SELECT * FROM old.wa_image_text
			 WHERE rowid IN (SELECT Z_PK FROM main.ZWAMESSAGE)`,
		)
		if err != nil {
			return nil, fmt.Errorf("preserve wa_image_text: %w", err)
		}
		if n, e := res.RowsAffected(); e == nil {
			stats.OCRPreserved = int(n)
		}
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM old.wa_image_text
			 WHERE rowid NOT IN (SELECT Z_PK FROM main.ZWAMESSAGE)`,
		).Scan(&stats.OCRDropped); err != nil {
			return nil, fmt.Errorf("count dropped ocr rows: %w", err)
		}
		// media_index is co-created with wa_image_text above.
		if had, _ := attachedTableExists(db, "old", "media_index"); had {
			res, err := db.Exec(
				`INSERT OR REPLACE INTO main.media_index
				 SELECT * FROM old.media_index
				 WHERE rowid IN (SELECT Z_PK FROM main.ZWAMESSAGE)`,
			)
			if err != nil {
				return nil, fmt.Errorf("preserve media_index: %w", err)
			}
			if n, e := res.RowsAffected(); e == nil {
				stats.MediaIndexCarried = int(n)
			}
		}
	}

	// wa_voice_text + voice_index — same dance, future-proof for the
	// voice-index port. Idempotent if old DB has neither.
	hadVoice, err := attachedTableExists(db, "old", "wa_voice_text")
	if err != nil {
		return nil, fmt.Errorf("probe old.wa_voice_text: %w", err)
	}
	if hadVoice {
		if _, err := db.Exec(createVoiceSidecarsSQL); err != nil {
			return nil, fmt.Errorf("create voice sidecar tables: %w", err)
		}
		res, err := db.Exec(
			`INSERT OR REPLACE INTO main.wa_voice_text
			 SELECT * FROM old.wa_voice_text
			 WHERE rowid IN (SELECT Z_PK FROM main.ZWAMESSAGE)`,
		)
		if err != nil {
			return nil, fmt.Errorf("preserve wa_voice_text: %w", err)
		}
		if n, e := res.RowsAffected(); e == nil {
			stats.VoicePreserved = int(n)
		}
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM old.wa_voice_text
			 WHERE rowid NOT IN (SELECT Z_PK FROM main.ZWAMESSAGE)`,
		).Scan(&stats.VoiceDropped); err != nil {
			return nil, fmt.Errorf("count dropped voice rows: %w", err)
		}
		if had, _ := attachedTableExists(db, "old", "voice_index"); had {
			res, err := db.Exec(
				`INSERT OR REPLACE INTO main.voice_index
				 SELECT * FROM old.voice_index
				 WHERE rowid IN (SELECT Z_PK FROM main.ZWAMESSAGE)`,
			)
			if err != nil {
				return nil, fmt.Errorf("preserve voice_index: %w", err)
			}
			if n, e := res.RowsAffected(); e == nil {
				stats.VoiceIndexCarried = int(n)
			}
		}
	}

	return stats, nil
}

// createImageSidecarsSQL is the canonical schema for the
// image-OCR sidecar tables. Embedded as a Go string (rather than a
// .sql file) because it's split between two callers — the
// merge-forward step here, and media-index's setup. Drift between
// the two would be a silent corruption hazard.
//
// Schema notes (departures from the Python original, see DESIGN.md):
//   - `engine` column dropped (we only have one engine; YAGNI).
//   - `language` column added (Vision returns recognizedLanguages
//     per request; useful for "find Arabic-text receipts" queries).
//   - `labels` kept as CSV alongside `label_scores` JSON, so FTS
//     rebuild can splice in label words with a single REPLACE.
const createImageSidecarsSQL = `
CREATE TABLE IF NOT EXISTS wa_image_text (
    rowid         INTEGER PRIMARY KEY,
    ocr_text      TEXT NOT NULL DEFAULT '',
    language      TEXT NOT NULL DEFAULT '',
    labels        TEXT NOT NULL DEFAULT '',
    label_scores  TEXT,
    generated_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS media_index (
    rowid         INTEGER PRIMARY KEY,
    manifest_path TEXT    NOT NULL,
    msg_type      INTEGER NOT NULL,
    status        TEXT    NOT NULL,
    bytes         INTEGER,
    error         TEXT,
    attempted_at  TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS media_index_status_idx ON media_index(status);
`

// createVoiceSidecarsSQL — placeholder for the voice-index port.
// Mirrors the Python schema so a future voice-index command can
// re-use it. Kept here so merge-forward can recreate the tables in
// staging without depending on a not-yet-written voice.go.
const createVoiceSidecarsSQL = `
CREATE TABLE IF NOT EXISTS wa_voice_text (
    rowid         INTEGER PRIMARY KEY,
    transcript    TEXT NOT NULL DEFAULT '',
    language      TEXT NOT NULL DEFAULT '',
    duration_sec  REAL,
    segments_json TEXT,
    generated_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS voice_index (
    rowid         INTEGER PRIMARY KEY,
    manifest_path TEXT    NOT NULL,
    status        TEXT    NOT NULL,
    bytes         INTEGER,
    duration_sec  REAL,
    error         TEXT,
    attempted_at  TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS voice_index_status_idx ON voice_index(status);
`

// pruneOrphans enforces the "./media folder mirrors device state"
// invariant after a SyncMessages run. Two passes:
//
//  1. DB-side defense in depth. Delete any sidecar rows whose
//     rowid is not in main.ZWAMESSAGE. After mergeSidecarsForward
//     this should always be a no-op (the merge filters by rowid
//     existence on the way in), but it's cheap insurance against
//     schema drift or interrupted prior syncs.
//
//  2. Disk-scan. Walk mediaDir, parse the leading integer from
//     each filename, and delete any whose rowid isn't in the
//     "alive" set. This also catches the failure mode where a
//     crashed earlier sync wrote a JPEG to disk before committing
//     the DB row — there's no DB row to drive the deletion, only
//     the orphaned file.
//
// Fail-soft: per-file os.Remove failures bump MediaFilesFailed but
// never abort.
func pruneOrphans(dbPath, mediaDir, voiceDir string) (*OrphanPruneStats, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	stats := &OrphanPruneStats{}

	// Pass 1 — DB rows. Loop the four sidecar tables; skip any that
	// don't exist yet (fresh workspace).
	type tbl struct {
		name string
		out  *int
	}
	tables := []tbl{
		{"wa_image_text", &stats.OCRRowsDeleted},
		{"media_index", &stats.MediaIndexRowsDeleted},
		{"wa_voice_text", &stats.VoiceRowsDeleted},
		{"voice_index", &stats.VoiceIndexRowsDeleted},
	}
	for _, t := range tables {
		has, err := tableExists(db, t.name)
		if err != nil {
			return nil, fmt.Errorf("probe %s: %w", t.name, err)
		}
		if !has {
			continue
		}
		q := fmt.Sprintf(
			`DELETE FROM %s WHERE rowid NOT IN (SELECT Z_PK FROM ZWAMESSAGE)`,
			t.name,
		)
		res, err := db.Exec(q)
		if err != nil {
			return nil, fmt.Errorf("delete orphans from %s: %w", t.name, err)
		}
		if n, e := res.RowsAffected(); e == nil {
			*t.out = int(n)
		}
	}

	// Pass 2 — disk files. Build the "alive" rowid set once
	// (memory cost ~8 bytes/msg → 800 KB for 100K messages; trivial)
	// and reuse it across all per-surface walks below.
	if mediaDir == "" && voiceDir == "" {
		return stats, nil
	}
	rows, err := db.Query(`SELECT Z_PK FROM ZWAMESSAGE`)
	if err != nil {
		return nil, fmt.Errorf("query live message rowids: %w", err)
	}
	alive := make(map[int64]struct{}, 4096)
	for rows.Next() {
		var r int64
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan live rowid: %w", err)
		}
		alive[r] = struct{}{}
	}
	rows.Close()

	// Media folder holds whatever extensions detectImageFormat
	// produces — JPEG / PNG / HEIC / GIF — so we walk each suffix.
	// Any non-matching file (user-dropped, README.txt, etc.) is
	// left alone by pruneOrphanFiles' stem-must-be-pure-int rule.
	for _, ext := range mediaImageExts {
		if err := pruneOrphanFiles(mediaDir, ext, alive,
			&stats.MediaFilesDeleted, &stats.MediaFilesFailed); err != nil {
			return nil, err
		}
	}
	if err := pruneOrphanFiles(voiceDir, ".opus", alive,
		&stats.VoiceFilesDeleted, &stats.VoiceFilesFailed); err != nil {
		return nil, err
	}

	return stats, nil
}

// pruneOrphanFiles walks `dir` and deletes any file whose name is
// "<rowid><suffix>" where rowid is not in `alive`. dir == "" or a
// nonexistent dir is a quiet no-op (first sync, sidecar never run,
// etc). Counts are accumulated into the int pointers.
func pruneOrphanFiles(dir, suffix string, alive map[int64]struct{}, deleted, failed *int) error {
	if dir == "" {
		return nil
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return nil //nolint:nilerr // missing dir is fine, not an error
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		// Filename shape: "<rowid><suffix>" produced by the
		// matching indexer. Anything else (user-dropped files,
		// transient .wav scratch files, etc) is left alone so we
		// never delete data we didn't put there. Use ParseInt
		// rather than Sscanf because the latter is happy to accept
		// "4_partial" → 4, which would mis-classify e.g. half-
		// written outputs from another tool as our orphans.
		stem := strings.TrimSuffix(e.Name(), suffix)
		r, err := strconv.ParseInt(stem, 10, 64)
		if err != nil {
			continue
		}
		if _, ok := alive[r]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			*failed++
		} else {
			*deleted++
		}
	}
	return nil
}

// rebuildFTS drops messages_fts and repopulates it from the current
// DB state. The indexed string per message is the concatenation of:
//
//   - ZWAMESSAGE.ZTEXT                            (always)
//   - wa_image_text.ocr_text + labels             (when wa_image_text exists)
//   - wa_voice_text.transcript                    (when wa_voice_text exists)
//   - wa_document.filename                        (when wa_document exists)
//   - ZWAMEDIAITEM.ZTITLE for link-preview msgs   (always — ZWAMEDIAITEM
//     is part of the iOS
//     schema, so it's
//     always present)
//
// We do this in one SELECT-with-LEFT-JOINs rather than four passes
// because (a) FTS5 inserts get expensive per-row and (b) we want a
// single row per ZWAMESSAGE in the index, not one per source surface.
//
// The CREATE VIRTUAL TABLE itself is owned by views.sql; this
// function only repopulates contents. Safe to call multiple times.
func rebuildFTS(db *sql.DB) (int, error) {
	hasOCR, err := tableExists(db, "wa_image_text")
	if err != nil {
		return 0, fmt.Errorf("probe wa_image_text: %w", err)
	}
	hasVoice, err := tableExists(db, "wa_voice_text")
	if err != nil {
		return 0, fmt.Errorf("probe wa_voice_text: %w", err)
	}
	hasDoc, err := tableExists(db, "wa_document")
	if err != nil {
		return 0, fmt.Errorf("probe wa_document: %w", err)
	}

	// Build the SELECT list incrementally. Every COALESCE returns
	// '' when its source is NULL (LEFT JOIN miss), so the final
	// concatenation is always well-defined even if no sidecar
	// tables exist yet.
	selectParts := []string{"COALESCE(m.ZTEXT, '')"}
	joinParts := []string{}
	whereParts := []string{"(m.ZTEXT IS NOT NULL AND m.ZTEXT <> '')"}

	if hasOCR {
		selectParts = append(selectParts,
			"COALESCE(t.ocr_text, '')",
			// labels is a CSV; replace ',' with ' ' so FTS
			// tokenizes each label as its own term.
			"COALESCE(REPLACE(t.labels, ',', ' '), '')",
		)
		joinParts = append(joinParts, "LEFT JOIN wa_image_text t ON t.rowid = m.Z_PK")
		whereParts = append(whereParts, "t.rowid IS NOT NULL")
	}
	if hasVoice {
		selectParts = append(selectParts, "COALESCE(v.transcript, '')")
		joinParts = append(joinParts, "LEFT JOIN wa_voice_text v ON v.rowid = m.Z_PK")
		whereParts = append(whereParts, "v.rowid IS NOT NULL")
	}
	if hasDoc {
		// Documents have no body text — only the filename is
		// indexable. That's still a huge agent-side win:
		// "passport.pdf", "Estimate_2021.pdf", etc. all become
		// matchable via MATCH.
		selectParts = append(selectParts, "COALESCE(d.filename, '')")
		joinParts = append(joinParts, "LEFT JOIN wa_document d ON d.rowid = m.Z_PK")
		whereParts = append(whereParts, "d.filename IS NOT NULL AND d.filename <> ''")
	}
	// Link preview titles — ZWAMEDIAITEM is always present, so this
	// branch needs no existence probe.
	selectParts = append(selectParts, "COALESCE(mi.ZTITLE, '')")
	joinParts = append(joinParts, "LEFT JOIN ZWAMEDIAITEM mi ON mi.ZMESSAGE = m.Z_PK")
	whereParts = append(whereParts, "mi.ZTITLE IS NOT NULL AND mi.ZTITLE <> ''")

	// WHERE clauses from sidecar tables and link previews are joined
	// with OR — a row qualifies if it has indexable text in ANY of
	// the sources. The first ZTEXT predicate is the dominant one;
	// the OR'd alternatives only matter when ZTEXT is empty (image
	// with no caption, voice note, document, etc).
	primary := whereParts[0]
	alts := whereParts[1:]
	whereSQL := primary
	if len(alts) > 0 {
		whereSQL = "(" + primary + ") OR (" + strings.Join(alts, ") OR (") + ")"
	}

	// Wipe and recreate, atomic within the implicit transaction
	// around DROP+CREATE+INSERT. The CREATE mirrors the schema in
	// views.sql exactly — keep the two in sync if the tokenizer
	// changes.
	if _, err := db.Exec(`DROP TABLE IF EXISTS messages_fts`); err != nil {
		return 0, fmt.Errorf("drop messages_fts: %w", err)
	}
	if _, err := db.Exec(`CREATE VIRTUAL TABLE messages_fts USING fts5(
		text,
		tokenize = 'unicode61 remove_diacritics 2'
	)`); err != nil {
		return 0, fmt.Errorf("create messages_fts: %w", err)
	}

	sqlStmt := fmt.Sprintf(
		`INSERT INTO messages_fts(rowid, text)
		 SELECT m.Z_PK,
		        TRIM(%s)
		 FROM   ZWAMESSAGE m
		        %s
		 WHERE  %s`,
		// SQLite has no n-ary || that ignores empty strings, so we
		// concat with explicit ' ' separators and TRIM the result.
		strings.Join(selectParts, " || ' ' || "),
		strings.Join(joinParts, "\n        "),
		whereSQL,
	)
	if _, err := db.Exec(sqlStmt); err != nil {
		return 0, fmt.Errorf("populate messages_fts: %w\n--SQL--\n%s", err, sqlStmt)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count messages_fts: %w", err)
	}
	return n, nil
}
