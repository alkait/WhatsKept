package live

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatskept/internal/backup"
)

// parsingClient is a whatsmeow client that is never connected: enough
// for ParseWebMessage, which only needs to know the account's own JID.
func parsingClient(t *testing.T) *whatsmeow.Client {
	t.Helper()
	ctx := context.Background()
	c, err := sqlstore.New(ctx, "sqlite3",
		"file:"+filepath.Join(t.TempDir(), "session.db")+"?_foreign_keys=on", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	dev := c.NewDevice()
	own := types.NewJID("971500000099", types.DefaultUserServer)
	dev.ID = &own
	return whatsmeow.NewClient(dev, nil)
}

// historyWriter is a writer over an empty ChatStorage — the real schema,
// views included — wired to the hints the way live wires it.
func historyWriter(t *testing.T, hints *historyHints) *Writer {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, backup.ChatStorageName)
	if err := backup.CreateEmpty(dbPath); err != nil {
		t.Fatal(err)
	}
	w := &Writer{
		dbPath:    dbPath,
		root:      root,
		knownName: hints.name,
		resolvePN: func(_ context.Context, lid types.JID) (types.JID, bool) { return hints.phoneFor(lid) },
		groupSubject: func(context.Context, types.JID) (string, bool) {
			t.Error("groupSubject called: the chunk's conversation name should have been used")
			return "", false
		},
	}
	if err := w.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	return w
}

func webText(chat, id string, fromMe bool, participant string, ts time.Time, text string) *waHistorySync.HistorySyncMsg {
	key := &waCommon.MessageKey{RemoteJID: proto.String(chat), FromMe: proto.Bool(fromMe), ID: proto.String(id)}
	if participant != "" {
		key.Participant = proto.String(participant)
	}
	return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
		Key:              key,
		Message:          &waE2E.Message{Conversation: proto.String(text)},
		MessageTimestamp: proto.Uint64(uint64(ts.Unix())),
	}}
}

func applyDirect(w *Writer) func(*events.Message) (bool, error) {
	return func(evt *events.Message) (bool, error) {
		res, err := w.Apply(context.Background(), decide(evt, nil))
		return res.Action == actionInsert, err
	}
}

