package live

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"whatskept/internal/backup"
)

var (
	testKey    = bytes.Repeat([]byte{0x11}, 32)
	testEncSHA = bytes.Repeat([]byte{0x22}, 32)
)

// storedBlobs builds ZMEDIAKEY and ZMETADATA the way the phone writes
// them, including fields the decoder must skip.
func storedBlobs(path string) (key, meta []byte) {
	key = protowire.AppendTag(key, 1, protowire.BytesType)
	key = protowire.AppendBytes(key, testKey)
	key = protowire.AppendTag(key, 2, protowire.BytesType)
	key = protowire.AppendBytes(key, testEncSHA)
	key = protowire.AppendTag(key, 3, protowire.VarintType)
	key = protowire.AppendVarint(key, 1788072483)
	key = protowire.AppendTag(key, 4, protowire.VarintType)
	key = protowire.AppendVarint(key, 0)

	meta = protowire.AppendTag(meta, 4, protowire.BytesType)
	meta = protowire.AppendBytes(meta, []byte(path))
	meta = protowire.AppendTag(meta, 35, protowire.VarintType)
	meta = protowire.AppendVarint(meta, 1774334992)
	meta = protowire.AppendTag(meta, 68, protowire.BytesType)
	meta = protowire.AppendBytes(meta, bytes.Repeat([]byte{0x33}, 32))
	return key, meta
}

func TestDecodeStoredMedia(t *testing.T) {
	key, meta := storedBlobs("/v/t62/old-path.enc?oe=1")
	m, err := decodeStoredMedia(key, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.MediaKey, testKey) || !bytes.Equal(m.FileEncSHA256, testEncSHA) ||
		m.KeyTimestamp != 1788072483 || m.DirectPath != "/v/t62/old-path.enc?oe=1" {
		t.Errorf("decoded %+v", m)
	}

	if _, err := decodeStoredMedia(key, nil); err == nil {
		t.Error("no path: want an error")
	}
	if _, err := decodeStoredMedia([]byte{0x0a, 0x20, 0x01}, meta); err == nil {
		t.Error("truncated ZMEDIAKEY: want an error")
	}
}

