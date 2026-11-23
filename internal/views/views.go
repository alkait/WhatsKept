// Package views turns a freshly extracted ChatStorage.sqlite into the
// query surface the MCP server expects: the SQL view layer (wa_contact,
// wa_jid_alias, wa_document, v_chats, v_messages, wa_reaction,
// v_reactions, wa_chat_read) plus the messages_fts FTS5 index.
package views

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed embed/views.sql
var viewsSQL string

// Apply runs the embedded views.sql against the database at dbPath and
// rebuilds messages_fts. Idempotent — the script drops and recreates
// everything it owns. Returns the number of rows in the FTS index.
func Apply(dbPath string) (int, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec(viewsSQL); err != nil {
		return 0, fmt.Errorf("apply views: %w", err)
	}
	if _, err := EnsureReactions(db); err != nil {
		return 0, fmt.Errorf("apply reactions: %w", err)
	}
	if _, err := EnsureChats(db, time.Now()); err != nil {
		return 0, fmt.Errorf("apply chats: %w", err)
	}
	n, err := rebuildFTS(db)
	if err != nil {
		return 0, fmt.Errorf("rebuild fts: %w", err)
	}
	return n, nil
}

// rebuildFTS wipes and repopulates messages_fts from the current DB
// state. The indexed string per message concatenates every text surface
// that exists: typed text (always), image OCR + descriptions
// (wa_image_text), voice transcripts (wa_voice_text), document
// filenames (wa_document), PDF body text (wa_document_text), and
// link-preview headlines (always). One row per message, built in a
// single SELECT — per-row FTS5 inserts are expensive.
//
// The enrichment tables are probed for existence so this extends
// automatically when enrichment lands; the CREATE VIRTUAL TABLE mirrors
// views.sql exactly — keep the two in sync if the tokenizer changes.
func rebuildFTS(db *sql.DB) (int, error) {
	selectParts := []string{"COALESCE(m.ZTEXT, '')"}
	joinParts := []string{}
	whereParts := []string{"(m.ZTEXT IS NOT NULL AND m.ZTEXT <> '')"}

	type surface struct {
		table   string
		cols    []string // selected, COALESCE'd to ''
		join    string
		nonNull string // qualifies the row for indexing
	}
	for _, s := range []surface{
		{"wa_image_text", []string{"t.ocr_text", "t.description"},
			"LEFT JOIN wa_image_text t ON t.rowid = m.Z_PK", "t.rowid IS NOT NULL"},
		{"wa_voice_text", []string{"v.transcript"},
			"LEFT JOIN wa_voice_text v ON v.rowid = m.Z_PK", "v.rowid IS NOT NULL"},
		{"wa_document", []string{"d.filename"},
			"LEFT JOIN wa_document d ON d.rowid = m.Z_PK", "d.filename IS NOT NULL AND d.filename <> ''"},
		{"wa_document_text", []string{"dt.text"},
			"LEFT JOIN wa_document_text dt ON dt.rowid = m.Z_PK", "dt.rowid IS NOT NULL AND dt.text <> ''"},
	} {
		ok, err := tableExists(db, s.table)
		if err != nil {
			return 0, fmt.Errorf("probe %s: %w", s.table, err)
		}
		if !ok {
			continue
		}
		for _, c := range s.cols {
			selectParts = append(selectParts, "COALESCE("+c+", '')")
		}
		joinParts = append(joinParts, s.join)
		whereParts = append(whereParts, s.nonNull)
	}
	// Link-preview headlines: ZWAMEDIAITEM is part of the iOS schema,
	// always present.
	selectParts = append(selectParts, "COALESCE(mi.ZTITLE, '')")
	joinParts = append(joinParts, "LEFT JOIN ZWAMEDIAITEM mi ON mi.ZMESSAGE = m.Z_PK")
	whereParts = append(whereParts, "mi.ZTITLE IS NOT NULL AND mi.ZTITLE <> ''")

	// A row qualifies if ANY surface has text for it.
	whereSQL := "(" + strings.Join(whereParts, ") OR (") + ")"

	if _, err := db.Exec(`DROP TABLE IF EXISTS messages_fts`); err != nil {
		return 0, fmt.Errorf("drop messages_fts: %w", err)
	}
	if _, err := db.Exec(`CREATE VIRTUAL TABLE messages_fts USING fts5(
		text,
		tokenize = 'unicode61 remove_diacritics 2'
	)`); err != nil {
		return 0, fmt.Errorf("create messages_fts: %w", err)
	}

	// SQLite's || doesn't skip empty strings, so concat with ' '
	// separators and TRIM the result.
	stmt := fmt.Sprintf(
		`INSERT INTO messages_fts(rowid, text)
		 SELECT m.Z_PK, TRIM(%s)
		 FROM   ZWAMESSAGE m
		        %s
		 WHERE  %s`,
		strings.Join(selectParts, " || ' ' || "),
		strings.Join(joinParts, "\n        "),
		whereSQL,
	)
	if _, err := db.Exec(stmt); err != nil {
		return 0, fmt.Errorf("populate messages_fts: %w", err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count messages_fts: %w", err)
	}
	return n, nil
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master
		 WHERE type IN ('table','virtual') AND name = ?`, name,
	).Scan(&n)
	return n > 0, err
}
