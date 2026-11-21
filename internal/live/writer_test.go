package live

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatskept/internal/backup"
)

// chatStorageDDL is the minimal slice of the iOS schema the writer
// touches, mirroring the real column sets.
const chatStorageDDL = `
CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER,
	ZCHATSESSION INTEGER, ZGROUPMEMBER INTEGER, ZISFROMME INTEGER,
	ZMESSAGETYPE INTEGER, ZMESSAGESTATUS INTEGER, ZMESSAGEDATE REAL, ZSENTDATE REAL,
	ZFROMJID TEXT, ZTOJID TEXT, ZSTANZAID TEXT, ZTEXT TEXT, ZPARENTMESSAGE INTEGER);
CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER,
	ZSESSIONTYPE INTEGER, ZCONTACTJID TEXT, ZCONTACTIDENTIFIER TEXT,
	ZPARTNERNAME TEXT, ZMESSAGECOUNTER INTEGER,
	ZLASTMESSAGE INTEGER, ZLASTMESSAGEDATE REAL, ZLASTMESSAGETEXT TEXT);
CREATE TABLE ZWAGROUPMEMBER (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER,
	ZCHATSESSION INTEGER, ZISACTIVE INTEGER, ZMEMBERJID TEXT);
CREATE TABLE ZWAMEDIAITEM (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER,
	ZMESSAGE INTEGER, ZMEDIALOCALPATH TEXT, ZFILESIZE INTEGER,
	ZMOVIEDURATION INTEGER, ZAUTHORNAME TEXT, ZTITLE TEXT,
	ZMEDIAKEY BLOB, ZMETADATA BLOB, ZVCARDSTRING TEXT);
CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER, Z_NAME TEXT, Z_SUPER INTEGER, Z_MAX INTEGER);
INSERT INTO Z_PRIMARYKEY VALUES (9, 'WAMessage', 0, 0), (4, 'WAChatSession', 0, 0),
	(6, 'WAGroupMember', 0, 0), (8, 'WAMediaItem', 0, 0);
CREATE VIRTUAL TABLE messages_fts USING fts5(text);
`

// newTestWriter creates a workspace-shaped temp dir with a minimal
// ChatStorage and returns a ready Writer over it.
func newTestWriter(t *testing.T) (*Writer, string) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, backup.ChatStorageName)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(chatStorageDDL); err != nil {
		t.Fatal(err)
	}
	db.Close()
	w := &Writer{dbPath: dbPath, root: root}
	if err := w.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	return w, root
}

func queryDB(t *testing.T, w *Writer, query string, args ...any) *sql.Row {
	t.Helper()
	db, err := openRW(w.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db.QueryRow(query, args...)
}

func dmEvent(stanza, text string) *events.Message {
	v := &events.Message{Message: &waE2E.Message{Conversation: proto.String(text)}}
	v.Info.ID = types.MessageID(stanza)
	v.Info.Chat = types.NewJID("971500000001", types.DefaultUserServer)
	v.Info.Sender = types.NewJID("971500000001", types.DefaultUserServer)
	v.Info.PushName = "Sam"
	v.Info.Timestamp = time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	return v
}

func TestDecideClassification(t *testing.T) {
	// Ordinary text → insert.
	p := decide(dmEvent("S1", "hello"), nil)
	if p.Action != actionInsert || p.Text != "hello" || p.StanzaID != "S1" {
		t.Errorf("text: %+v", p)
	}

	// Status/broadcast noise → skip.
	noise := dmEvent("S2", "story")
	noise.Info.Chat = types.NewJID("status", "broadcast")
	if p := decide(noise, nil); p.Action != actionSkip || !strings.HasPrefix(p.SkipReason, "noise:") {
		t.Errorf("noise: %+v", p)
	}

	// Revoke → target from the protocol key, not the revoke's own ID.
	rev := &events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key:  &waCommon.MessageKey{ID: proto.String("TARGET")},
	}}}
	rev.Info.ID = "R1"
	rev.Info.Chat = types.NewJID("971500000001", types.DefaultUserServer)
	if p := decide(rev, nil); p.Action != actionRevoke || p.TargetID != "TARGET" {
		t.Errorf("revoke: %+v", p)
	}

	// Contentless (sender-key distribution half of the two-event
	// duplicate) → skip, so the content-carrying twin can insert.
	empty := &events.Message{Message: &waE2E.Message{}}
	empty.Info.ID = "S3"
	empty.Info.Chat = types.NewJID("971500000001", types.DefaultUserServer)
	if p := decide(empty, nil); p.Action != actionSkip {
		t.Errorf("contentless: %+v", p)
	}
}