// importedMedia inserts attachment rows shaped like the phone's, in the
// chat of 971500000001, plus enrichment text for the image.
func importedMedia(t *testing.T, w *Writer) {
	t.Helper()
	if _, err := w.Apply(context.Background(), decide(dmEvent("IN1", "hello"), nil)); err != nil {
		t.Fatal(err)
	}
	key, meta := storedBlobs("/v/t62/old-path.enc")
	db, err := openRW(w.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		INSERT INTO ZWAMESSAGE (Z_PK, ZCHATSESSION, ZISFROMME, ZMESSAGETYPE, ZFROMJID, ZSTANZAID) VALUES
			(50, 1, 0, 1, '971500000001@s.whatsapp.net', 'IMG'),
			(51, 1, 0, 3, '971500000001@s.whatsapp.net', 'VOICE'),
			(52, 1, 0, 8, '971500000001@s.whatsapp.net', 'PDF'),
			(53, 1, 0, 3, '971500000001@s.whatsapp.net', 'SONG'),
			(54, 1, 0, 5, '971500000001@s.whatsapp.net', 'PLACE'),
			(55, 1, 0, 1, '971500000001@s.whatsapp.net', 'LIVEIMG'),
			(56, 1, 0, 14, '971500000001@s.whatsapp.net', 'GONE'),
			(57, 1, 0, 2, '971500000001@s.whatsapp.net', 'VIDEO'),
			(58, 1, 0, 11, '971500000001@s.whatsapp.net', 'GIF'),
			(59, 1, 0, 15, '971500000001@s.whatsapp.net', 'STICKER');
		INSERT INTO ZWAMEDIAITEM (Z_PK, ZMESSAGE, ZMEDIALOCALPATH, ZFILESIZE, ZMOVIEDURATION,
		                          ZAUTHORNAME, ZTITLE, ZMEDIAKEY, ZMETADATA, ZVCARDSTRING) VALUES
			(1, 50, 'Media/x/a.jpg',  1234, 0, NULL, 'the receipt', ?1, ?2, 'image/jpeg'),
			(2, 51, 'Media/x/b.opus', 999,  7, NULL, NULL,          ?1, ?2, 'audio/ogg; codecs=opus'),
			(3, 52, 'Media/x/c.pdf',  5555, 0, 'scan.pdf', NULL,    ?1, ?2, 'application/pdf'),
			(4, 53, 'Media/x/d.mp3',  777,  0, NULL, NULL,          ?1, ?2, 'audio/mpeg'),
			(5, 55, 'Media/x/e.jpg',  1234, 0, NULL, NULL,          NULL, NULL, NULL),
			(6, 57, 'Media/x/f.mp4',  4000, 30, NULL, 'holiday',    ?1, ?2, 'video/mp4'),
			(7, 58, 'Media/x/g.mp4',  500,  3, NULL, NULL,          ?1, ?2, 'video/mp4'),
			(8, 59, 'Media/x/h.webp', 200,  0, NULL, NULL,          ?1, ?2, 'image/webp');
		CREATE TABLE wa_image_text (rowid INTEGER PRIMARY KEY, ocr_text TEXT, description TEXT,
			language TEXT, source TEXT, model TEXT, generated_at TEXT);
		INSERT INTO wa_image_text VALUES (50, 'TOTAL 42.00', 'a supermarket receipt', 'en', 'photo', 'm', 't');`,
		key, meta); err != nil {
		t.Fatal(err)
	}
}

func TestForwardText(t *testing.T) {
	w, root, wire := sendFixture(t)
	if _, err := w.Apply(context.Background(), decide(dmEvent("IN1", "lunch at noon?"), nil)); err != nil {
		t.Fatal(err)
	}

	sent, err := Forward(context.Background(), root, "+971500000002", "IN1")
	if err != nil {
		t.Fatal(err)
	}
	ext := wire.msgs[0].GetExtendedTextMessage()
	if wire.sent[0] != "971500000002@s.whatsapp.net lunch at noon?" ||
		!ext.GetContextInfo().GetIsForwarded() || ext.GetContextInfo().GetForwardingScore() != 1 {
		t.Errorf("wire = %q, context = %v", wire.sent, ext.GetContextInfo())
	}
	if len(wire.fetched) != 0 {
		t.Errorf("fetched %q for a text forward", wire.fetched)
	}

	// Recorded in the destination chat, not the original's.
	var text, to string
	if err := queryDB(t, w, `SELECT ZTEXT, ZTOJID FROM ZWAMESSAGE WHERE ZSTANZAID = ?`, sent.ID).Scan(&text, &to); err != nil {
		t.Fatal(err)
	}
	if text != "lunch at noon?" || to != "971500000002@s.whatsapp.net" {
		t.Errorf("row: text=%q to=%q", text, to)
	}
}

func TestForwardMedia(t *testing.T) {
	w, root, wire := sendFixture(t)
	importedMedia(t, w)
	fresh, sum := "/v/fresh-path", []byte("0123456789abcdef0123456789abcdef")

	sent, err := Forward(context.Background(), root, "+971500000002", "IMG")
	if err != nil {
		t.Fatal(err)
	}
	img := wire.msgs[0].GetImageMessage()
	if img.GetDirectPath() != fresh || img.GetURL() != "https://mmg.whatsapp.net"+fresh ||
		!bytes.Equal(img.GetMediaKey(), testKey) || !bytes.Equal(img.GetFileEncSHA256(), testEncSHA) ||
		!bytes.Equal(img.GetFileSHA256(), sum) || img.GetFileLength() != 1234 ||
		img.GetMimetype() != "image/jpeg" || img.GetCaption() != "the receipt" ||
		img.GetMediaKeyTimestamp() != 1788072483 || !img.GetContextInfo().GetIsForwarded() {
		t.Errorf("image = %v", img)
	}

	// Recorded as an image in the destination chat, with the original's
	// description copied and searchable — and no file queued, because
	// nothing was downloaded to enrich.
	var msgType int
	var to string
	var pk int64
	if err := queryDB(t, w, `SELECT Z_PK, ZMESSAGETYPE, ZTOJID FROM ZWAMESSAGE WHERE ZSTANZAID = ?`, sent.ID).
		Scan(&pk, &msgType, &to); err != nil {
		t.Fatal(err)
	}
	if msgType != 1 || to != "971500000002@s.whatsapp.net" {
		t.Errorf("row: type=%d to=%q", msgType, to)
	}
	var desc string
	if err := queryDB(t, w, `SELECT description FROM wa_image_text WHERE rowid = ?`, pk).Scan(&desc); err != nil || desc != "a supermarket receipt" {
		t.Errorf("copied description = %q, err = %v", desc, err)
	}
	var hit int64
	if err := queryDB(t, w, `SELECT rowid FROM messages_fts WHERE messages_fts MATCH 'supermarket' AND rowid = ?`, pk).Scan(&hit); err != nil {
		t.Errorf("forward not searchable by the copied description: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, backup.UnenrichedDir, "media")); len(entries) != 0 {
		t.Errorf("queued %d file(s) for a forward", len(entries))
	}

	// A voice note stays a voice note; a document keeps its filename.
	if _, err := Forward(context.Background(), root, "+971500000002", "VOICE"); err != nil {
		t.Fatal(err)
	}
	if a := wire.msgs[1].GetAudioMessage(); !a.GetPTT() || a.GetSeconds() != 7 || a.GetMimetype() != "audio/ogg; codecs=opus" {
		t.Errorf("voice = %v", a)
	}
	if _, err := Forward(context.Background(), root, "120363000000000001@g.us", "PDF"); err != nil {
		t.Fatal(err)
	}
	if d := wire.msgs[2].GetDocumentMessage(); d.GetFileName() != "scan.pdf" || d.GetMimetype() != "application/pdf" {
		t.Errorf("document = %v", d)
	}
	var docHit int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH '"scan.pdf"'`).Scan(&docHit); err != nil || docHit != 1 {
		t.Errorf("forwarded document searchable by filename: hits = %d, err = %v", docHit, err)
	}
	if strings.Join(wire.fetched, ",") != "IMG,VOICE,PDF" {
		t.Errorf("fetched = %q", wire.fetched)
	}
}