// The case that matters most: a database already holding messages (as a
// backup import leaves it). The chunk's copies of those are skipped;
// everything else lands in the right chat, through the ordinary path.
func TestStoreHistoryOverExistingMessages(t *testing.T) {
	ctx := context.Background()
	hints := newHistoryHints()
	w := historyWriter(t, hints)
	client := parsingClient(t)

	// Already stored: a DM message, and a message from a second contact
	// whose chat is keyed by phone JID.
	if _, err := w.Apply(ctx, decide(dmEvent("S1", "hello"), nil)); err != nil {
		t.Fatal(err)
	}
	other := dmEvent("P1", "from lina")
	other.Info.Chat = types.NewJID("971500000002", types.DefaultUserServer)
	other.Info.Sender = other.Info.Chat
	other.Info.PushName = "Lina"
	linaChat, err := w.Apply(ctx, decide(other, nil))
	if err != nil {
		t.Fatal(err)
	}

	ts := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC) // dmEvent's timestamp
	dm, lid, group := "971500000001@s.whatsapp.net", "220000000002@lid", "120363000000000001@g.us"
	data := &waHistorySync.HistorySync{
		SyncType:   waHistorySync.HistorySync_RECENT.Enum(),
		ChunkOrder: proto.Uint32(1),
		Progress:   proto.Uint32(40),
		PhoneNumberToLidMappings: []*waHistorySync.PhoneNumberToLIDMapping{{
			PnJID: proto.String("971500000002@s.whatsapp.net"), LidJID: proto.String(lid),
		}},
		Conversations: []*waHistorySync.Conversation{
			{ID: proto.String(dm), Messages: []*waHistorySync.HistorySyncMsg{
				webText(dm, "S0", false, "", ts.Add(-time.Hour), "earlier"),
				webText(dm, "S1", false, "", ts, "hello"), // already present
				webText(dm, "S-OUT", true, "", ts.Add(-30*time.Minute), "my reply"),
			}},
			// The same contact as P1, keyed by LID: must join P1's chat.
			{ID: proto.String(lid), Messages: []*waHistorySync.HistorySyncMsg{
				webText(lid, "L1", false, "", ts.Add(-2*time.Hour), "via lid"),
			}},
			{ID: proto.String(group), Name: proto.String("Family"), Messages: []*waHistorySync.HistorySyncMsg{
				webText(group, "G1", false, "220000000003@lid", ts.Add(-3*time.Hour), "group hi"),
			}},
			{ID: proto.String("status@broadcast"), Messages: []*waHistorySync.HistorySyncMsg{
				webText("status@broadcast", "ST1", false, "971500000001@s.whatsapp.net", ts, "story"),
			}},
		},
	}

	st := storeHistory(ctx, w, hints, data, client.ParseWebMessage, applyDirect(w))
	want := historyStats{Messages: 6, Inserted: 4, Skipped: 2}
	if st != want {
		t.Errorf("stats = %+v, want %+v", st, want)
	}

	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := queryDB(t, w, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM ZWAMESSAGE WHERE ZSTANZAID = 'S1'`); n != 1 {
		t.Errorf("S1 rows = %d, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM ZWAMESSAGE WHERE ZSTANZAID = 'ST1'`); n != 0 {
		t.Errorf("status message stored")
	}
	// LID-keyed history joined the phone-keyed chat instead of forking.
	if n := count(`SELECT COUNT(*) FROM ZWAMESSAGE WHERE ZSTANZAID = 'L1' AND ZCHATSESSION = ?`, linaChat.ChatPK); n != 1 {
		t.Errorf("L1 not in Lina's existing chat %d", linaChat.ChatPK)
	}
	if n := count(`SELECT COUNT(*) FROM ZWACHATSESSION`); n != 3 {
		t.Errorf("chat sessions = %d, want 3 (dm, lina, group)", n)
	}
	// Older history doesn't become the chat's last message.
	var last string
	if err := queryDB(t, w, `SELECT m.ZSTANZAID FROM ZWACHATSESSION c JOIN ZWAMESSAGE m ON m.Z_PK = c.ZLASTMESSAGE
		WHERE c.ZCONTACTJID = ?`, dm).Scan(&last); err != nil || last != "S1" {
		t.Errorf("last message = %q, %v; want S1", last, err)
	}
	// The group is titled from the chunk, and the outgoing message is ours.
	var title string
	if err := queryDB(t, w, `SELECT c.title FROM v_messages m JOIN v_chats c ON c.chat_id = m.chat_id
		WHERE m.stanza_id = 'G1'`).Scan(&title); err != nil || title != "Family" {
		t.Errorf("group title = %q, %v", title, err)
	}
	if n := count(`SELECT COUNT(*) FROM ZWAMESSAGE WHERE ZSTANZAID = 'S-OUT' AND ZISFROMME = 1 AND ZTOJID = ?`, dm); n != 1 {
		t.Errorf("outgoing history message not stored as ours")
	}
	// Searchable like any capture.
	if n := count(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'earlier'`); n != 1 {
		t.Errorf("fts matches for history text = %d", n)
	}

	// Replaying the chunk — a re-pair re-sends history — adds nothing.
	again := storeHistory(ctx, w, hints, data, client.ParseWebMessage, applyDirect(w))
	if again.Inserted != 0 || again.Skipped != 6 {
		t.Errorf("replay stats = %+v", again)
	}
}

func TestStoreHistoryPushNames(t *testing.T) {
	ctx := context.Background()
	hints := newHistoryHints()
	w := historyWriter(t, hints)
	client := parsingClient(t)

	group := "120363000000000001@g.us"
	msgs := &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_RECENT.Enum(),
		Conversations: []*waHistorySync.Conversation{{ID: proto.String(group), Name: proto.String("Family"),
			Messages: []*waHistorySync.HistorySyncMsg{
				webText(group, "G1", false, "220000000003@lid", time.Now(), "hi all"),
			}}},
	}
	storeHistory(ctx, w, hints, msgs, client.ParseWebMessage, applyDirect(w))

	names := &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_PUSH_NAME.Enum(),
		Pushnames: []*waHistorySync.Pushname{
			{ID: proto.String("220000000003@lid"), Pushname: proto.String("Omar")},
			{ID: proto.String("not a jid@@"), Pushname: proto.String("x")},
		},
	}
	st := storeHistory(ctx, w, hints, names, client.ParseWebMessage, applyDirect(w))
	if st.PushNames != 2 || st.PushNamesStored != 1 {
		t.Errorf("stats = %+v", st)
	}
	var sender string
	if err := queryDB(t, w, `SELECT sender_name FROM v_messages WHERE stanza_id = 'G1'`).Scan(&sender); err != nil || sender != "Omar" {
		t.Errorf("sender_name = %q, %v", sender, err)
	}

	// A changed name updates the row; an unchanged one is left alone.
	names.Pushnames = []*waHistorySync.Pushname{{ID: proto.String("220000000003@lid"), Pushname: proto.String("Omar K")}}
	if n, err := w.PutPushNames(ctx, names.Pushnames); err != nil || n != 1 {
		t.Errorf("update: n=%d err=%v", n, err)
	}
	if n, err := w.PutPushNames(ctx, names.Pushnames); err != nil || n != 0 {
		t.Errorf("no-op: n=%d err=%v", n, err)
	}
	var rows int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM ZWAPROFILEPUSHNAME`).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("push name rows = %d, %v", rows, err)
	}
}