func TestApplyInsertTextDM(t *testing.T) {
	w, _ := newTestWriter(t)
	res, err := w.Apply(context.Background(), decide(dmEvent("S1", "hello there"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != actionInsert || !res.NewChat {
		t.Fatalf("res = %+v", res)
	}

	var text, from, stanza string
	var chatPK int64
	var date float64
	if err := queryDB(t, w, `SELECT ZTEXT, ZFROMJID, ZSTANZAID, ZCHATSESSION, ZMESSAGEDATE
		FROM ZWAMESSAGE WHERE Z_PK = ?`, res.PK).Scan(&text, &from, &stanza, &chatPK, &date); err != nil {
		t.Fatal(err)
	}
	if text != "hello there" || from != "971500000001@s.whatsapp.net" || stanza != "S1" || chatPK != res.ChatPK {
		t.Errorf("row: text=%q from=%q stanza=%q chat=%d", text, from, stanza, chatPK)
	}
	wantDate := float64(time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC).Unix() - cocoaEpoch)
	if date != wantDate {
		t.Errorf("ZMESSAGEDATE = %f, want %f", date, wantDate)
	}

	// Chat created with the partner's push name and counter bumped.
	var partner string
	var counter int
	if err := queryDB(t, w, `SELECT ZPARTNERNAME, ZMESSAGECOUNTER FROM ZWACHATSESSION WHERE Z_PK = ?`,
		res.ChatPK).Scan(&partner, &counter); err != nil {
		t.Fatal(err)
	}
	if partner != "Sam" || counter != 1 {
		t.Errorf("chat: partner=%q counter=%d", partner, counter)
	}

	// FTS row shares the PK; live ledger records it.
	var n int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND text = 'hello there'`,
		res.PK).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts rows = %d, %v", n, err)
	}
	if err := queryDB(t, w, `SELECT COUNT(*) FROM wa_live_pk WHERE pk = ?`, res.PK).Scan(&n); err != nil || n != 1 {
		t.Errorf("wa_live_pk rows = %d, %v", n, err)
	}
}

func TestApplyDedupAndChatReuse(t *testing.T) {
	w, _ := newTestWriter(t)
	ctx := context.Background()
	first, err := w.Apply(ctx, decide(dmEvent("S1", "one"), nil))
	if err != nil {
		t.Fatal(err)
	}

	// Same stanza again → skip, no second row.
	dup, err := w.Apply(ctx, decide(dmEvent("S1", "one"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if dup.Action != actionSkip || dup.Reason != "already present" {
		t.Errorf("dup = %+v", dup)
	}

	// New message, same chat → reused session, not a fork.
	second, err := w.Apply(ctx, decide(dmEvent("S2", "two"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if second.NewChat || second.ChatPK != first.ChatPK {
		t.Errorf("second = %+v (first chat %d)", second, first.ChatPK)
	}
	var counter int
	if err := queryDB(t, w, `SELECT ZMESSAGECOUNTER FROM ZWACHATSESSION WHERE Z_PK = ?`,
		first.ChatPK).Scan(&counter); err != nil || counter != 2 {
		t.Errorf("counter = %d, %v", counter, err)
	}
}

func TestApplyEdit(t *testing.T) {
	w, _ := newTestWriter(t)
	ctx := context.Background()
	ins, err := w.Apply(ctx, decide(dmEvent("S1", "typo"), nil))
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.Apply(ctx, plan{Action: actionEdit, TargetID: "S1", Text: "fixed"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != actionEdit || res.PK != ins.PK {
		t.Fatalf("res = %+v", res)
	}
	var text string
	if err := queryDB(t, w, `SELECT ZTEXT FROM ZWAMESSAGE WHERE Z_PK = ?`, ins.PK).Scan(&text); err != nil || text != "fixed" {
		t.Errorf("text = %q, %v", text, err)
	}
	var n int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND text = 'fixed'`,
		ins.PK).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts = %d, %v", n, err)
	}
}