// Each stays its own kind of message, recorded as such in the
// destination chat.
func TestForwardVideoGIFSticker(t *testing.T) {
	w, root, wire := sendFixture(t)
	importedMedia(t, w)
	ctx := context.Background()
	recorded := func(id string) int {
		t.Helper()
		var msgType int
		if err := queryDB(t, w, `SELECT ZMESSAGETYPE FROM ZWAMESSAGE WHERE ZSTANZAID = ?`, id).Scan(&msgType); err != nil {
			t.Fatal(err)
		}
		return msgType
	}

	sent, err := Forward(ctx, root, "+971500000002", "VIDEO")
	if err != nil {
		t.Fatal(err)
	}
	v := wire.msgs[0].GetVideoMessage()
	if v == nil || v.GetGifPlayback() || v.GetSeconds() != 30 || v.GetCaption() != "holiday" ||
		v.GetMimetype() != "video/mp4" || !v.GetContextInfo().GetIsForwarded() {
		t.Errorf("video = %v", v)
	}
	if got := recorded(sent.ID); got != 2 {
		t.Errorf("video recorded as type %d", got)
	}

	if sent, err = Forward(ctx, root, "+971500000002", "GIF"); err != nil {
		t.Fatal(err)
	}
	if g := wire.msgs[1].GetVideoMessage(); g == nil || !g.GetGifPlayback() {
		t.Errorf("gif = %v", g)
	}
	if got := recorded(sent.ID); got != 11 {
		t.Errorf("GIF recorded as type %d", got)
	}

	if sent, err = Forward(ctx, root, "+971500000002", "STICKER"); err != nil {
		t.Fatal(err)
	}
	st := wire.msgs[2].GetStickerMessage()
	if st == nil || st.GetMimetype() != "image/webp" || !st.GetContextInfo().GetIsForwarded() {
		t.Errorf("sticker = %v", st)
	}
	if got := recorded(sent.ID); got != 15 {
		t.Errorf("sticker recorded as type %d", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, backup.UnenrichedDir, "media")); len(entries) != 0 {
		t.Errorf("queued %d file(s) for forwards", len(entries))
	}
}

