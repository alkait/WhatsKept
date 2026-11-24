package backup

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// writeDB creates a SQLite file at path and runs stmts against it.
func writeDB(t *testing.T, path, stmts string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmts); err != nil {
		t.Fatal(err)
	}
}

const voiceTextDDL = `CREATE TABLE wa_voice_text (
	rowid INTEGER PRIMARY KEY, transcript TEXT, language TEXT,
	duration_sec REAL, model TEXT, generated_at TEXT);`

func TestMergeForwardCarriesEnrichment(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite")
	temp := filepath.Join(dir, "temp.sqlite")
	// Live DB: two enriched voice notes, one enriched image.
	writeDB(t, live, voiceTextDDL+`
		CREATE TABLE wa_image_text (rowid INTEGER PRIMARY KEY, ocr_text TEXT, description TEXT,
			language TEXT, source TEXT, model TEXT, generated_at TEXT);
		INSERT INTO wa_voice_text VALUES (1, 'keep me', '', NULL, 'm', 't');
		INSERT INTO wa_voice_text VALUES (2, 'message deleted on device', '', NULL, 'm', 't');
		INSERT INTO wa_image_text VALUES (3, 'receipt text', 'a receipt', '', 'cloud', 'm', 't');`)
	// Staging DB (fresh extract): messages 1 and 3 still exist, 2 is gone.
	writeDB(t, temp, `
		CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY);
		INSERT INTO ZWAMESSAGE VALUES (1), (3);`)

	if err := mergeForward(live, temp); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", temp)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var transcript string
	if err := db.QueryRow(`SELECT transcript FROM wa_voice_text WHERE rowid = 1`).Scan(&transcript); err != nil || transcript != "keep me" {
		t.Errorf("voice row 1: %q, %v", transcript, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wa_voice_text`).Scan(&n); err != nil || n != 1 {
		t.Errorf("voice rows = %d, want 1 (deleted message's row dropped)", n)
	}
	var ocr string
	if err := db.QueryRow(`SELECT ocr_text FROM wa_image_text WHERE rowid = 3`).Scan(&ocr); err != nil || ocr != "receipt text" {
		t.Errorf("image row 3: %q, %v", ocr, err)
	}
}

// Enrichment attached to live-captured rows must NOT carry forward: a
// live-assigned rowid means a different message in the fresh import.
// The wa_live_pk ledger itself must not carry either.
func TestMergeForwardSkipsLivePKs(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite")
	temp := filepath.Join(dir, "temp.sqlite")
	writeDB(t, live, voiceTextDDL+`
		CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZMESSAGETYPE INTEGER, ZSTANZAID TEXT);
		CREATE TABLE wa_live_pk (pk INTEGER PRIMARY KEY);
		INSERT INTO wa_live_pk VALUES (2);
		INSERT INTO wa_voice_text VALUES (1, 'from backup', '', NULL, 'm', 't');
		INSERT INTO wa_voice_text VALUES (2, 'live capture', '', NULL, 'm', 't');`)
	// The fresh import re-delivers the live-captured message — rowid 2
	// exists but is a DIFFERENT message now.
	writeDB(t, temp, `
		CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZSTANZAID TEXT);
		INSERT INTO ZWAMESSAGE VALUES (1, 'A'), (2, 'B');`)

	if err := mergeForward(live, temp); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", temp)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wa_voice_text`).Scan(&n); err != nil || n != 1 {
		t.Errorf("voice rows = %d, want 1 (live row's enrichment dropped)", n)
	}
	var transcript string
	if err := db.QueryRow(`SELECT transcript FROM wa_voice_text WHERE rowid = 1`).Scan(&transcript); err != nil || transcript != "from backup" {
		t.Errorf("voice row 1: %q, %v", transcript, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'wa_live_pk'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("wa_live_pk carried into staging DB (n=%d)", n)
	}
}