func imageEvent(stanza, caption string) *events.Message {
	v := &events.Message{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption:       proto.String(caption),
		DirectPath:    proto.String("/v/t62.7118-24/x"),
		MediaKey:      []byte{1},
		FileSHA256:    []byte{2},
		FileEncSHA256: []byte{3},
		FileLength:    proto.Uint64(3),
	}}}
	v.Info.ID = types.MessageID(stanza)
	v.Info.Chat = types.NewJID("971500000001", types.DefaultUserServer)
	v.Info.Sender = types.NewJID("971500000001", types.DefaultUserServer)
	v.Info.Timestamp = time.Date(2026, 8, 27, 11, 0, 0, 0, time.UTC)
	return v
}

func TestApplyMediaInsertAndRevoke(t *testing.T) {
	w, root := newTestWriter(t)
	w.download = func(context.Context, downloadable) ([]byte, error) {
		return []byte("jpgbytes"), nil
	}
	ctx := context.Background()

	res, err := w.Apply(ctx, decide(imageEvent("M1", "sunset"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != actionInsert {
		t.Fatalf("res = %+v", res)
	}

	// File in the enrichment queue, named by PK.
	wantRel := filepath.Join(backup.UnenrichedDir, "media", "1.jpg")
	if res.MediaPath != wantRel {
		t.Errorf("MediaPath = %q, want %q", res.MediaPath, wantRel)
	}
	data, err := os.ReadFile(filepath.Join(root, wantRel))
	if err != nil || string(data) != "jpgbytes" {
		t.Errorf("queued file: %q, %v", data, err)
	}

	// Media item row: caption in ZTITLE, size from the real bytes, and
	// a .jpg local path (the extension queue selection filters on).
	var title, localPath string
	var size int64
	if err := queryDB(t, w, `SELECT ZTITLE, ZMEDIALOCALPATH, ZFILESIZE FROM ZWAMEDIAITEM WHERE ZMESSAGE = ?`,
		res.PK).Scan(&title, &localPath, &size); err != nil {
		t.Fatal(err)
	}
	if title != "sunset" || size != int64(len("jpgbytes")) || !strings.HasSuffix(localPath, ".jpg") {
		t.Errorf("media item: title=%q size=%d path=%q", title, size, localPath)
	}
	// ZTEXT stays NULL for media; the caption is the FTS surface.
	var text sql.NullString
	if err := queryDB(t, w, `SELECT ZTEXT FROM ZWAMESSAGE WHERE Z_PK = ?`, res.PK).Scan(&text); err != nil || text.Valid {
		t.Errorf("ZTEXT = %+v, want NULL", text)
	}
	var n int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND text = 'sunset'`,
		res.PK).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts = %d, %v", n, err)
	}

	// Revoke: tombstone, FTS gone, queued file gone.
	rev, err := w.Apply(ctx, plan{Action: actionRevoke, TargetID: "M1"})
	if err != nil {
		t.Fatal(err)
	}
	if rev.Action != actionRevoke || rev.PK != res.PK {
		t.Fatalf("rev = %+v", rev)
	}
	var msgType int
	if err := queryDB(t, w, `SELECT ZMESSAGETYPE FROM ZWAMESSAGE WHERE Z_PK = ?`, res.PK).Scan(&msgType); err != nil || msgType != 14 {
		t.Errorf("type = %d, %v", msgType, err)
	}
	if _, err := os.Stat(filepath.Join(root, wantRel)); !os.IsNotExist(err) {
		t.Errorf("revoked file still present: %v", err)
	}
	if err := queryDB(t, w, `SELECT COUNT(*) FROM messages_fts WHERE rowid = ?`, res.PK).Scan(&n); err != nil || n != 0 {
		t.Errorf("fts after revoke = %d, %v", n, err)
	}
}

func TestApplyStickerQueuedAsImage(t *testing.T) {
	w, root := newTestWriter(t)
	w.download = func(context.Context, downloadable) ([]byte, error) {
		return []byte("webpbytes"), nil
	}
	ev := dmEvent("S1", "")
	ev.Message = &waE2E.Message{StickerMessage: &waE2E.StickerMessage{
		DirectPath:  proto.String("/v/sticker"),
		Mimetype:    proto.String("image/webp"),
		ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("QUOTED")},
	}}
	p := decide(ev, nil)
	if p.Media == nil || p.QuotedID != "QUOTED" {
		t.Fatalf("plan = %+v", p)
	}
	res, err := w.Apply(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != actionInsert {
		t.Fatalf("res = %+v", res)
	}
	wantRel := filepath.Join(backup.UnenrichedDir, "media", "1.webp")
	if res.MediaPath != wantRel {
		t.Errorf("MediaPath = %q, want %q", res.MediaPath, wantRel)
	}
	if _, err := os.Stat(filepath.Join(root, wantRel)); err != nil {
		t.Errorf("queued file: %v", err)
	}
	var msgType int
	var localPath string
	if err := queryDB(t, w, `SELECT m.ZMESSAGETYPE, mi.ZMEDIALOCALPATH FROM ZWAMESSAGE m
		JOIN ZWAMEDIAITEM mi ON mi.ZMESSAGE = m.Z_PK WHERE m.Z_PK = ?`, res.PK).Scan(&msgType, &localPath); err != nil {
		t.Fatal(err)
	}
	if msgType != 15 || !strings.HasSuffix(localPath, ".webp") {
		t.Errorf("type = %d, path = %q", msgType, localPath)
	}
}

func TestApplyGIFQueued(t *testing.T) {
	w, root := newTestWriter(t)
	w.download = func(context.Context, downloadable) ([]byte, error) {
		return []byte("mp4bytes"), nil
	}
	video := func(stanza string, gif bool) *events.Message {
		ev := dmEvent(stanza, "")
		ev.Message = &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			DirectPath:  proto.String("/v/clip"),
			GifPlayback: proto.Bool(gif),
		}}
		return ev
	}
	res, err := w.Apply(context.Background(), decide(video("G1", true), nil))
	if err != nil {
		t.Fatal(err)
	}
	wantRel := filepath.Join(backup.UnenrichedDir, "media", "1.mp4")
	if res.Action != actionInsert || res.MediaPath != wantRel {
		t.Fatalf("res = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, wantRel)); err != nil {
		t.Errorf("queued file: %v", err)
	}
	var msgType int
	if err := queryDB(t, w, `SELECT ZMESSAGETYPE FROM ZWAMESSAGE WHERE Z_PK = ?`, res.PK).Scan(&msgType); err != nil || msgType != 11 {
		t.Errorf("type = %d, %v", msgType, err)
	}
}

func TestApplyVideo(t *testing.T) {
	cases := []struct {
		name       string
		seconds    uint32
		size       uint64
		downloaded bool
	}{
		{"short", 60, 9_000_000, true},
		{"at the limits", videoMaxSeconds, videoMaxBytes, true},
		{"too long", videoMaxSeconds + 1, 9_000_000, false},
		{"too big", 60, videoMaxBytes + 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, root := newTestWriter(t)
			downloads := 0
			w.download = func(context.Context, downloadable) ([]byte, error) {
				downloads++
				return []byte("mp4bytes"), nil
			}
			ev := dmEvent("V1", "")
			ev.Message = &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
				DirectPath: proto.String("/v/clip"),
				Caption:    proto.String("holiday"),
				Seconds:    proto.Uint32(c.seconds),
				FileLength: proto.Uint64(c.size),
			}}
			ctx := context.Background()
			res, err := w.Apply(ctx, decide(ev, nil))
			if err != nil {
				t.Fatal(err)
			}
			if res.Action != actionInsert {
				t.Fatalf("res = %+v", res)
			}
			if got := downloads == 1; got != c.downloaded {
				t.Errorf("downloads = %d", downloads)
			}
			wantRel := ""
			if c.downloaded {
				wantRel = filepath.Join(backup.UnenrichedDir, "media", "1.m4v")
				if _, err := os.Stat(filepath.Join(root, wantRel)); err != nil {
					t.Errorf("queued file: %v", err)
				}
			}
			if res.MediaPath != wantRel {
				t.Errorf("MediaPath = %q, want %q", res.MediaPath, wantRel)
			}
			var msgType int
			var localPath, title string
			if err := queryDB(t, w, `SELECT m.ZMESSAGETYPE, mi.ZMEDIALOCALPATH, mi.ZTITLE FROM ZWAMESSAGE m
				JOIN ZWAMEDIAITEM mi ON mi.ZMESSAGE = m.Z_PK WHERE m.Z_PK = ?`, res.PK).Scan(&msgType, &localPath, &title); err != nil {
				t.Fatal(err)
			}
			if msgType != 2 || !strings.HasSuffix(localPath, ".mp4") || title != "holiday" {
				t.Errorf("type = %d, path = %q, title = %q", msgType, localPath, title)
			}
			if _, err := w.Apply(ctx, plan{Action: actionRevoke, TargetID: "V1"}); err != nil {
				t.Errorf("revoke: %v", err)
			}
		})
	}
}

func TestApplyMediaDownloadFailureSkips(t *testing.T) {
	w, root := newTestWriter(t)
	w.download = func(context.Context, downloadable) ([]byte, error) {
		return nil, errors.New("cdn said no")
	}
	res, err := w.Apply(context.Background(), decide(imageEvent("M1", ""), nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != actionSkip || !strings.Contains(res.Reason, "download failed") {
		t.Fatalf("res = %+v", res)
	}
	// No row, no file: the next backup brings both together.
	var n int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM ZWAMESSAGE`).Scan(&n); err != nil || n != 0 {
		t.Errorf("messages = %d, %v", n, err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, backup.UnenrichedDir, "media"))
	if len(entries) != 0 {
		t.Errorf("unexpected queued files: %v", entries)
	}
}

func TestMediaPathFor(t *testing.T) {
	cases := []struct {
		pk      int64
		msgType int
		local   string
		want    string
	}{
		{7, 1, "Media/x/a/b/uuid.jpg", filepath.Join(backup.UnenrichedDir, "media", "7.jpg")},
		{8, 3, "Media/x/a/b/uuid.opus", filepath.Join(backup.UnenrichedDir, "voice", "8.opus")},
		{9, 8, "Media/x/a/b/uuid.docx", filepath.Join(backup.UnenrichedDir, "documents", "9.docx")},
		{12, 15, "Media/x/a/b/uuid.webp", filepath.Join(backup.UnenrichedDir, "media", "12.webp")},
		{13, 11, "Media/x/a/b/uuid.mp4", filepath.Join(backup.UnenrichedDir, "media", "13.mp4")},
		{14, 2, "Media/x/a/b/uuid.mp4", filepath.Join(backup.UnenrichedDir, "media", "14.m4v")},
		{10, 0, "", ""},
		{11, 14, "", ""},
	}
	for _, c := range cases {
		if got := mediaPathFor(c.pk, c.msgType, c.local); got != c.want {
			t.Errorf("mediaPathFor(%d, %d, %q) = %q, want %q", c.pk, c.msgType, c.local, got, c.want)
		}
	}
}

func reactionEvent(stanza, target, emoji string) *events.Message {
	v := dmEvent(stanza, "")
	v.Message = &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key:  &waCommon.MessageKey{ID: proto.String(target)},
		Text: proto.String(emoji),
	}}
	return v
}

