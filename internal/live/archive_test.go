package live

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"whatskept/internal/workspace"
)

// archiveWriter is a test writer holding chats the way real workspaces
// store them: a group, a DM keyed on the phone JID, one keyed on the
// phone JID with its LID beside it, one keyed on the LID alone, and a
// DM whose LID nobody knows. whatsmeow's LID map knows the first and
// third DMs' pairs.
func archiveWriter(t *testing.T) *Writer {
	t.Helper()
	w, _ := newTestWriter(t)
	db, err := openRW(w.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Each chat's latest message, which an archive change carries, is
	// the later of two; chat 5 has none.
	if _, err := db.Exec(`INSERT INTO ZWACHATSESSION (Z_PK, ZSESSIONTYPE, ZCONTACTJID, ZCONTACTIDENTIFIER, ZARCHIVED) VALUES
		(1, 1, '120363000000000001@g.us', NULL, 0),
		(2, 0, '971500000001@s.whatsapp.net', NULL, 0),
		(3, 0, '971500000002@s.whatsapp.net', '110000000002@lid', 0),
		(4, 0, '110000000003@lid', NULL, NULL),
		(5, 0, '971500000005@s.whatsapp.net', NULL, 0);
		INSERT INTO ZWAGROUPMEMBER (Z_PK, ZCHATSESSION, ZMEMBERJID) VALUES (1, 1, '110000000009@lid');
		INSERT INTO ZWAMESSAGE (Z_PK, ZCHATSESSION, ZGROUPMEMBER, ZISFROMME, ZMESSAGEDATE, ZSTANZAID) VALUES
		(1, 1, NULL, 1, 800000000, 'G-OLD'),
		(2, 1, 1,    0, 800000100.5, 'G-NEW'),
		(3, 2, NULL, 0, 800000000, 'D-OLD'),
		(4, 2, NULL, 1, 800000200, 'D-NEW'),
		(5, 3, NULL, 0, 800000300, 'L-NEW'),
		(6, 4, NULL, 0, 800000400, 'K-NEW')`); err != nil {
		t.Fatal(err)
	}
	pair := map[string]string{"110000000001@lid": "971500000001@s.whatsapp.net",
		"110000000003@lid": "971500000003@s.whatsapp.net"}
	w.resolvePN = func(_ context.Context, lid types.JID) (types.JID, bool) {
		pn, ok := pair[lid.String()]
		jid, _ := types.ParseJID(pn)
		return jid, ok
	}
	w.resolveLID = func(_ context.Context, pn types.JID) (types.JID, bool) {
		for lid, p := range pair {
			if p == pn.String() {
				jid, _ := types.ParseJID(lid)
				return jid, true
			}
		}
		return types.JID{}, false
	}
	return w
}

func archivedOf(t *testing.T, w *Writer, pk int64) sql.NullInt64 {
	t.Helper()
	var v sql.NullInt64
	if err := queryDB(t, w, `SELECT ZARCHIVED FROM ZWACHATSESSION WHERE Z_PK = ?`, pk).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// An archive change names the chat by whichever JID WhatsApp holds it
// under; each lands on the one session it means.
func TestSetArchivedFindsTheChat(t *testing.T) {
	w := archiveWriter(t)
	ctx := context.Background()
	for chat, wantPK := range map[string]int64{
		"120363000000000001@g.us":     1,
		"971500000001@s.whatsapp.net": 2,
		"110000000001@lid":            2, // LID, mapped to the phone JID
		"110000000002@lid":            3, // LID, stored beside the phone JID
		"971500000003@s.whatsapp.net": 4, // phone JID, mapped to the LID the chat is keyed on
	} {
		jid, err := types.ParseJID(chat)
		if err != nil {
			t.Fatal(err)
		}
		pk, ok, err := w.SetArchived(ctx, jid, true)
		if err != nil || !ok || pk != wantPK {
			t.Errorf("%s: pk=%d ok=%v err=%v, want pk %d", chat, pk, ok, err, wantPK)
			continue
		}
		if v := archivedOf(t, w, wantPK); v.Int64 != 1 {
			t.Errorf("%s: ZARCHIVED = %v, want 1", chat, v)
		}
		if _, _, err := w.SetArchived(ctx, jid, false); err != nil {
			t.Fatal(err)
		}
		if v := archivedOf(t, w, wantPK); !v.Valid || v.Int64 != 0 {
			t.Errorf("%s: after unarchive ZARCHIVED = %v, want 0", chat, v)
		}
	}
}

// Archiving never creates a chat: one we don't hold is reported, and
// nothing changes.
func TestSetArchivedUnknownChat(t *testing.T) {
	w := archiveWriter(t)
	pk, ok, err := w.SetArchived(context.Background(), types.NewJID("971509999999", types.DefaultUserServer), true)
	if err != nil || ok || pk != 0 {
		t.Errorf("pk=%d ok=%v err=%v, want not found", pk, ok, err)
	}
	var chats, archived int
	if err := queryDB(t, w, `SELECT COUNT(*), COUNT(*) FILTER (WHERE ZARCHIVED = 1) FROM ZWACHATSESSION`).
		Scan(&chats, &archived); err != nil {
		t.Fatal(err)
	}
	if chats != 5 || archived != 0 {
		t.Errorf("chats=%d archived=%d, want 5 and 0", chats, archived)
	}
}

// An archive change addresses a DM by its LID wherever one is known,
// and carries the chat's latest message the way WhatsApp's clients do.
func TestArchiveTarget(t *testing.T) {
	w := archiveWriter(t)
	ts := func(cocoa int64) time.Time { return time.Unix(cocoa+cocoaEpoch, 0) }
	for _, c := range []struct {
		to, jid, last, participant string
		fromMe                     bool
		ts                         time.Time
		pk                         int64
	}{
		{"120363000000000001@g.us", "120363000000000001@g.us", "G-NEW", "110000000009@lid", false, ts(800000100), 1},
		{"971500000001@s.whatsapp.net", "110000000001@lid", "D-NEW", "", true, ts(800000200), 2}, // LID from whatsmeow's map
		{"110000000001@lid", "110000000001@lid", "D-NEW", "", true, ts(800000200), 2},
		{"971500000002@s.whatsapp.net", "110000000002@lid", "L-NEW", "", false, ts(800000300), 3},     // LID stored beside the phone JID
		{"971500000003@s.whatsapp.net", "110000000003@lid", "K-NEW", "", false, ts(800000400), 4},     // chat keyed on the LID
		{"971500000005@s.whatsapp.net", "971500000005@s.whatsapp.net", "", "", false, time.Time{}, 5}, // no LID known, no messages
	} {
		to, _ := types.ParseJID(c.to)
		got, err := w.archiveTarget(context.Background(), to)
		if err != nil {
			t.Errorf("%s: %v", c.to, err)
			continue
		}
		if got.PK != c.pk || got.JID.String() != c.jid || !got.LastTs.Equal(c.ts) {
			t.Errorf("%s: pk=%d jid=%s ts=%v, want %d %s %v", c.to, got.PK, got.JID, got.LastTs, c.pk, c.jid, c.ts)
		}
		if c.last == "" {
			if got.Last != nil {
				t.Errorf("%s: last = %v, want none", c.to, got.Last)
			}
			continue
		}
		if got.Last == nil || got.Last.GetID() != c.last || got.Last.GetRemoteJID() != c.jid ||
			got.Last.GetFromMe() != c.fromMe || got.Last.GetParticipant() != c.participant {
			t.Errorf("%s: last = %v, want %s from_me=%v participant=%q", c.to, got.Last, c.last, c.fromMe, c.participant)
		}
	}

	_, err := w.archiveTarget(context.Background(), types.NewJID("971509999999", types.DefaultUserServer))
	if !errors.Is(err, errNoChat) {
		t.Errorf("unknown chat: err = %v, want errNoChat", err)
	}
}

// archiveFixture runs the real endpoint over archiveWriter's chats,
// with the fake wire standing in for WhatsApp.
func archiveFixture(t *testing.T) (*Writer, string, *fakeWire) {
	t.Helper()
	w := archiveWriter(t)
	root := filepath.Dir(w.dbPath)
	if _, err := workspace.Init(root); err != nil {
		t.Fatal(err)
	}
	wire := &fakeWire{}
	stop, err := startSend(root, wire.deliver, w.quote, w.forwardSource, wire.fetch,
		func(*events.Message, int64) error { return nil }, w.archiver(wire.archive))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return w, root, wire
}

func TestArchiveThroughLive(t *testing.T) {
	w, root, wire := archiveFixture(t)
	ctx := context.Background()

	got, err := Archive(ctx, root, "+971 50 000 0001", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Archived{Chat: "110000000001@lid", Archived: true}) {
		t.Errorf("archive = %+v", got)
	}
	if len(wire.archived) != 1 || wire.archived[0].JID.String() != "110000000001@lid" ||
		wire.archived[0].Last.GetID() != "D-NEW" || !wire.archives[0] {
		t.Errorf("wire = %+v %v", wire.archived, wire.archives)
	}
	if v := archivedOf(t, w, 2); v.Int64 != 1 {
		t.Errorf("ZARCHIVED = %v, want 1", v)
	}

	got, err = Archive(ctx, root, "110000000001@lid", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Archived{Chat: "110000000001@lid", Archived: false}) || wire.archives[1] {
		t.Errorf("unarchive = %+v, wire %v", got, wire.archives)
	}
	if v := archivedOf(t, w, 2); !v.Valid || v.Int64 != 0 {
		t.Errorf("ZARCHIVED = %v, want 0", v)
	}
}

// A request that can't be carried out changes nothing, on the wire or
// in the database.
func TestArchiveRejects(t *testing.T) {
	w, root, wire := archiveFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		to      string
		archive bool
		want    string
	}{
		{"Sarah", true, "invalid chat"},
		{"status@broadcast", true, "is not a chat"},
		{"+971509999999", true, "cannot archive: chat 971509999999@s.whatsapp.net is not in the database"},
		{"+971509999999", false, "cannot unarchive: chat"},
	} {
		if _, err := Archive(ctx, root, c.to, c.archive); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Archive(%q, %v): err = %v, want %q", c.to, c.archive, err, c.want)
		}
	}

	wire.archiveErr = errors.New("websocket not connected")
	if _, err := Archive(ctx, root, "120363000000000001@g.us", true); err == nil ||
		!strings.Contains(err.Error(), "archive failed: websocket not connected") {
		t.Errorf("wire failure: err = %v", err)
	}
	if len(wire.archived) != 0 {
		t.Errorf("wire = %+v, want nothing", wire.archived)
	}
	var archived int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM ZWACHATSESSION WHERE ZARCHIVED = 1`).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived != 0 {
		t.Errorf("archived chats = %d, want 0", archived)
	}
}

// A change WhatsApp took but the database didn't is still a success,
// flagged with a warning.
func TestArchiveRecordFailureWarns(t *testing.T) {
	w := archiveWriter(t)
	root := filepath.Dir(w.dbPath)
	if _, err := workspace.Init(root); err != nil {
		t.Fatal(err)
	}
	wire := &fakeWire{}
	arch := w.archiver(wire.archive)
	arch.record = func(context.Context, archiveTarget, bool) error { return errors.New("database is locked") }
	stop, err := startSend(root, wire.deliver, nil, nil, nil, func(*events.Message, int64) error { return nil }, arch)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	got, err := Archive(context.Background(), root, "120363000000000001@g.us", true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Archived || !strings.Contains(got.Warning, "database is locked") || len(wire.archived) != 1 {
		t.Errorf("archive = %+v, wire = %d", got, len(wire.archived))
	}
}

func TestArchiveLiveNotRunning(t *testing.T) {
	if _, err := Archive(context.Background(), initWorkspace(t), "+971500000001", true); !errors.Is(err, ErrNotRunning) {
		t.Errorf("err = %v, want ErrNotRunning", err)
	}
}
