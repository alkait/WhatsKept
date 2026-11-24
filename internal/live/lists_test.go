package live

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Records as WhatsApp holds them (shapes captured from a real account).
func labelEdit(id, name string, typ waSyncAction.LabelEditAction_ListType, active bool, order int32, deleted bool) appstate.Mutation {
	a := &waSyncAction.LabelEditAction{Name: proto.String(name), Color: proto.Int32(0), Deleted: proto.Bool(deleted),
		IsActive: proto.Bool(active), Type: typ.Enum()}
	if order >= 0 {
		a.OrderIndex = proto.Int32(order)
	}
	return appstate.Mutation{Operation: waServerSync.SyncdMutation_SET, Index: []string{"label_edit", id},
		Action: &waSyncAction.SyncActionValue{LabelEditAction: a}}
}

func labelJID(id, jid string, labeled bool) appstate.Mutation {
	return appstate.Mutation{Operation: waServerSync.SyncdMutation_SET, Index: []string{"label_jid", id, jid},
		Action: &waSyncAction.SyncActionValue{LabelAssociationAction: &waSyncAction.LabelAssociationAction{Labeled: proto.Bool(labeled)}}}
}

func labelOrder(ids ...int32) appstate.Mutation {
	return appstate.Mutation{Operation: waServerSync.SyncdMutation_SET, Index: []string{"label_reordering"},
		Action: &waSyncAction.SyncActionValue{LabelReorderingAction: &waSyncAction.LabelReorderingAction{SortedLabelIDs: ids}}}
}

func favorites(jids ...string) appstate.Mutation {
	var fs []*waSyncAction.FavoritesAction_Favorite
	for _, j := range jids {
		fs = append(fs, &waSyncAction.FavoritesAction_Favorite{ID: proto.String(j)})
	}
	return appstate.Mutation{Operation: waServerSync.SyncdMutation_SET, Index: []string{"favorites"},
		Action: &waSyncAction.SyncActionValue{FavoritesAction: &waSyncAction.FavoritesAction{Favorites: fs}}}
}

func remove(m appstate.Mutation) appstate.Mutation {
	m.Operation = waServerSync.SyncdMutation_REMOVE
	return m
}

