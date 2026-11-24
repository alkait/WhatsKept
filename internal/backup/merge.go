package backup

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"

	"whatskept/internal/views"
)

// enrichmentTables maps each blob kind's enrichment-result table.
// These rows are produced by `whatskept enrich` (paid API work) and
// exist only in the live DB — a freshly decrypted backup never has
// them, so re-imports must carry them forward and blob extraction must
// not re-queue what they cover.
var enrichmentTables = map[string]string{
	"images":    "wa_image_text",
	"voice":     "wa_voice_text",
	"documents": "wa_document_text",
}

// mergeForward copies the enrichment tables from the current live DB
// into the staging DB, keeping only rows whose message still exists
// there (messages deleted on the device drop their enrichment too).
// Tables the live DB doesn't have are skipped — nothing to carry.
//
// Reactions travel differently: the backup holds them too, so the
// staging DB gets its own from the blobs, and only those newer than the
// backup come across. They are keyed by stanza ID, so wa_live_pk doesn't
// apply. A reaction withdrawn after the backup was taken does come back:
// nothing records that it is gone.
func mergeForward(livePath, tempPath string) error {
	db, err := sql.Open("sqlite", tempPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := views.EnsureReactions(db); err != nil {
		return fmt.Errorf("read reactions: %w", err)
	}
	if _, err := db.Exec(`ATTACH DATABASE ? AS old`, livePath); err != nil {
		return fmt.Errorf("attach live db: %w", err)
	}
	// Rows written by `whatskept live` carry locally assigned rowids;
	// after a re-import the same number belongs to a DIFFERENT message
	// (the phone re-delivers live-captured messages under its own
	// numbering). Carrying their enrichment forward would attach text
	// to the wrong rows, so those are dropped — the wa_live_pk ledger
	// says which they are. The ledger itself is not carried either: a
	// fresh import contains only the phone's own rows.
	liveExclusion := ""
	var hasLedger int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM old.sqlite_master WHERE type = 'table' AND name = 'wa_live_pk'`,
	).Scan(&hasLedger); err != nil {
		return fmt.Errorf("check wa_live_pk: %w", err)
	}
	if hasLedger > 0 {
		liveExclusion = ` AND rowid NOT IN (SELECT pk FROM old.wa_live_pk)`
	}

	for _, table := range enrichmentTables {
		var ddl string
		err := db.QueryRow(
			`SELECT sql FROM old.sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&ddl)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s schema: %w", table, err)
		}
		// The DDL names the table unqualified, so it creates in main
		// (the staging DB).
		if _, err := db.Exec(ddl); err != nil {
			return fmt.Errorf("create %s: %w", table, err)
		}
		if _, err := db.Exec(fmt.Sprintf(
			`INSERT INTO main.%[1]s SELECT * FROM old.%[1]s
			 WHERE rowid IN (SELECT Z_PK FROM main.ZWAMESSAGE)`+liveExclusion, table)); err != nil {
			return fmt.Errorf("copy %s rows: %w", table, err)
		}
	}

	if hasLedger > 0 {
		if err := carryLiveVideoText(db); err != nil {
			return err
		}
	}

	var hasReactions int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM old.sqlite_master WHERE type = 'table' AND name = 'wa_reaction'`,
	).Scan(&hasReactions); err != nil {
		return fmt.Errorf("check wa_reaction: %w", err)
	}
	if hasReactions > 0 {
		if _, err := db.Exec(`
			INSERT OR REPLACE INTO main.wa_reaction SELECT * FROM old.wa_reaction
			WHERE ts > (SELECT datetime(MAX(ZMESSAGEDATE) + 978307200, 'unixepoch')
			            FROM main.ZWAMESSAGE)`); err != nil {
			return fmt.Errorf("copy wa_reaction rows: %w", err)
		}
	}
	return carryLists(db)
}

// carryLists keeps live's copy of the user's chat lists across an
// import: backups don't hold them, and live only rebuilds them when it
// next connects. Chats are named by JID, so the rows stay valid.
func carryLists(db *sql.DB) error {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM old.sqlite_master WHERE type = 'table' AND name IN ('wa_list', 'wa_list_chat')`,
	).Scan(&n); err != nil {
		return fmt.Errorf("check lists: %w", err)
	}
	if n < 2 {
		return nil
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS main.wa_list (id INTEGER PRIMARY KEY, name TEXT NOT NULL,
			kind TEXT NOT NULL, shown INTEGER NOT NULL, position INTEGER)`,
		`CREATE TABLE IF NOT EXISTS main.wa_list_chat (list_id INTEGER NOT NULL, jid TEXT NOT NULL,
			phone TEXT, PRIMARY KEY (list_id, jid))`,
		`INSERT OR REPLACE INTO main.wa_list (id, name, kind, shown, position)
			SELECT id, name, kind, shown, position FROM old.wa_list`,
		`INSERT OR REPLACE INTO main.wa_list_chat (list_id, jid, phone)
			SELECT list_id, jid, phone FROM old.wa_list_chat`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("carry lists: %w", err)
		}
	}
	return nil
}

// carryLiveVideoText moves live videos' text onto their backup rows,
// matched by stanza ID. Photos, stickers and GIFs are enriched again
// from the backup's files; import skips videos, so this text would
// otherwise be lost.
func carryLiveVideoText(db *sql.DB) error {
	for table, cols := range map[string]string{
		"wa_image_text": "ocr_text, description, language, source, model, generated_at",
		"wa_voice_text": "transcript, language, duration_sec, model, generated_at",
	} {
		var exists int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM old.sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check %s: %w", table, err)
		}
		if exists == 0 {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf(`
			INSERT OR REPLACE INTO main.%[1]s (rowid, %[2]s)
			SELECT nm.Z_PK, %[2]s
			FROM   old.%[1]s t
			JOIN   old.wa_live_pk l ON l.pk = t.rowid
			JOIN   old.ZWAMESSAGE om ON om.Z_PK = t.rowid AND om.ZMESSAGETYPE = 2
			JOIN   main.ZWAMESSAGE nm ON nm.ZSTANZAID = om.ZSTANZAID`, table, cols)); err != nil {
			return fmt.Errorf("carry %s: %w", table, err)
		}
	}
	return nil
}

// enrichedExclusion returns a WHERE-clause suffix excluding media rows
// already enriched (their text is in the DB; the blob must not be
// re-queued and re-paid). Empty when the table doesn't exist yet.
func enrichedExclusion(db *sql.DB, kind string) (string, error) {
	table := enrichmentTables[kind]
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&n); err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}
	return " AND m.ZMESSAGE NOT IN (SELECT rowid FROM " + table + ")", nil
}
