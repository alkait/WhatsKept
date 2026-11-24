package live

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"go.mau.fi/whatsmeow/appstate"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"whatskept/internal/workspace"
)

// fakeWire stands in for the WhatsApp connection: it remembers what it
// was asked to send and hands out stanza IDs OUT1, OUT2…
type fakeWire struct {
	err  error
	sent []string // "<jid> <text>"
	msgs []*waE2E.Message

	// fetch stand-in: the attachment's current path and plaintext hash,
	// or fetchErr.
	fetchErr error
	fetched  []string // stanza IDs

	// archive stand-in: the changes it was asked to make, or archiveErr.
	archiveErr error
	archived   []archiveTarget
	archives   []bool

	// read stand-in: the read changes it was asked to make, or readErr.
	readErr error
	readOf  []archiveTarget
	reads   []bool

	// list stand-in: the patches it was asked to write, or listErr.
	listErr error
	lists   []appstate.PatchInfo
}

// listOps is the endpoint's list operations over w, with this wire
// standing in for WhatsApp: lists are planned from w's replica.
func (f *fakeWire) listOps(w *Writer) *listOps {
	return &listOps{
		state:  w.replicaState,
		target: w.archiveTarget,
		forms:  w.chatForms,
		write: func(_ context.Context, p appstate.PatchInfo) error {
			if f.listErr != nil {
				return f.listErr
			}
			f.lists = append(f.lists, p)
			return nil
		},
		record: w.recordList,
	}
}

func (f *fakeWire) read(_ context.Context, t archiveTarget, read bool) error {
	if f.readErr != nil {
		return f.readErr
	}
	f.readOf = append(f.readOf, t)
	f.reads = append(f.reads, read)
	return nil
}

func (f *fakeWire) archive(_ context.Context, t archiveTarget, archive bool) error {
	if f.archiveErr != nil {
		return f.archiveErr
	}
	f.archived = append(f.archived, t)
	f.archives = append(f.archives, archive)
	return nil
}

func (f *fakeWire) fetch(_ context.Context, src forwardSource) (string, []byte, error) {
	f.fetched = append(f.fetched, src.Stanza)
	if f.fetchErr != nil {
		return "", nil, f.fetchErr
	}
	return "/v/fresh-path", []byte("0123456789abcdef0123456789abcdef"), nil
}

func (f *fakeWire) deliver(_ context.Context, to types.JID, msg *waE2E.Message) (types.MessageID, time.Time, error) {
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	text, _ := textOf(msg)
	f.sent = append(f.sent, to.String()+" "+text)
	f.msgs = append(f.msgs, msg)
	id := types.MessageID("OUT" + string(rune('0'+len(f.sent))))
	return id, time.Date(2026, 8, 27, 10, 0, len(f.sent), 0, time.UTC), nil
}