func reactionRows(t *testing.T, w *Writer) string {
	t.Helper()
	var got sql.NullString
	if err := queryDB(t, w, `SELECT group_concat(sender_jid || '=' || emoji || '/' || is_from_me, ' ')
		FROM (SELECT * FROM wa_reaction ORDER BY sender_jid)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got.String
}

func TestDecideReaction(t *testing.T) {
	p := decide(reactionEvent("r1", "m1", "❤️"), nil)
	if p.Action != actionReaction || p.TargetID != "m1" || p.Text != "❤️" {
		t.Errorf("plan = %+v", p)
	}
	// Withdrawing is flagged as a sender revoke; it must not become one.
	v := reactionEvent("r2", "m1", "")
	v.Info.Edit = types.EditAttributeSenderRevoke
	if p := decide(v, nil); p.Action != actionReaction || p.TargetID != "m1" || p.Text != "" {
		t.Errorf("withdrawal plan = %+v", p)
	}
	if p := decide(reactionEvent("r3", "", "❤️"), nil); p.Action != actionSkip {
		t.Errorf("targetless plan = %+v", p)
	}
}

func TestReactionLifecycle(t *testing.T) {
	w, _ := newTestWriter(t)
	ctx := context.Background()
	apply := func(v *events.Message) {
		t.Helper()
		res, err := w.Apply(ctx, decide(v, nil))
		if err != nil {
			t.Fatal(err)
		}
		if res.Action != actionReaction {
			t.Fatalf("result = %+v", res)
		}
	}
	if _, err := w.Apply(ctx, decide(dmEvent("m1", "hello"), nil)); err != nil {
		t.Fatal(err)
	}
	const sam = "971500000001@s.whatsapp.net"

	apply(reactionEvent("r1", "m1", "❤️"))
	if got := reactionRows(t, w); got != sam+"=❤️/0" {
		t.Fatalf("after react: %q", got)
	}
	apply(reactionEvent("r2", "m1", "😂")) // a new emoji replaces
	if got := reactionRows(t, w); got != sam+"=😂/0" {
		t.Fatalf("after change: %q", got)
	}

	// Mine sits beside theirs, with no sender, as in the phone's blobs.
	v := reactionEvent("r3", "m1", "👍")
	v.Info.IsFromMe = true
	apply(v)
	if got := reactionRows(t, w); got != "=👍/1 "+sam+"=😂/0" {
		t.Fatalf("after mine: %q", got)
	}

	gone := reactionEvent("r4", "m1", "")
	gone.Info.Edit = types.EditAttributeSenderRevoke
	apply(gone)
	if got := reactionRows(t, w); got != "=👍/1" {
		t.Fatalf("after withdrawal: %q", got)
	}
	// The message reacted to is untouched by all of it.
	var msgType int
	var text string
	if err := queryDB(t, w, `SELECT ZMESSAGETYPE, ZTEXT FROM ZWAMESSAGE WHERE ZSTANZAID = 'm1'`).Scan(&msgType, &text); err != nil {
		t.Fatal(err)
	}
	if msgType != 0 || text != "hello" {
		t.Errorf("message after withdrawal: type %d text %q", msgType, text)
	}
}

// The phone may have filed a reaction under the phone JID while live
// sees the LID: changing it must replace that row, not add a second.
func TestReactionReplacesAcrossLIDAndPhone(t *testing.T) {
	w, _ := newTestWriter(t)
	db, err := openRW(w.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO wa_reaction VALUES ('m1', '971500000001@s.whatsapp.net', 0, '❤️', '2026-01-01 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	v := reactionEvent("r1", "m1", "😂")
	v.Info.Sender = types.NewJID("12345", types.HiddenUserServer)
	v.Info.SenderAlt = types.NewJID("971500000001", types.DefaultUserServer)
	if _, err := w.Apply(context.Background(), decide(v, nil)); err != nil {
		t.Fatal(err)
	}
	if got := reactionRows(t, w); got != "12345@lid=😂/0" {
		t.Errorf("rows = %q", got)
	}
}

func TestReactionBeforeItsMessage(t *testing.T) {
	w, _ := newTestWriter(t)
	if _, err := w.Apply(context.Background(), decide(reactionEvent("r1", "not-yet", "❤️"), nil)); err != nil {
		t.Fatal(err)
	}
	if got := reactionRows(t, w); got == "" {
		t.Error("reaction dropped because its message isn't stored yet")
	}
}

func TestRevokeClearsReactions(t *testing.T) {
	w, _ := newTestWriter(t)
	ctx := context.Background()
	for _, v := range []*events.Message{dmEvent("m1", "hello"), reactionEvent("r1", "m1", "❤️")} {
		if _, err := w.Apply(ctx, decide(v, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Apply(ctx, plan{Action: actionRevoke, TargetID: "m1"}); err != nil {
		t.Fatal(err)
	}
	if got := reactionRows(t, w); got != "" {
		t.Errorf("reactions on a deleted message: %q", got)
	}
}

// A workspace imported before reactions existed gets its history on the
// next live start, once.
func TestEnsureReadyBackfillsReactionsOnce(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, backup.ChatStorageName)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(chatStorageDDL + `
		CREATE TABLE ZWAMESSAGEINFO (Z_PK INTEGER PRIMARY KEY, ZMESSAGE INTEGER, ZRECEIPTINFO BLOB);
		INSERT INTO ZWAMESSAGE (Z_PK, ZSTANZAID) VALUES (1, 'm1');
		INSERT INTO ZWAMESSAGEINFO (ZMESSAGE, ZRECEIPTINFO) VALUES
			(1, x'3A1512130A0272311204F09F918D18F4C5FECAAD322001');`); err != nil {
		t.Fatal(err)
	}
	w := &Writer{dbPath: dbPath, root: root}
	if err := w.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wa_reaction`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("backfilled = %d, %v", n, err)
	}
	if _, err := db.Exec(`DELETE FROM wa_reaction`); err != nil {
		t.Fatal(err)
	}
	if err := w.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM wa_reaction`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows after second start = %d, %v", n, err)
	}
}

// A workspace that never imported starts from backup.CreateEmpty: the
// writer appends to it and the message is visible through the views.
func TestApplyOnEmptyDatabase(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, backup.ChatStorageName)
	if err := backup.CreateEmpty(dbPath); err != nil {
		t.Fatal(err)
	}
	w := &Writer{dbPath: dbPath, root: root}
	if err := w.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := w.Apply(context.Background(), decide(dmEvent("S1", "first ever"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != actionInsert || res.PK != 1 || !res.NewChat {
		t.Fatalf("res = %+v", res)
	}
	var text, chat string
	if err := queryDB(t, w, `SELECT m.text, c.title FROM v_messages m JOIN v_chats c ON c.chat_id = m.chat_id
		WHERE m.rowid = ?`, res.PK).Scan(&text, &chat); err != nil {
		t.Fatal(err)
	}
	if text != "first ever" || chat != "Sam" {
		t.Errorf("v_messages: text=%q chat=%q", text, chat)
	}
	var max int
	if err := queryDB(t, w, `SELECT Z_MAX FROM Z_PRIMARYKEY WHERE Z_NAME = 'WAMessage'`).Scan(&max); err != nil || max != 1 {
		t.Errorf("WAMessage Z_MAX = %d, %v", max, err)
	}
	var n int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'first'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts matches = %d, %v", n, err)
	}
}