func TestHistoryHintsFromConversation(t *testing.T) {
	h := newHistoryHints()
	h.absorb(&waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{
		ID:          proto.String("220000000005@lid"),
		PnJID:       proto.String("971500000005@s.whatsapp.net"),
		LidJID:      proto.String("220000000005@lid"),
		DisplayName: proto.String("Noor"),
	}}})
	lid := types.NewJID("220000000005", types.HiddenUserServer)
	if pn, ok := h.phoneFor(lid); !ok || pn.String() != "971500000005@s.whatsapp.net" {
		t.Errorf("phoneFor = %v, %v", pn, ok)
	}
	for _, j := range []types.JID{lid, types.NewJID("971500000005", types.DefaultUserServer)} {
		if n, ok := h.name(j); !ok || n != "Noor" {
			t.Errorf("name(%s) = %q, %v", j, n, ok)
		}
	}
}

func TestRecordHistoryChunk(t *testing.T) {
	ctx := context.Background()
	w := historyWriter(t, newHistoryHints())
	t0 := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	type row struct {
		started, last       string
		progress, ins, done int
	}
	read := func() row {
		t.Helper()
		var r row
		if err := queryDB(t, w, `SELECT started_at, last_chunk_at, progress, inserted, done FROM wa_history_sync`).
			Scan(&r.started, &r.last, &r.progress, &r.ins, &r.done); err != nil {
			t.Fatal(err)
		}
		return r
	}
	steps := []struct {
		kind     waHistorySync.HistorySync_HistorySyncType
		progress uint32
		inserted int
		want     row
	}{
		{waHistorySync.HistorySync_INITIAL_BOOTSTRAP, 5, 100, row{"2026-11-21 10:00:00", "2026-11-21 10:00:00", 5, 100, 0}},
		{waHistorySync.HistorySync_PUSH_NAME, 0, 0, row{"2026-11-21 10:00:00", "2026-11-21 10:01:00", 5, 100, 0}},
		{waHistorySync.HistorySync_RECENT, 60, 50, row{"2026-11-21 10:00:00", "2026-11-21 10:02:00", 60, 150, 0}},
		{waHistorySync.HistorySync_FULL, 100, 7, row{"2026-11-21 10:00:00", "2026-11-21 10:03:00", 100, 157, 1}},
		// A re-pair starts a new sync.
		{waHistorySync.HistorySync_INITIAL_BOOTSTRAP, 3, 2, row{"2026-11-21 10:04:00", "2026-11-21 10:04:00", 3, 2, 0}},
	}
	for i, s := range steps {
		if err := w.RecordHistoryChunk(ctx, s.kind, s.progress, s.inserted, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if got := read(); got != s.want {
			t.Errorf("step %d (%s): got %+v, want %+v", i, s.kind, got, s.want)
		}
	}
}