// sendFixture starts the real send endpoint over a test workspace, with
// the fake wire for delivery and the real writer for recording.
func sendFixture(t *testing.T) (*Writer, string, *fakeWire) {
	t.Helper()
	w, root := newTestWriter(t)
	if _, err := workspace.Init(root); err != nil {
		t.Fatal(err)
	}
	wire := &fakeWire{}
	stop, err := startSend(root, wire.deliver, w.quote, w.forwardSource, wire.fetch,
		func(v *events.Message, forwardOf int64) error {
			_, err := w.Apply(context.Background(), forwardPlan(decide(v, nil), forwardOf))
			return err
		},
		w.chatOps(wire.archive, wire.read, wire.listOps(w)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return w, root, wire
}

func TestParseChat(t *testing.T) {
	for in, want := range map[string]string{
		"+971 50 000 0001":               "971500000001@s.whatsapp.net",
		"971500000001":                   "971500000001@s.whatsapp.net",
		"+1 (415) 555-0100":              "14155550100@s.whatsapp.net",
		"971500000001@s.whatsapp.net":    "971500000001@s.whatsapp.net",
		"971500000001:13@s.whatsapp.net": "971500000001@s.whatsapp.net",
		"120363000000000001@g.us":        "120363000000000001@g.us",
		"110000000001@lid":               "110000000001@lid",
	} {
		got, err := ParseChat(in)
		if err != nil || got.String() != want {
			t.Errorf("ParseChat(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "Sarah", "12345", "+97150abc0001", "status@broadcast", "123@newsletter", "@g.us"} {
		if got, err := ParseChat(in); err == nil {
			t.Errorf("ParseChat(%q) = %q, want an error", in, got)
		}
	}
}

func TestSendDeliversAndRecords(t *testing.T) {
	w, root, wire := sendFixture(t)

	sent, err := Send(context.Background(), root, "+971 50 000 0001", "hello from send", "")
	if err != nil {
		t.Fatal(err)
	}
	if sent.ID != "OUT1" || sent.Chat != "971500000001@s.whatsapp.net" ||
		sent.Ts != "2026-08-27T10:00:01Z" || sent.Warning != "" {
		t.Errorf("sent = %+v", sent)
	}
	if len(wire.sent) != 1 || wire.sent[0] != "971500000001@s.whatsapp.net hello from send" {
		t.Errorf("wire = %q", wire.sent)
	}

	// Recorded the way the phone stores an outgoing message.
	var text, to string
	var from sql.NullString
	var fromMe int
	var chatPK int64
	if err := queryDB(t, w, `SELECT ZTEXT, ZTOJID, ZFROMJID, ZISFROMME, ZCHATSESSION
		FROM ZWAMESSAGE WHERE ZSTANZAID = 'OUT1'`).Scan(&text, &to, &from, &fromMe, &chatPK); err != nil {
		t.Fatal(err)
	}
	if text != "hello from send" || to != "971500000001@s.whatsapp.net" || from.Valid || fromMe != 1 {
		t.Errorf("row: text=%q to=%q from=%v fromMe=%d", text, to, from, fromMe)
	}
	var hits int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'hello'`).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("fts hits = %d, want 1", hits)
	}

	// A second send lands in the same chat; a reply arriving from the
	// partner joins it too rather than forking a thread.
	if _, err := Send(context.Background(), root, "971500000001@s.whatsapp.net", "and again", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(context.Background(), decide(dmEvent("IN1", "got it"), nil)); err != nil {
		t.Fatal(err)
	}
	var chats, inChat int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM ZWACHATSESSION`).Scan(&chats); err != nil {
		t.Fatal(err)
	}
	if err := queryDB(t, w, `SELECT COUNT(*) FROM ZWAMESSAGE WHERE ZCHATSESSION = ?`, chatPK).Scan(&inChat); err != nil {
		t.Fatal(err)
	}
	if chats != 1 || inChat != 3 {
		t.Errorf("chats=%d messages in chat=%d, want 1 and 3", chats, inChat)
	}
}

func TestSendToGroup(t *testing.T) {
	w, root, _ := sendFixture(t)
	if _, err := Send(context.Background(), root, "120363000000000001@g.us", "hi all", ""); err != nil {
		t.Fatal(err)
	}
	var sessionType int
	var member sql.NullInt64
	if err := queryDB(t, w, `SELECT cs.ZSESSIONTYPE, m.ZGROUPMEMBER FROM ZWAMESSAGE m
		JOIN ZWACHATSESSION cs ON cs.Z_PK = m.ZCHATSESSION
		WHERE m.ZTOJID = '120363000000000001@g.us'`).Scan(&sessionType, &member); err != nil {
		t.Fatal(err)
	}
	if sessionType != 1 || member.Valid {
		t.Errorf("sessionType=%d member=%v, want a group session and no member", sessionType, member)
	}
}

// A reply quotes its target on the wire and is recorded with the
// parent link, whoever wrote the target.
func TestSendReply(t *testing.T) {
	w, root, wire := sendFixture(t)
	w.self = func() types.JID { return types.NewJID("971509999999", types.DefaultUserServer) }
	in, err := w.Apply(context.Background(), decide(dmEvent("IN1", "lunch tomorrow?"), nil))
	if err != nil {
		t.Fatal(err)
	}

	sent, err := Send(context.Background(), root, "+971500000001", "yes, 1pm", "IN1")
	if err != nil {
		t.Fatal(err)
	}
	ci := wire.msgs[0].GetExtendedTextMessage().GetContextInfo()
	if wire.sent[0] != "971500000001@s.whatsapp.net yes, 1pm" || ci.GetStanzaID() != "IN1" ||
		ci.GetParticipant() != "971500000001@s.whatsapp.net" ||
		ci.GetQuotedMessage().GetConversation() != "lunch tomorrow?" {
		t.Errorf("wire = %q, context = %v", wire.sent, ci)
	}
	var parent sql.NullInt64
	if err := queryDB(t, w, `SELECT ZPARENTMESSAGE FROM ZWAMESSAGE WHERE ZSTANZAID = ?`, sent.ID).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if parent.Int64 != in.PK {
		t.Errorf("ZPARENTMESSAGE = %v, want %d", parent, in.PK)
	}

	// Quoting one of our own messages names us as its author.
	if _, err := Send(context.Background(), root, "+971500000001", "make it 2pm", sent.ID); err != nil {
		t.Fatal(err)
	}
	ci = wire.msgs[1].GetExtendedTextMessage().GetContextInfo()
	if ci.GetParticipant() != "971509999999@s.whatsapp.net" || ci.GetQuotedMessage().GetConversation() != "yes, 1pm" {
		t.Errorf("own-message context = %v", ci)
	}
}

// An attachment has no text of its own: the quote shows its caption,
// or failing that its filename.
func TestSendReplyQuotesMedia(t *testing.T) {
	w, root, wire := sendFixture(t)
	if _, err := w.Apply(context.Background(), decide(dmEvent("IN1", "hello"), nil)); err != nil {
		t.Fatal(err)
	}
	db, err := openRW(w.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		INSERT INTO ZWAMESSAGE (Z_PK, ZCHATSESSION, ZISFROMME, ZMESSAGETYPE, ZFROMJID, ZSTANZAID) VALUES
			(50, 1, 0, 1, '971500000001@s.whatsapp.net', 'IMG'),
			(51, 1, 0, 8, '971500000001@s.whatsapp.net', 'PDF');
		INSERT INTO ZWAMEDIAITEM (Z_PK, ZMESSAGE, ZAUTHORNAME, ZTITLE) VALUES
			(1, 50, NULL, 'the receipt'), (2, 51, 'scan.pdf', NULL);`); err != nil {
		t.Fatal(err)
	}
	for i, c := range []struct{ target, want string }{{"IMG", "the receipt"}, {"PDF", "scan.pdf"}} {
		if _, err := Send(context.Background(), root, "+971500000001", "got it", c.target); err != nil {
			t.Fatal(err)
		}
		got := wire.msgs[i].GetExtendedTextMessage().GetContextInfo().GetQuotedMessage().GetConversation()
		if got != c.want {
			t.Errorf("quote of %s = %q, want %q", c.target, got, c.want)
		}
	}
}

func TestSendReplyRejectsBadTarget(t *testing.T) {
	w, root, wire := sendFixture(t)
	for _, v := range []*events.Message{dmEvent("IN1", "hello"), dmEvent("GONE", "oops")} {
		if _, err := w.Apply(context.Background(), decide(v, nil)); err != nil {
			t.Fatal(err)
		}
	}
	db, err := openRW(w.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE ZWAMESSAGE SET ZMESSAGETYPE = 14, ZTEXT = NULL WHERE ZSTANZAID = 'GONE'`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ to, target, want string }{
		{"+971500000001", "NOPE", "no message with stanza ID"},
		{"+971500000002", "IN1", "is not in chat 971500000002@s.whatsapp.net"},
		{"120363000000000001@g.us", "IN1", "is not in chat"},
		{"+971500000001", "GONE", "was deleted"},
	} {
		_, err := Send(context.Background(), root, c.to, "hi", c.target)
		if err == nil || !strings.Contains(err.Error(), "cannot reply") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Send(%q, reply %q): err = %v, want %q", c.to, c.target, err, c.want)
		}
	}
	var rows int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM ZWAMESSAGE`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if len(wire.sent) != 0 || rows != 2 {
		t.Errorf("wire=%q rows=%d, want nothing sent or stored", wire.sent, rows)
	}
}

func TestSendRejectsBadInput(t *testing.T) {
	w, root, wire := sendFixture(t)
	for _, c := range []struct{ to, text, want string }{
		{"Sarah", "hi", "invalid chat"},
		{"status@broadcast", "hi", "is not a chat"},
		{"+971500000001", "  ", "empty message"},
	} {
		_, err := Send(context.Background(), root, c.to, c.text, "")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Send(%q, %q): err = %v, want %q", c.to, c.text, err, c.want)
		}
	}
	var rows int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM ZWAMESSAGE`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if len(wire.sent) != 0 || rows != 0 {
		t.Errorf("wire=%q rows=%d, want nothing sent or stored", wire.sent, rows)
	}
}

func TestSendDeliveryFailure(t *testing.T) {
	w, root, wire := sendFixture(t)
	wire.err = errors.New("websocket not connected")
	_, err := Send(context.Background(), root, "+971500000001", "hi", "")
	if err == nil || !strings.Contains(err.Error(), "websocket not connected") {
		t.Fatalf("err = %v", err)
	}
	var rows int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM ZWAMESSAGE`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("rows = %d, want none for a message that never went out", rows)
	}
}

// A message that went out but could not be stored is still a success —
// retrying would send it twice — flagged with a warning.
func TestSendRecordFailureWarns(t *testing.T) {
	root := initWorkspace(t)
	wire := &fakeWire{}
	stop, err := startSend(root, wire.deliver, nil, nil, nil, func(*events.Message, int64) error {
		return errors.New("database is locked")
	}, chatOps{})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	sent, err := Send(context.Background(), root, "+971500000001", "hi", "")
	if err != nil {
		t.Fatal(err)
	}
	if sent.ID != "OUT1" || !strings.Contains(sent.Warning, "database is locked") {
		t.Errorf("sent = %+v", sent)
	}
}

func TestSendWrongToken(t *testing.T) {
	_, root, wire := sendFixture(t)
	path := workspace.LiveEndpointPath(root)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ep endpoint
	if err := json.Unmarshal(data, &ep); err != nil {
		t.Fatal(err)
	}
	if len(ep.Token) != 32 {
		t.Errorf("token = %q, want 32 hex chars", ep.Token)
	}
	ep.Token = strings.Repeat("0", 32)
	data, _ = json.Marshal(ep)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Send(context.Background(), root, "+971500000001", "hi", ""); err == nil {
		t.Error("send with the wrong token succeeded")
	}
	if len(wire.sent) != 0 {
		t.Errorf("wire = %q, want nothing sent", wire.sent)
	}
}

func TestSendEndpointLifecycle(t *testing.T) {
	root := initWorkspace(t)
	path := workspace.LiveEndpointPath(root)

	// Never started.
	if _, err := Send(context.Background(), root, "+971500000001", "hi", ""); !errors.Is(err, ErrNotRunning) {
		t.Errorf("no live: err = %v, want ErrNotRunning", err)
	}

	wire := &fakeWire{}
	stop, err := startSend(root, wire.deliver, nil, nil, nil, func(*events.Message, int64) error { return nil }, chatOps{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("live.json mode = %v, want 0600 (it holds the token)", info.Mode().Perm())
	}
	stale, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Stopped cleanly: the advertisement goes with it.
	stop()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("live.json still present after stop: %v", err)
	}

	// Crashed: the file stays behind, pointing at a dead port.
	if err := os.WriteFile(path, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Send(context.Background(), root, "+971500000001", "hi", ""); !errors.Is(err, ErrNotRunning) {
		t.Errorf("stale live.json: err = %v, want ErrNotRunning", err)
	}
}
