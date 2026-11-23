package live

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatskept/internal/workspace"
)

// at is the instant Cocoa seconds c names.
func at(c float64) time.Time {
	return time.Unix(int64(c)+cocoaEpoch, 0)
}

// unreadOf is the chat's v_chats.unread. The test schema lacks the
// name tables v_chats also reads; they are added empty.
func unreadOf(t *testing.T, w *Writer, pk int64) int {
	t.Helper()
	db, err := openRW(w.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS wa_contact (jid TEXT, display_name TEXT);
		CREATE TABLE IF NOT EXISTS ZWAPROFILEPUSHNAME (ZJID TEXT, ZPUSHNAME TEXT)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT unread FROM v_chats WHERE chat_id = ?`, pk).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Only the user's own reading counts: their read receipts, plain or
// read-self, and their "Mark as read/unread".
func TestReadChangeOf(t *testing.T) {
	me := types.NewJID("971500000099", types.DefaultUserServer)
	chat := types.NewJID("110000000001", types.HiddenUserServer)
	when := at(800000000)
	receipt := func(fromMe bool, typ types.ReceiptType, c types.JID, ts time.Time) *events.Receipt {
		r := &events.Receipt{Type: typ, Timestamp: ts, MessageIDs: []types.MessageID{"M1"}}
		r.Chat, r.Sender, r.IsFromMe = c, me, fromMe
		return r
	}
	mark := func(read bool) *events.MarkChatAsRead {
		return &events.MarkChatAsRead{JID: chat, Timestamp: when,
			Action: &waSyncAction.MarkChatAsReadAction{Read: proto.Bool(read)}}
	}
	for _, c := range []struct {
		name string
		evt  any
		want *readChange
	}{
		{"own read receipt", receipt(true, types.ReceiptTypeRead, chat, when),
			&readChange{Chat: chat, Read: true, At: when, Via: "receipt"}},
		{"own read-self receipt", receipt(true, types.ReceiptTypeReadSelf, chat, when),
			&readChange{Chat: chat, Read: true, At: when, Via: "receipt"}},
		{"someone reading the user's message", receipt(false, types.ReceiptTypeRead, chat, when), nil},
		{"own delivery receipt", receipt(true, types.ReceiptTypeDelivered, chat, when), nil},
		{"own played receipt", receipt(true, types.ReceiptTypePlayed, chat, when), nil},
		{"receipt without a time", receipt(true, types.ReceiptTypeRead, chat, time.Time{}), nil},
		{"status update read", receipt(true, types.ReceiptTypeRead, types.StatusBroadcastJID, when), nil},
		{"marked read", mark(true), &readChange{Chat: chat, Read: true, At: when, Via: "mark"}},
		{"marked unread", mark(false), &readChange{Chat: chat, Read: false, At: when, Via: "mark"}},
		{"a message", &events.Message{}, nil},
	} {
		got, ok := readChangeOf(c.evt)
		switch {
		case c.want == nil && ok:
			t.Errorf("%s: got %+v, want nothing", c.name, got)
		case c.want != nil && (!ok || got != *c.want):
			t.Errorf("%s: got %+v %v, want %+v", c.name, got, ok, *c.want)
		}
	}
}

// A read change finds the chat by phone JID or LID, and the newest
// change wins whatever order they arrive in.
func TestSetRead(t *testing.T) {
	w := archiveWriter(t)
	ctx := context.Background()
	// Chat 2 has one incoming message, at 800000000; nothing is read yet.
	if n := unreadOf(t, w, 2); n != 1 {
		t.Fatalf("unread before = %d, want 1", n)
	}
	step := func(name string, chat string, read bool, ts float64, want int) {
		t.Helper()
		jid, _ := types.ParseJID(chat)
		pk, ok, err := w.SetRead(ctx, jid, read, at(ts))
		if err != nil || !ok || pk != 2 {
			t.Fatalf("%s: pk=%d ok=%v err=%v", name, pk, ok, err)
		}
		if n := unreadOf(t, w, 2); n != want {
			t.Errorf("%s: unread = %d, want %d", name, n, want)
		}
	}
	// Receipts name a DM by its LID; the chat is stored by phone JID.
	step("read via LID", "110000000001@lid", true, 800000100, 0)
	step("an older receipt changes nothing", "971500000001@s.whatsapp.net", true, 799999999, 0)
	step("marked unread", "971500000001@s.whatsapp.net", false, 800000300, 1)
	step("a read older than the mark leaves it", "110000000001@lid", true, 800000250, 1)
	step("a newer read clears it", "110000000001@lid", true, 800000400, 0)
	step("a mark older than the read is stale", "971500000001@s.whatsapp.net", false, 800000350, 0)

	// Stored by LID alone, named by phone JID.
	if pk, ok, err := w.SetRead(ctx, types.NewJID("971500000003", types.DefaultUserServer), true, at(800000500)); err != nil || !ok || pk != 4 {
		t.Errorf("phone JID for a LID chat: pk=%d ok=%v err=%v", pk, ok, err)
	}
	if n := unreadOf(t, w, 4); n != 0 {
		t.Errorf("chat 4 unread = %d, want 0", n)
	}

	if _, ok, err := w.SetRead(ctx, types.NewJID("971509999999", types.DefaultUserServer), true, at(800000500)); err != nil || ok {
		t.Errorf("unknown chat: ok=%v err=%v", ok, err)
	}
}

// A read change goes on the wire addressed like an archive change —
// a DM by its LID, with the chat's latest message — and is in
// v_chats.unread at once.
func TestMarkReadThroughLive(t *testing.T) {
	w, root, wire := archiveFixture(t)
	ctx := context.Background()

	got, err := MarkRead(ctx, root, "+971 50 000 0001", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != (MarkedRead{Chat: "110000000001@lid", Read: true}) {
		t.Errorf("read = %+v", got)
	}
	if len(wire.readOf) != 1 || wire.readOf[0].JID.String() != "110000000001@lid" ||
		wire.readOf[0].Last.GetID() != "D-NEW" || !wire.reads[0] {
		t.Errorf("wire = %+v %v", wire.readOf, wire.reads)
	}
	if n := unreadOf(t, w, 2); n != 0 {
		t.Errorf("unread after read = %d, want 0", n)
	}
	if _, err := MarkRead(ctx, root, "120363000000000001@g.us", true); err != nil {
		t.Fatal(err)
	}
	if n := unreadOf(t, w, 1); n != 0 {
		t.Errorf("group unread after read = %d, want 0", n)
	}

	got, err = MarkRead(ctx, root, "120363000000000001@g.us", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != (MarkedRead{Chat: "120363000000000001@g.us", Read: false}) || wire.reads[2] ||
		wire.readOf[2].Last.GetParticipant() != "110000000009@lid" {
		t.Errorf("unread = %+v, wire %+v %v", got, wire.readOf[2], wire.reads)
	}
	if n := unreadOf(t, w, 1); n != 1 {
		t.Errorf("group unread after mark unread = %d, want 1", n)
	}
	if len(wire.archived) != 0 {
		t.Errorf("archive wire = %+v, want nothing", wire.archived)
	}
	if v := archivedOf(t, w, 2); v.Int64 != 0 {
		t.Errorf("ZARCHIVED = %v, want 0", v)
	}
}

// A request that can't be carried out puts nothing on the wire.
func TestMarkReadRejects(t *testing.T) {
	_, root, wire := archiveFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		to   string
		read bool
		want string
	}{
		{"Sarah", true, "invalid chat"},
		{"status@broadcast", true, "is not a chat"},
		{"+971509999999", true, "cannot mark read: chat 971509999999@s.whatsapp.net is not in the database"},
		{"+971509999999", false, "cannot mark unread: chat"},
	} {
		if _, err := MarkRead(ctx, root, c.to, c.read); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("MarkRead(%q, %v): err = %v, want %q", c.to, c.read, err, c.want)
		}
	}

	wire.readErr = errors.New("websocket not connected")
	if _, err := MarkRead(ctx, root, "120363000000000001@g.us", true); err == nil ||
		!strings.Contains(err.Error(), "mark read failed: websocket not connected") {
		t.Errorf("wire failure: err = %v", err)
	}
	if len(wire.readOf) != 0 {
		t.Errorf("wire = %+v, want nothing", wire.readOf)
	}
}

// A change WhatsApp took but the database didn't is reported, not
// failed: it must not be repeated.
func TestMarkReadRecordFailureWarns(t *testing.T) {
	w := archiveWriter(t)
	root := filepath.Dir(w.dbPath)
	if _, err := workspace.Init(root); err != nil {
		t.Fatal(err)
	}
	wire := &fakeWire{}
	ops := w.chatOps(wire.archive, wire.read)
	ops.recordRead = func(context.Context, archiveTarget, bool) error { return errors.New("database is locked") }
	stop, err := startSend(root, wire.deliver, nil, nil, nil, func(*events.Message, int64) error { return nil }, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	got, err := MarkRead(context.Background(), root, "120363000000000001@g.us", true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Read || !strings.Contains(got.Warning, "database is locked") || len(wire.readOf) != 1 {
		t.Errorf("read = %+v, wire = %d", got, len(wire.readOf))
	}
}

func TestMarkReadLiveNotRunning(t *testing.T) {
	if _, err := MarkRead(context.Background(), initWorkspace(t), "+971500000001", true); !errors.Is(err, ErrNotRunning) {
		t.Errorf("err = %v, want ErrNotRunning", err)
	}
}
