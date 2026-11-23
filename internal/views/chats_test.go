package views

import (
	"database/sql"
	"testing"
	"time"
)

// cocoaTime is the instant Cocoa seconds c names.
func cocoaTime(c float64) time.Time {
	return time.Unix(int64(c)+cocoaEpoch, 0)
}

// The first run seeds every chat as read through that moment; later
// messages count, and later runs seed nothing.
func TestEnsureChatsSeedsOnce(t *testing.T) {
	path := writeFixtureDB(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(viewsSQL); err != nil {
		t.Fatal(err)
	}

	// Chat 1 holds incoming messages at 700000000, 200 and 300, and an
	// outgoing one at 100.
	n, err := EnsureChats(db, cocoaTime(700000150))
	if err != nil || n != 1 {
		t.Fatalf("seeded %d, %v; want 1", n, err)
	}
	if got := query1(t, path, `SELECT unread FROM v_chats WHERE chat_id = 1`); got != "2" {
		t.Errorf("unread = %s, want 2 (the two incoming messages after the seed)", got)
	}

	// A chat created after the seed has nothing read: every incoming
	// message counts, but not system rows (6), deleted ones (14) or the
	// user's own.
	if _, err := db.Exec(`
		INSERT INTO ZWACHATSESSION VALUES (2, '971502222222@s.whatsapp.net', NULL, 'Omar', 4, 700000900, 0);
		INSERT INTO ZWAMESSAGE (Z_PK, ZCHATSESSION, ZMESSAGEDATE, ZISFROMME, ZMESSAGETYPE, ZTEXT) VALUES
			(20, 2, 700000500, 0, 0, 'hello'),
			(21, 2, 700000600, 0, 6, NULL),
			(22, 2, 700000700, 0, 14, NULL),
			(23, 2, 700000800, 1, 0, 'hi')`); err != nil {
		t.Fatal(err)
	}
	if n, err := EnsureChats(db, cocoaTime(800000000)); err != nil || n != 0 {
		t.Fatalf("second run seeded %d, %v; want 0", n, err)
	}
	if got := query1(t, path, `SELECT unread FROM v_chats WHERE chat_id = 2`); got != "1" {
		t.Errorf("new chat unread = %s, want 1", got)
	}
	if got := query1(t, path, `SELECT unread FROM v_chats WHERE chat_id = 1`); got != "2" {
		t.Errorf("unread after second run = %s, want 2", got)
	}

	// Marked unread with nothing new: still 1.
	if _, err := db.Exec(`UPDATE wa_chat_read SET read_through = 700000999, unread_marked = 700001000 WHERE chat_pk = 1`); err != nil {
		t.Fatal(err)
	}
	if got := query1(t, path, `SELECT unread FROM v_chats WHERE chat_id = 1`); got != "1" {
		t.Errorf("marked unread = %s, want 1", got)
	}
}

// An import starts clean: everything in the backup counts as read.
func TestVChatsUnreadAfterApply(t *testing.T) {
	path := writeFixtureDB(t)
	if _, err := Apply(path); err != nil {
		t.Fatal(err)
	}
	if got := query1(t, path, `SELECT unread FROM v_chats WHERE chat_id = 1`); got != "0" {
		t.Errorf("unread = %s, want 0", got)
	}
}
