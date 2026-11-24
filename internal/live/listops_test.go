package live

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/encoding/protojson"
)

// describe renders a planned patch as "index value" lines.
func describe(p appstate.PatchInfo) string {
	var out []string
	for _, m := range p.Mutations {
		v, _ := protojson.MarshalOptions{UseProtoNames: false}.Marshal(m.Value)
		out = append(out, strings.Join(m.Index, " ")+" "+strings.ReplaceAll(string(v), " ", ""))
	}
	return strings.Join(out, "\n")
}

// The phone, creating a list on this account, wrote exactly this (the
// deleted list's ID 7 reused, colour = custom lists so far, order last).
func TestPlanCreateMatchesThePhone(t *testing.T) {
	st := listStateOf(accountLists())
	p, out, err := planCreate(st, " probe-phone ")
	if err != nil {
		t.Fatal(err)
	}
	want := `label_edit 7 {"labelEditAction":{"name":"probe-phone","color":2,"predefinedID":0,"deleted":false,"orderIndex":6,"isActive":true,"type":"CUSTOM","isImmutable":false}}
label_reordering {"labelReorderingAction":{"sortedLabelIDs":[6,4,1,2,3,5,7]}}`
	if got := describe(p); got != want {
		t.Fatalf("create:\n%s\nwant:\n%s", got, want)
	}
	if out.ID != 7 || out.Name != "probe-phone" || p.Type != appstate.WAPatchRegular || p.Mutations[0].Version != 3 || p.Mutations[1].Version != 3 {
		t.Fatalf("create: %+v, type %s", out, p.Type)
	}
	for _, name := range []string{"", "  ", "contractors", "UNREAD"} {
		if _, _, err := planCreate(st, name); err == nil {
			t.Errorf("create %q was planned", name)
		}
	}
}

func TestPlanRenameAndDelete(t *testing.T) {
	st := listStateOf(accountLists())
	p, out, err := planRename(st, "contractors", "Builders")
	if err != nil {
		t.Fatal(err)
	}
	// The whole record again, only the name changed (Contractors predates
	// list order, so it has none).
	if got, want := describe(p), `label_edit 4 {"labelEditAction":{"name":"Builders","color":0,"deleted":false,"isActive":true,"type":"CUSTOM"}}`; got != want || out.ID != 4 {
		t.Fatalf("rename:\n%s\nwant:\n%s", got, want)
	}
	if _, _, err := planRename(st, "4", "🎗️"); err == nil {
		t.Error("rename onto another list's name was planned")
	}
	if _, _, err := planRename(st, "4", "contractors"); err != nil {
		t.Errorf("rename to its own name in another case: %v", err)
	}

	// Delete: marked deleted and every chat in it removed; order untouched.
	p, out, err = planDelete(st, "4")
	if err != nil {
		t.Fatal(err)
	}
	want := `label_edit 4 {"labelEditAction":{"name":"Contractors","color":0,"deleted":true,"isActive":true,"type":"CUSTOM"}}
label_jid 4 110000000003@lid {"labelAssociationAction":{"labeled":false}}
label_jid 4 110000000099@lid {"labelAssociationAction":{"labeled":false}}
label_jid 4 971500000001@s.whatsapp.net {"labelAssociationAction":{"labeled":false}}`
	if got := describe(p); got != want || out.Name != "Contractors" {
		t.Fatalf("delete:\n%s\nwant:\n%s", got, want)
	}
}

func TestFindListRefuses(t *testing.T) {
	st := listStateOf(append(accountLists(), labelEdit("8", "Twins", waSyncAction.LabelEditAction_CUSTOM, true, 7, false),
		labelEdit("9", "twins", waSyncAction.LabelEditAction_CUSTOM, true, 8, false)))
	for ref, want := range map[string]string{
		"":          "no list given",
		"Nope":      `no list "Nope"`,
		"7":         `no list "7"`, // deleted
		"Favorites": "built-in filters (favorites)",
		"1":         "built-in filters (unread)",
		"twins":     "2 lists are named",
	} {
		if _, _, err := findList(st, ref); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("findList(%q): %v, want %q", ref, err, want)
		}
	}
	if id, _, err := findList(st, "9"); err != nil || id != 9 {
		t.Errorf("by ID: %d, %v", id, err)
	}
	if id, _, err := findList(st, " 🎗️ "); err != nil || id != 6 {
		t.Errorf("by name: %d, %v", id, err)
	}
}