// Media live captured itself forwards like imported media: capture
// stores the download details forward reads.
func TestForwardLiveCapture(t *testing.T) {
	w, root, wire := sendFixture(t)
	w.download = func(context.Context, downloadable) ([]byte, error) { return []byte("jpgbytes"), nil }
	v := imageEvent("CAPTURED", "sunset")
	img := v.Message.GetImageMessage()
	img.MediaKey, img.FileEncSHA256 = testKey, testEncSHA
	img.Mimetype, img.MediaKeyTimestamp = proto.String("image/png"), proto.Int64(1788072483)
	if _, err := w.Apply(context.Background(), decide(v, nil)); err != nil {
		t.Fatal(err)
	}

	var key, meta []byte
	if err := queryDB(t, w, `SELECT ZMEDIAKEY, ZMETADATA FROM ZWAMEDIAITEM`).Scan(&key, &meta); err != nil {
		t.Fatal(err)
	}
	if m, err := decodeStoredMedia(key, meta); err != nil || m.DirectPath != "/v/t62.7118-24/x" {
		t.Errorf("stored details = %+v, %v", m, err)
	}

	if _, err := Forward(context.Background(), root, "+971500000002", "CAPTURED"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(wire.fetched, ",") != "CAPTURED" {
		t.Errorf("fetched = %q", wire.fetched)
	}
	got := wire.msgs[0].GetImageMessage()
	if !bytes.Equal(got.GetMediaKey(), testKey) || !bytes.Equal(got.GetFileEncSHA256(), testEncSHA) ||
		got.GetMediaKeyTimestamp() != 1788072483 || got.GetMimetype() != "image/png" || got.GetCaption() != "sunset" {
		t.Errorf("image = %v", got)
	}
}

func TestForwardRefusals(t *testing.T) {
	w, root, wire := sendFixture(t)
	importedMedia(t, w)
	var before int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM ZWAMESSAGE`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ id, want string }{
		{"NOPE", "no message with stanza ID"},
		{"GONE", "was deleted"},
		{"SONG", "only voice notes"},
		{"PLACE", "cannot be forwarded yet"},
		{"LIVEIMG", "download details are not stored"},
	} {
		_, err := Forward(context.Background(), root, "+971500000002", c.id)
		if err == nil || !strings.Contains(err.Error(), "cannot forward") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Forward(%s): err = %v, want %q", c.id, err, c.want)
		}
	}

	// A forward carries nothing of the caller's own.
	_, err := post[sendReply](context.Background(), root, "send", sendRequest{To: "+971500000002", Text: "look", Forward: "IN1"})
	if err == nil || !strings.Contains(err.Error(), "no text or reply_to") {
		t.Errorf("forward with text: err = %v", err)
	}

	// The phone no longer has the file.
	wire.fetchErr = errors.New("media no longer available — the phone answered NOT_FOUND")
	if _, err := Forward(context.Background(), root, "+971500000002", "IMG"); err == nil ||
		!strings.Contains(err.Error(), "media no longer available") {
		t.Errorf("unavailable media: err = %v", err)
	}

	var after int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM ZWAMESSAGE`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if len(wire.sent) != 0 || after != before {
		t.Errorf("wire=%q rows %d→%d, want nothing sent or stored", wire.sent, before, after)
	}
}