func TestMergeForwardCarriesLiveVideoByStanza(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite")
	temp := filepath.Join(dir, "temp.sqlite")
	writeDB(t, live, voiceTextDDL+`
		CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZMESSAGETYPE INTEGER, ZSTANZAID TEXT);
		CREATE TABLE wa_live_pk (pk INTEGER PRIMARY KEY);
		INSERT INTO wa_live_pk VALUES (5), (6);
		INSERT INTO ZWAMESSAGE VALUES (5, 2, 'VID'), (6, 1, 'IMG');
		INSERT INTO wa_voice_text VALUES (5, 'video speech', '', NULL, 'm', 't');
		INSERT INTO wa_voice_text VALUES (6, 'not a video', '', NULL, 'm', 't');`)
	writeDB(t, temp, `
		CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZSTANZAID TEXT);
		INSERT INTO ZWAMESSAGE VALUES (5, 'OTHER'), (70, 'VID'), (71, 'IMG');`)

	if err := mergeForward(live, temp); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", temp)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rowid int
	var transcript string
	if err := db.QueryRow(`SELECT rowid, transcript FROM wa_voice_text`).Scan(&rowid, &transcript); err != nil {
		t.Fatal(err)
	}
	if rowid != 70 || transcript != "video speech" {
		t.Errorf("row = %d %q, want 70 %q", rowid, transcript, "video speech")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wa_voice_text`).Scan(&n); err != nil || n != 1 {
		t.Errorf("voice rows = %d, %v", n, err)
	}
}

func TestMergeForwardNoEnrichmentTables(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite")
	temp := filepath.Join(dir, "temp.sqlite")
	writeDB(t, live, `CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY);`)
	writeDB(t, temp, `CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY);`)
	if err := mergeForward(live, temp); err != nil {
		t.Fatalf("never-enriched live DB must merge as a no-op: %v", err)
	}
}

func TestMergeForwardCorruptLiveDBFails(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite")
	temp := filepath.Join(dir, "temp.sqlite")
	if err := os.WriteFile(live, []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDB(t, temp, `CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY);`)
	if err := mergeForward(live, temp); err == nil {
		t.Error("corrupt live DB must fail the merge, not silently drop enrichment")
	}
}

// Blob candidate selection must skip rows whose enrichment text is
// already in the DB — re-queueing them would re-pay for it.
func TestSelectBlobCandidatesSkipsEnriched(t *testing.T) {
	dir := t.TempDir()
	db := writeFixtureChatDB(t, dir) // image rowid 1, voice rowid 2, pdf rowid 3, docx rowid 4
	if _, err := db.Exec(voiceTextDDL + `
		CREATE TABLE wa_image_text (rowid INTEGER PRIMARY KEY, ocr_text TEXT);
		INSERT INTO wa_image_text VALUES (1, 'done');
		INSERT INTO wa_voice_text VALUES (2, 'done', '', NULL, 'm', 't');`); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		kind, where string
		want        int
	}{
		{"images", "m.ZMEDIALOCALPATH LIKE '%.jpg'", 0},
		{"voice", "m.ZMEDIALOCALPATH LIKE '%.opus'", 0},
		// wa_document_text doesn't exist → documents unaffected.
		{"documents", "wm.ZMESSAGETYPE = 8 AND m.ZMEDIALOCALPATH IS NOT NULL", 2},
	} {
		excl, err := enrichedExclusion(db, c.kind)
		if err != nil {
			t.Fatal(err)
		}
		got, err := selectBlobCandidates(db, c.where+excl)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != c.want {
			t.Errorf("%s: %d candidates, want %d", c.kind, len(got), c.want)
		}
	}
}

const reactionDDL = `CREATE TABLE wa_reaction (target_stanza TEXT NOT NULL, sender_jid TEXT NOT NULL,
	is_from_me INTEGER NOT NULL, emoji TEXT NOT NULL, ts TEXT NOT NULL,
	PRIMARY KEY (target_stanza, sender_jid)) WITHOUT ROWID;`

// The backup brings the reactions it knows from its own blobs; only
// what live saw after the backup was taken comes across.
func TestMergeForwardReactions(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite")
	temp := filepath.Join(dir, "temp.sqlite")
	writeDB(t, live, reactionDDL+`
		INSERT INTO wa_reaction VALUES
			('old', 'a@lid', 0, '🙈', '2020-01-01 00:00:00'),
			('m1',  'b@lid', 0, '🔥', '2030-01-01 00:00:00');`)
	// 3A.. is field 7: one reaction by "a@lid" to the staged message m1,
	// January 2025 — before the live one above, after the stale one.
	writeDB(t, temp, `
		CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZSTANZAID TEXT, ZMESSAGEDATE REAL);
		CREATE TABLE ZWAMESSAGEINFO (Z_PK INTEGER PRIMARY KEY, ZMESSAGE INTEGER, ZRECEIPTINFO BLOB);
		INSERT INTO ZWAMESSAGE VALUES (1, 'm1', 757382400);
		INSERT INTO ZWAMESSAGEINFO (ZMESSAGE, ZRECEIPTINFO) VALUES
			(1, x'3A160A140A0172120561406C69641A01212080F8A5F9C132');`)

	if err := mergeForward(live, temp); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", temp)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRow(`SELECT group_concat(target_stanza || ':' || sender_jid || ':' || emoji, ' ')
		FROM (SELECT * FROM wa_reaction ORDER BY sender_jid)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "m1:a@lid:! m1:b@lid:🔥" {
		t.Errorf("reactions = %q", got)
	}
}

// Lists live only in the workspace (backups don't hold them), so an
// import keeps live's copy; chats are named by JID, so rows stay valid.
func TestMergeForwardCarriesLists(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite")
	temp := filepath.Join(dir, "temp.sqlite")
	writeDB(t, live, `
		CREATE TABLE wa_list (id INTEGER PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL,
			shown INTEGER NOT NULL, position INTEGER);
		CREATE TABLE wa_list_chat (list_id INTEGER NOT NULL, jid TEXT NOT NULL, phone TEXT,
			PRIMARY KEY (list_id, jid));
		INSERT INTO wa_list VALUES (4, 'Contractors', 'custom', 1, 1), (2, 'Favorites', 'favorites', 1, 3);
		INSERT INTO wa_list_chat VALUES (4, '110000000003@lid', '971500000003@s.whatsapp.net'),
			(2, '120363000000000001@g.us', NULL);`)
	writeDB(t, temp, `CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY);`)

	if err := mergeForward(live, temp); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", temp)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var lists, chats int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM wa_list), (SELECT COUNT(*) FROM wa_list_chat)`).Scan(&lists, &chats); err != nil {
		t.Fatal(err)
	}
	if lists != 2 || chats != 2 {
		t.Fatalf("carried %d lists, %d memberships; want 2, 2", lists, chats)
	}
	var phone string
	if err := db.QueryRow(`SELECT phone FROM wa_list_chat WHERE list_id = 4`).Scan(&phone); err != nil || phone != "971500000003@s.whatsapp.net" {
		t.Fatalf("membership phone: %q, %v", phone, err)
	}

	// A live DB from before lists carries nothing and fails nothing.
	temp2 := filepath.Join(dir, "temp2.sqlite")
	writeDB(t, temp2, `CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY);`)
	old := filepath.Join(dir, "old.sqlite")
	writeDB(t, old, `CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY);`)
	if err := mergeForward(old, temp2); err != nil {
		t.Fatal(err)
	}
}
