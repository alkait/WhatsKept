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
	MessagesBefore       int `json:"messages_before"`
	MessagesAfter        int `json:"messages_after"`
	MessagesNew          int `json:"messages_new"`
	MessagesDropped      int `json:"messages_dropped"`
	OCRPreserved         int `json:"ocr_preserved"`
	OCRDropped           int `json:"ocr_dropped"`
	MediaIndexCarried    int `json:"media_index_carried"`
	VoicePreserved       int `json:"voice_preserved"`
	VoiceDropped         int `json:"voice_dropped"`
	VoiceIndexCarried    int `json:"voice_index_carried"`
	DocumentPreserved    int `json:"document_preserved"`
	DocumentDropped      int `json:"document_dropped"`
	DocumentIndexCarried int `json:"document_index_carried"`
}

// OrphanPruneStats summarises pruneOrphans's work.
type OrphanPruneStats struct {
	OCRRowsDeleted           int `json:"ocr_rows_deleted"`
	MediaIndexRowsDeleted    int `json:"media_index_rows_deleted"`
	VoiceRowsDeleted         int `json:"voice_rows_deleted"`
	VoiceIndexRowsDeleted    int `json:"voice_index_rows_deleted"`
	DocumentTextRowsDeleted  int `json:"document_text_rows_deleted"`
	DocumentIndexRowsDeleted int `json:"document_index_rows_deleted"`
	PersonFaceRowsDeleted    int `json:"person_face_rows_deleted"`
	MediaFilesDeleted        int `json:"media_files_deleted"`
	MediaFilesFailed         int `json:"media_files_failed"`
	VoiceFilesDeleted        int `json:"voice_files_deleted"`
	VoiceFilesFailed         int `json:"voice_files_failed"`
	DocumentFilesDeleted     int `json:"document_files_deleted"`
	DocumentFilesFailed      int `json:"document_files_failed"`
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
		if err := ensureImageSidecarSchema(db); err != nil {
			return nil, err
		}
		// Copy old rows forward by the old↔new column intersection
		// rather than SELECT *. Two failure modes this avoids: (1) a
		// column-count mismatch when old is narrower than the current
		// schema (would error), and (2) silently dropping columns the
		// old table already had — e.g. a user upgrading who already has
		// cloud `description`/`source`/`model` keeps them.
		imgCols, err := imageTextCopyColumns(db)
		if err != nil {
			return nil, fmt.Errorf("plan wa_image_text copy: %w", err)
		}
		res, err := db.Exec(fmt.Sprintf(
			`INSERT OR REPLACE INTO main.wa_image_text (%[1]s)
			 SELECT %[1]s FROM old.wa_image_text
			 WHERE rowid IN (SELECT Z_PK FROM main.ZWAMESSAGE)`, imgCols),
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

	// wa_document_text + document_index — populated by `whatskept
	// document-index`. Schema is co-created here if the old DB had
	// these tables, then we carry forward rows whose rowid still
	// exists in the new ZWAMESSAGE. Note: the `wa_document` table
	// (filename metadata) is NOT touched here — it's rebuilt
	// deterministically by views.sql on every sync, so it doesn't
	// need carry-forward.
	hadDoc, err := attachedTableExists(db, "old", "wa_document_text")
	if err != nil {
		return nil, fmt.Errorf("probe old.wa_document_text: %w", err)
	}
	if hadDoc {
		if _, err := db.Exec(createDocumentSidecarsSQL); err != nil {
			return nil, fmt.Errorf("create document sidecar tables: %w", err)
		}
		res, err := db.Exec(
			`INSERT OR REPLACE INTO main.wa_document_text
			 SELECT * FROM old.wa_document_text
			 WHERE rowid IN (SELECT Z_PK FROM main.ZWAMESSAGE)`,
		)
		if err != nil {
			return nil, fmt.Errorf("preserve wa_document_text: %w", err)
		}
		if n, e := res.RowsAffected(); e == nil {
			stats.DocumentPreserved = int(n)
		}
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM old.wa_document_text
			 WHERE rowid NOT IN (SELECT Z_PK FROM main.ZWAMESSAGE)`,
		).Scan(&stats.DocumentDropped); err != nil {
			return nil, fmt.Errorf("count dropped document rows: %w", err)
		}
		if had, _ := attachedTableExists(db, "old", "document_index"); had {
			res, err := db.Exec(
				`INSERT OR REPLACE INTO main.document_index
				 SELECT * FROM old.document_index
				 WHERE rowid IN (SELECT Z_PK FROM main.ZWAMESSAGE)`,
			)
			if err != nil {
				return nil, fmt.Errorf("preserve document_index: %w", err)
			}
			if n, e := res.RowsAffected(); e == nil {
				stats.DocumentIndexCarried = int(n)
			}
		}
	}

	// wa_person + wa_person_face — user-authored people tags. wa_person is
	// carried forward UNCONDITIONALLY (a name is irreplaceable user input,
	// not keyed to any one message); wa_person_face is filtered by surviving
	// rowid like the other per-message sidecars.
	hadPerson, err := attachedTableExists(db, "old", "wa_person")
	if err != nil {
		return nil, fmt.Errorf("probe old.wa_person: %w", err)
	}
	if hadPerson {
		if _, err := db.Exec(PersonSidecarsSQL); err != nil {
			return nil, fmt.Errorf("create person sidecar tables: %w", err)
		}
		if _, err := db.Exec(
			`INSERT OR REPLACE INTO main.wa_person SELECT * FROM old.wa_person`); err != nil {
			return nil, fmt.Errorf("preserve wa_person: %w", err)
		}
		if had, _ := attachedTableExists(db, "old", "wa_person_face"); had {
			if _, err := db.Exec(
				`INSERT OR REPLACE INTO main.wa_person_face
				 SELECT * FROM old.wa_person_face
				 WHERE rowid IN (SELECT Z_PK FROM main.ZWAMESSAGE)`); err != nil {
				return nil, fmt.Errorf("preserve wa_person_face: %w", err)
			}
		}
	}

	return stats, nil
}

// PersonSidecarsSQL is the canonical schema for the people-tagging
// sidecar tables. Mirrors the CREATE IF NOT EXISTS in views.sql; keep the
// two in sync. Co-created here so mergeSidecarsForward can recreate the
// tables in a fresh staging DB before copying rows into them.
const PersonSidecarsSQL = `
CREATE TABLE IF NOT EXISTS wa_person (
    person_id  INTEGER PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    hidden     INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT
);
CREATE INDEX IF NOT EXISTS wa_person_name_idx ON wa_person(name);
CREATE TABLE IF NOT EXISTS wa_person_face (
    rowid     INTEGER NOT NULL,
    face_idx  INTEGER NOT NULL,
    person_id INTEGER NOT NULL,
    PRIMARY KEY (rowid, face_idx)
);
CREATE INDEX IF NOT EXISTS wa_person_face_person_idx ON wa_person_face(person_id);
`

// createImageSidecarsSQL is the canonical schema for the
// image-OCR sidecar tables. Embedded as a Go string (rather than a
// .sql file) because it's split between two callers — the
// merge-forward step here, and media-index's setup. Drift between
// the two would be a silent corruption hazard.
//
// Always apply this via ensureImageSidecarSchema, which also runs
// migrateImageSidecar to ADD COLUMN any fields missing on a table
// created by an older whatskept (CREATE TABLE IF NOT EXISTS is a
// no-op against an existing, narrower table).
//
// Schema notes (departures from the Python original, see DESIGN.md):
//   - `source` / `model` record provenance: `source` is 'cloud' for
//     rows produced by the cloud describer; legacy rows from prior
//     on-device runs carry a non-'cloud' value (e.g. 'apple') and are
//     treated as upgradeable. `model` is the cloud model slug.
//   - `description` is a short natural-language summary produced by the
//     cloud describer.
//   - `language` added (the describer returns a best-effort dominant
//     script; useful for "find Arabic-text receipts" queries).
const createImageSidecarsSQL = `
CREATE TABLE IF NOT EXISTS wa_image_text (
    rowid         INTEGER PRIMARY KEY,
    ocr_text      TEXT NOT NULL DEFAULT '',
    language      TEXT NOT NULL DEFAULT '',
    description   TEXT NOT NULL DEFAULT '',
    source        TEXT NOT NULL DEFAULT 'apple',
    model         TEXT NOT NULL DEFAULT '',
    generated_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS media_index (
    rowid          INTEGER PRIMARY KEY,
    manifest_path  TEXT    NOT NULL,
    msg_type       INTEGER NOT NULL,
    status         TEXT    NOT NULL,
    bytes          INTEGER,
    error          TEXT,
    describe_error TEXT,
    attempted_at   TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS media_index_status_idx ON media_index(status);
`

// imageSidecarColumns is the full wa_image_text column set in schema
// order. The canonical list used by migrateImageSidecar (to know what
// must exist) and by the merge-forward copy (to compute the old↔new
// column intersection so `SELECT *` schema drift can't bite us).
var imageSidecarColumns = []string{
	"rowid", "ocr_text", "language",
	"description", "source", "model", "generated_at",
}

// ensureImageSidecarSchema creates wa_image_text + media_index if
// absent, then migrates an older (narrower) wa_image_text up to the
// current column set. Idempotent; the single entry point every caller
// must use instead of exec'ing createImageSidecarsSQL directly.
func ensureImageSidecarSchema(db dbExecQuerier) error {
	if _, err := db.Exec(createImageSidecarsSQL); err != nil {
		return fmt.Errorf("create image sidecar tables: %w", err)
	}
	return migrateImageSidecar(db)
}

// migrateImageSidecar ADD COLUMNs any wa_image_text / media_index field
// introduced after the table was first created by an older whatskept.
// Each ALTER carries a DEFAULT (or is nullable), so existing rows get a
// sane value with no backfill — notably wa_image_text.source defaults to
// 'apple', tagging every pre-migration row as a legacy on-device
// description so the cloud describer treats it as upgradeable.
// media_index.describe_error is nullable: NULL means "no describe
// failure recorded", which is the right default for every pre-split row
// (download and describe were one step then).
func migrateImageSidecar(db dbExecQuerier) error {
	// DDL keyed by (table, column); only post-v1 additions need ALTERs.
	adds := []struct{ table, col, ddl string }{
		{"wa_image_text", "description", `ALTER TABLE wa_image_text ADD COLUMN description TEXT NOT NULL DEFAULT ''`},
		{"wa_image_text", "source", `ALTER TABLE wa_image_text ADD COLUMN source TEXT NOT NULL DEFAULT 'apple'`},
		{"wa_image_text", "model", `ALTER TABLE wa_image_text ADD COLUMN model TEXT NOT NULL DEFAULT ''`},
		{"media_index", "describe_error", `ALTER TABLE media_index ADD COLUMN describe_error TEXT`},
	}
	cols := map[string]map[string]bool{}
	for _, a := range adds {
		have, ok := cols[a.table]
		if !ok {
			var err error
			have, err = tableColumns(db, "main", a.table)
			if err != nil {
				return fmt.Errorf("inspect %s: %w", a.table, err)
			}
			cols[a.table] = have
		}
		// An absent table reports zero columns. Skip it — the
		// CREATE TABLE IF NOT EXISTS in ensureImageSidecarSchema is
		// what creates it; migrate only ADD COLUMNs onto a table that
		// already exists (callers may run migrate on a partial DB).
		if len(have) == 0 || have[a.col] {
			continue
		}
		if _, err := db.Exec(a.ddl); err != nil {
			return fmt.Errorf("add column %s.%s: %w", a.table, a.col, err)
		}
	}
	return nil
}

// imageTextCopyColumns returns the comma-joined wa_image_text columns
// present in BOTH the attached `old` DB and the current `main` DB, in
// canonical schema order — the safe column list for a merge-forward
// copy across a possible schema change.
func imageTextCopyColumns(db dbExecQuerier) (string, error) {
	mainCols, err := tableColumns(db, "main", "wa_image_text")
	if err != nil {
		return "", err
	}
	oldCols, err := tableColumns(db, "old", "wa_image_text")
	if err != nil {
		return "", err
	}
	keep := make([]string, 0, len(imageSidecarColumns))
	for _, c := range imageSidecarColumns {
		if mainCols[c] && oldCols[c] {
			keep = append(keep, c)
		}
	}
	return strings.Join(keep, ", "), nil
}

// tableColumns returns the set of column names on schema.table (e.g.
// "main"."wa_image_text"). Empty set if the table doesn't exist.
func tableColumns(db dbExecQuerier, schema, table string) (map[string]bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA %s.table_info(%s)", schema, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// dbExecQuerier is the subset of *sql.DB used by the schema helpers,
// so they work against either a *sql.DB or a *sql.Tx if ever needed.
type dbExecQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

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

// createDocumentSidecarsSQL is the schema for the document-index
// output tables. Shared by document.go's setup and the merge-
// forward step here (drift between the two would be a silent
// corruption hazard, same rationale as the image / voice sidecars).
//
// Two tables, mirroring the image/voice pattern:
//
//	wa_document_text — extracted body text per PDF. Joined into
//	  messages_fts by rebuildFTS so MATCH 'cardamom' hits a recipe
//	  PDF that the sender attached.
//
//	document_index   — terminal-state ledger driving the per-row
//	  resume logic. status values:
//	     'extracted'   PDFKit and/or Vision OCR returned text;
//	                   wa_document_text row exists.
//	     'extracted_empty'  pipeline ran cleanly but the PDF has
//	                   no recoverable text (image-only PDF whose
//	                   OCR also came back blank). No wa_document_text
//	                   row but the file is on disk.
//	     'missing'     file referenced in DB but not in the iOS
//	                   backup manifest, or stored as a zero-byte
//	                   record (selective backup / not downloaded
//	                   on the phone).
//	     'unsupported' document is not a PDF (xlsx, docx, ...).
//	                   We don't have an extractor for these yet;
//	                   the row is parked here so future runs can
//	                   skip it cheaply.
//	     'error'       decrypt / write / PDFKit / Vision failure.
//
// pages_with_text + pages_ocr add up to <= page_count (some pages
// may have been blank entirely, or skipped due to the OCR cap).
const createDocumentSidecarsSQL = `
CREATE TABLE IF NOT EXISTS wa_document_text (
    rowid           INTEGER PRIMARY KEY,
    text            TEXT NOT NULL DEFAULT '',
    page_count      INTEGER NOT NULL DEFAULT 0,
    pages_with_text INTEGER NOT NULL DEFAULT 0,
    pages_ocr       INTEGER NOT NULL DEFAULT 0,
    method          TEXT NOT NULL DEFAULT '',
    generated_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS document_index (
    rowid         INTEGER PRIMARY KEY,
    manifest_path TEXT    NOT NULL,
    ext           TEXT,
    status        TEXT    NOT NULL,
    bytes         INTEGER,
    page_count    INTEGER,
    error         TEXT,
    attempted_at  TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS document_index_status_idx ON document_index(status);
CREATE INDEX IF NOT EXISTS document_index_ext_idx    ON document_index(ext);
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
func pruneOrphans(dbPath, mediaDir, voiceDir, documentsDir string) (*OrphanPruneStats, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	stats := &OrphanPruneStats{}

	// Pass 1 — DB rows. Loop every sidecar table; skip any that
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
		{"wa_document_text", &stats.DocumentTextRowsDeleted},
		{"document_index", &stats.DocumentIndexRowsDeleted},
		{"wa_person_face", &stats.PersonFaceRowsDeleted},
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
	if mediaDir == "" && voiceDir == "" && documentsDir == "" {
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
	// Documents folder currently only holds PDFs (document-index
	// scope). Same stem-must-be-pure-int rule means a user-dropped
	// README.pdf is left alone.
	if err := pruneOrphanFiles(documentsDir, ".pdf", alive,
		&stats.DocumentFilesDeleted, &stats.DocumentFilesFailed); err != nil {
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
//   - wa_image_text.ocr_text + description        (when wa_image_text exists)
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
	if hasOCR {
		// The FTS SELECT references t.description, so guarantee an
		// older table has it. rebuildFTS is reached from the voice /
		// document indexers too, which don't otherwise migrate the
		// image sidecar — make it self-sufficient here.
		if err := migrateImageSidecar(db); err != nil {
			return 0, fmt.Errorf("migrate wa_image_text: %w", err)
		}
	}
	hasVoice, err := tableExists(db, "wa_voice_text")
	if err != nil {
		return 0, fmt.Errorf("probe wa_voice_text: %w", err)
	}
	hasDoc, err := tableExists(db, "wa_document")
	if err != nil {
		return 0, fmt.Errorf("probe wa_document: %w", err)
	}
	hasDocText, err := tableExists(db, "wa_document_text")
	if err != nil {
		return 0, fmt.Errorf("probe wa_document_text: %w", err)
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
			// description (cloud describer) lets MATCH hit summary words
			// like "necklace" that aren't in the literal OCR.
			"COALESCE(t.description, '')",
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
		// Document filenames are always indexable (no separate
		// indexer required — views.sql rebuilds wa_document on every
		// sync). "passport.pdf", "Estimate_2021.pdf", etc. all
		// become matchable via MATCH.
		selectParts = append(selectParts, "COALESCE(d.filename, '')")
		joinParts = append(joinParts, "LEFT JOIN wa_document d ON d.rowid = m.Z_PK")
		whereParts = append(whereParts, "d.filename IS NOT NULL AND d.filename <> ''")
	}
	if hasDocText {
		// Extracted PDF body text from `whatskept document-index`.
		// Optional — only present once the user has run the indexer.
		selectParts = append(selectParts, "COALESCE(dt.text, '')")
		joinParts = append(joinParts, "LEFT JOIN wa_document_text dt ON dt.rowid = m.Z_PK")
		whereParts = append(whereParts, "dt.rowid IS NOT NULL AND dt.text <> ''")
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
