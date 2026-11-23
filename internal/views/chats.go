package views

import (
	"database/sql"
	_ "embed"
	"fmt"
	"time"
)

//go:embed embed/chats.sql
var chatsSQL string

// cocoaEpoch is the Core Data reference date (2001-01-01) as a Unix
// timestamp; ZMESSAGEDATE and wa_chat_read count seconds from it.
const cocoaEpoch = 978307200

// EnsureChats creates v_chats and the read state behind its unread
// column. When the read-state table is new, every chat is seeded as
// read through now, exactly once: unread counts start clean and are
// right for everything after. The phone's ZUNREADCOUNT is not trusted —
// it is only as fresh as the backup. One transaction, so an interrupted
// run leaves no table behind and simply happens again. Returns the
// chats seeded.
func EnsureChats(db *sql.DB, now time.Time) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	existed, err := hasTable(tx, "wa_chat_read")
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(chatsSQL); err != nil {
		return 0, fmt.Errorf("apply chats schema: %w", err)
	}
	var n int64
	if !existed {
		res, err := tx.Exec(`INSERT INTO wa_chat_read (chat_pk, read_through)
			SELECT Z_PK, ? FROM ZWACHATSESSION`, Cocoa(now))
		if err != nil {
			return 0, fmt.Errorf("seed read state: %w", err)
		}
		if n, err = res.RowsAffected(); err != nil {
			return 0, err
		}
	}
	return int(n), tx.Commit()
}

// Cocoa converts t to Cocoa seconds, the unit of ZMESSAGEDATE.
func Cocoa(t time.Time) float64 {
	return float64(t.UnixNano())/1e9 - cocoaEpoch
}