func TestPlanMembership(t *testing.T) {
	st := listStateOf(accountLists())
	lid := types.NewJID("110000000001", types.HiddenUserServer)
	phone := types.NewJID("971500000001", types.DefaultUserServer)
	forms := []types.JID{lid, phone}

	// Add by LID, how WhatsApp addresses the chat now.
	p, out, err := planMembership(st, "🎗️", lid, forms, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := describe(p), `label_jid 6 110000000001@lid {"labelAssociationAction":{"labeled":true}}`; got != want || out.Chat != lid.String() {
		t.Fatalf("add:\n%s\nwant:\n%s", got, want)
	}
	// Contractors holds this chat by its phone JID (an older entry):
	// removing it clears that one; adding it by LID writes the LID.
	p, _, err = planMembership(st, "Contractors", lid, forms, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := describe(p), `label_jid 4 971500000001@s.whatsapp.net {"labelAssociationAction":{"labeled":false}}`; got != want {
		t.Fatalf("remove:\n%s\nwant:\n%s", got, want)
	}
	// Already there under that JID: nothing to write.
	if p, _, err := planMembership(st, "Contractors", types.NewJID("110000000003", types.HiddenUserServer), nil, true); err != nil || len(p.Mutations) != 0 {
		t.Fatalf("add of a member: %v, %v", describe(p), err)
	}
	if _, _, err := planMembership(st, "🎗️", phone, []types.JID{phone}, false); err == nil || !strings.Contains(err.Error(), "is not in list") {
		t.Fatalf("remove of a non-member: %v", err)
	}
}

// TestListThroughLive drives the endpoint: create, add, remove, rename,
// delete, each written to WhatsApp (the stand-in) and recorded.
func TestListThroughLive(t *testing.T) {
	ctx := context.Background()
	w, root, wire := archiveFixture(t)
	if _, _, err := w.ReplaceLists(ctx, listStateOf(accountLists())); err != nil {
		t.Fatal(err)
	}
	change := func(op, list, name, chat string) ListChange {
		t.Helper()
		out, err := ManageList(ctx, root, op, list, name, chat)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		return out
	}

	if out := change("create", "", "Clients", ""); out.ID != 7 || out.Name != "Clients" || out.Op != "create" {
		t.Fatalf("create: %+v", out)
	}
	if got := listRows(t, w, `SELECT name, kind, shown, position, chats FROM v_lists WHERE id = 7`); got != "Clients|custom|1|6|0" {
		t.Fatalf("created list: %q", got)
	}
	// Chat 2 by its phone number: written by its LID, shown as the chat.
	if out := change("add", "Clients", "", "+971500000001"); out.Chat != "110000000001@lid" {
		t.Fatalf("add: %+v", out)
	}
	if got := listRows(t, w, `SELECT chat_id FROM v_list_chats WHERE list_id = 7`); got != "2" {
		t.Fatalf("list chats after add: %q", got)
	}
	change("remove", "7", "", "971500000001@s.whatsapp.net")
	if got := listRows(t, w, `SELECT COUNT(*) FROM v_list_chats WHERE list_id = 7`); got != "0" {
		t.Fatalf("list chats after remove: %q", got)
	}
	change("add", "Clients", "", "120363000000000001@g.us")
	change("rename", "Clients", "Customers", "")
	if got := listRows(t, w, `SELECT name, chats FROM v_lists WHERE id = 7`); got != "Customers|1" {
		t.Fatalf("after rename: %q", got)
	}
	change("delete", "Customers", "", "")
	if got := listRows(t, w, `SELECT COUNT(*) FROM wa_list WHERE id = 7`); got != "0" {
		t.Fatalf("after delete: %q", got)
	}
	var ops []string
	for _, p := range wire.lists {
		for _, m := range p.Mutations {
			ops = append(ops, m.Index[0])
		}
	}
	if want := []string{"label_edit", "label_reordering", "label_jid", "label_jid", "label_jid", "label_edit", "label_edit", "label_jid"}; !slices.Equal(ops, want) {
		t.Fatalf("written: %v, want %v", ops, want)
	}

	for _, c := range []struct{ op, list, name, chat, want string }{
		{"rename", "Favorites", "Faves", "", "built-in filters"},
		{"add", "🎗️", "", "+971500000077", "is not in the database"},
		{"add", "🎗️", "", "Sarah", "invalid chat"},
		{"create", "", "", "", "needs a name"},
		{"archive", "", "", "", "unknown list operation"},
	} {
		if _, err := ManageList(ctx, root, c.op, c.list, c.name, c.chat); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %q: %v, want %q", c.op, c.list, err, c.want)
		}
	}
	n := len(wire.lists)
	wire.listErr = errors.New("server returned error updating app state")
	if _, err := ManageList(ctx, root, "create", "", "Later", ""); err == nil || !strings.Contains(err.Error(), "list create failed") {
		t.Fatalf("create with WhatsApp failing: %v", err)
	}
	if len(wire.lists) != n || listRows(t, w, `SELECT COUNT(*) FROM wa_list WHERE name = 'Later'`) != "0" {
		t.Fatal("a failed create was recorded")
	}
}

func TestListLiveNotRunning(t *testing.T) {
	if _, err := ManageList(context.Background(), t.TempDir(), "create", "", "x", ""); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("got %v, want ErrNotRunning", err)
	}
}