// listRows runs a query and returns its rows as "a|b" lines. The test
// schema lacks the name tables v_chats reads; they are added empty.
func listRows(t *testing.T, w *Writer, query string) string {
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
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, v := range vals {
			switch x := v.(type) {
			case nil:
				parts = append(parts, "NULL")
			case []byte:
				parts = append(parts, string(x))
			default:
				parts = append(parts, fmt.Sprint(x))
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return strings.Join(out, "\n")
}

// accountLists is a real account's lists in miniature: the built-ins,
// two custom lists (one from before lists had an order) and a deleted
// one; members by phone JID (older) and by LID (newer), some removed;
// a record the phone moved to a newer key (REMOVE then SET); the order
// of the filters; and Favorites.
func accountLists() []appstate.Mutation {
	return []appstate.Mutation{
		labelEdit("1", "Unread", waSyncAction.LabelEditAction_UNREAD, true, 2, false),
		labelEdit("2", "Favorites", waSyncAction.LabelEditAction_FAVORITES, true, 3, false),
		labelEdit("3", "Groups", waSyncAction.LabelEditAction_GROUPS, true, 4, false),
		labelEdit("4", "Contractors", waSyncAction.LabelEditAction_CUSTOM, true, -1, false),
		labelEdit("5", "Communities", waSyncAction.LabelEditAction_COMMUNITY, false, 5, false),
		labelEdit("6", "🎗️", waSyncAction.LabelEditAction_CUSTOM, true, 0, false),
		labelEdit("7", "old list", waSyncAction.LabelEditAction_CUSTOM, true, 6, true),
		labelJID("4", "971500000001@s.whatsapp.net", true),  // chat 2, by phone
		labelJID("4", "110000000003@lid", true),             // chat 4, by LID
		labelJID("4", "971500000005@s.whatsapp.net", false), // removed
		labelJID("4", "110000000099@lid", true),             // not in the database
		labelJID("6", "120363000000000001@g.us", true),      // chat 1
		labelJID("6", "110000000001@lid", false),            // removed
		labelJID("7", "971500000002@s.whatsapp.net", true),  // in a deleted list
		remove(labelJID("6", "120363000000000001@g.us", true)),
		labelJID("6", "120363000000000001@g.us", true), // moved to a newer key
		labelOrder(6, 4, 1, 2, 3, 5),
		favorites("110000000001@lid", "120363000000000001@g.us"),
	}
}

func TestReplaceLists(t *testing.T) {
	ctx := context.Background()
	w := archiveWriter(t)
	lists, chats, err := w.ReplaceLists(ctx, listStateOf(accountLists()))
	if err != nil {
		t.Fatal(err)
	}
	if lists != 6 || chats != 6 {
		t.Fatalf("wrote %d lists, %d chats; want 6, 6", lists, chats)
	}
	got := listRows(t, w, `SELECT id, name, kind, shown, COALESCE(position, -1), chats FROM v_lists ORDER BY position`)
	want := strings.Join([]string{
		"6|🎗️|custom|1|0|1",
		"4|Contractors|custom|1|1|3",
		"1|Unread|unread|1|2|0",
		"2|Favorites|favorites|1|3|2",
		"3|Groups|groups|1|4|0",
		"5|Communities|community|0|5|0",
	}, "\n")
	if got != want {
		t.Fatalf("v_lists:\n%s\nwant:\n%s", got, want)
	}
	got = listRows(t, w, `SELECT list, COALESCE(chat_id, -1), jid FROM v_list_chats ORDER BY list_id, jid`)
	want = strings.Join([]string{
		"Favorites|1|120363000000000001@g.us",
		"Favorites|2|971500000001@s.whatsapp.net", // a LID the map knows → its phone chat
		"Contractors|4|110000000003@lid",          // a chat keyed on its LID
		"Contractors|-1|110000000099@lid",         // not in the database
		"Contractors|2|971500000001@s.whatsapp.net",
		"🎗️|1|120363000000000001@g.us",
	}, "\n")
	if got != want {
		t.Fatalf("v_list_chats:\n%s\nwant:\n%s", got, want)
	}

	// A rebuild replaces everything: a list gone from WhatsApp is gone here.
	if _, _, err := w.ReplaceLists(ctx, listStateOf(accountLists()[:3])); err != nil {
		t.Fatal(err)
	}
	if got := listRows(t, w, `SELECT COUNT(*) FROM wa_list`); got != "3" {
		t.Fatalf("lists after a smaller rebuild: %s, want 3", got)
	}
	if got := listRows(t, w, `SELECT COUNT(*) FROM wa_list_chat`); got != "0" {
		t.Fatalf("memberships after a smaller rebuild: %s, want 0", got)
	}
}

// TestApplyListRecord replays what a phone sends when the user creates
// a list with a chat in it, removes and re-adds the chat, renames the
// list and deletes it — the sequence observed on a real account.
func TestApplyListRecord(t *testing.T) {
	ctx := context.Background()
	w := archiveWriter(t)
	if _, _, err := w.ReplaceLists(ctx, listStateOf(accountLists())); err != nil {
		t.Fatal(err)
	}
	apply := func(m appstate.Mutation) {
		t.Helper()
		if _, err := w.ApplyListRecord(ctx, m.Index, m.Action); err != nil {
			t.Fatal(err)
		}
	}
	chatsOf := func(id string) string {
		return listRows(t, w, `SELECT GROUP_CONCAT(jid, ',') FROM (SELECT jid FROM wa_list_chat WHERE list_id = `+id+` ORDER BY jid)`)
	}

	apply(labelEdit("7", "probe-phone", waSyncAction.LabelEditAction_CUSTOM, true, 6, false))
	apply(labelOrder(6, 4, 1, 2, 3, 5, 7))
	apply(labelJID("7", "110000000003@lid", true))
	if got := listRows(t, w, `SELECT name, position, chats FROM v_lists WHERE id = 7`); got != "probe-phone|6|1" {
		t.Fatalf("created list: %q", got)
	}
	apply(labelJID("7", "110000000003@lid", false))
	if got := chatsOf("7"); got != "NULL" {
		t.Fatalf("after removing the chat: %q", got)
	}
	apply(labelJID("7", "110000000003@lid", true))
	apply(labelEdit("7", "probe-phone-2", waSyncAction.LabelEditAction_CUSTOM, true, 6, false))
	if got := listRows(t, w, `SELECT name, position, chats FROM v_lists WHERE id = 7`); got != "probe-phone-2|6|1" {
		t.Fatalf("renamed list: %q", got)
	}
	apply(labelEdit("7", "probe-phone-2", waSyncAction.LabelEditAction_CUSTOM, true, 6, true))
	apply(labelJID("7", "110000000003@lid", false))
	if got := listRows(t, w, `SELECT COUNT(*) FROM wa_list WHERE id = 7`); got != "0" {
		t.Fatalf("deleted list still there: %s", got)
	}

	// A phone-number member removed by its phone JID, a LID member by LID.
	apply(labelJID("4", "971500000001@s.whatsapp.net", false))
	apply(labelJID("4", "110000000099@lid", false))
	if got := chatsOf("4"); got != "110000000003@lid" {
		t.Fatalf("Contractors after removals: %q", got)
	}
	// Favorites is replaced whole.
	apply(favorites("971500000005@s.whatsapp.net"))
	if got := chatsOf("2"); got != "971500000005@s.whatsapp.net" {
		t.Fatalf("Favorites after a change: %q", got)
	}
	// Records that are not about lists change nothing.
	if line, err := w.ApplyListRecord(ctx, []string{"archive", "x"}, &waSyncAction.SyncActionValue{}); err != nil || line != "" {
		t.Fatalf("non-list record: %q, %v", line, err)
	}
}

// TestListSyncWaitsForRebuild: changes before the first rebuild are
// left to it, and a failed rebuild keeps the last copy.
func TestListSyncWaitsForRebuild(t *testing.T) {
	ctx := context.Background()
	w := archiveWriter(t)
	fail := true
	s := &listSync{w: w, decode: func(context.Context) ([]appstate.Mutation, error) {
		if fail {
			return nil, context.DeadlineExceeded
		}
		return accountLists(), nil
	}}
	m := labelEdit("9", "early", waSyncAction.LabelEditAction_CUSTOM, true, 7, false)
	s.apply(ctx, m.Index, m.Action)
	s.rebuild(ctx)
	if got := listRows(t, w, `SELECT COUNT(*) FROM wa_list`); got != "0" {
		t.Fatalf("lists before any rebuild: %s", got)
	}
	fail = false
	s.rebuild(ctx)
	fail = true
	s.rebuild(ctx)
	if got := listRows(t, w, `SELECT COUNT(*) FROM wa_list`); got != "6" {
		t.Fatalf("lists after a failed rebuild: %s, want the last copy's 6", got)
	}
	s.apply(ctx, m.Index, m.Action)
	if got := listRows(t, w, `SELECT name FROM wa_list WHERE id = 9`); got != "early" {
		t.Fatalf("change after the rebuild: %q", got)
	}
}

// TestDecodeIntoScratchStore: decoding lists on the side leaves the
// device's own app-state version alone.
func TestDecodeIntoScratchStore(t *testing.T) {
	ctx := context.Background()
	container, err := sqlstore.New(ctx, "sqlite3",
		"file:"+filepath.Join(t.TempDir(), "session.db")+"?_foreign_keys=on", waLog.Noop)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { container.Close() })
	real := container.NewDevice()
	real.ID = &types.JID{User: "971500000000", Server: types.DefaultUserServer}
	real.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{}, AccountSignature: make([]byte, 64),
		AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := container.PutDevice(ctx, real); err != nil {
		t.Fatal(err)
	}
	keyID := []byte{0, 0, 0, 0, 0x7f, 0x28}
	if err := real.AppStateKeys.PutAppStateSyncKey(ctx, keyID, store.AppStateSyncKey{Data: bytes.Repeat([]byte{9}, 32), Fingerprint: []byte{}, Timestamp: 1}); err != nil {
		t.Fatal(err)
	}
	// A patch adding a chat to list 4, as a phone would write it.
	raw, err := appstate.NewProcessor(real, waLog.Noop).EncodePatch(ctx, keyID, appstate.HashState{},
		appstate.BuildLabelChat(types.NewJID("110000000003", types.HiddenUserServer), "4", true))
	if err != nil {
		t.Fatal(err)
	}
	var patch waServerSync.SyncdPatch
	if err := proto.Unmarshal(raw, &patch); err != nil {
		t.Fatal(err)
	}
	patch.Version = &waServerSync.SyncdVersion{Version: proto.Uint64(1)}

	dev := *real
	dev.AppState = newMemAppState()
	muts, _, err := appstate.NewProcessor(&dev, waLog.Noop).DecodePatches(ctx,
		&appstate.PatchList{Name: appstate.WAPatchRegular, Patches: []*waServerSync.SyncdPatch{&patch}}, appstate.HashState{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(muts) != 1 || !slices.Equal(muts[0].Index, []string{"label_jid", "4", "110000000003@lid"}) {
		t.Fatalf("decoded %+v", muts)
	}
	if v, _, err := real.AppState.GetAppStateVersion(ctx, string(appstate.WAPatchRegular)); err != nil || v != 0 {
		t.Fatalf("the device's own regular state moved to v%d (%v)", v, err)
	}
}
