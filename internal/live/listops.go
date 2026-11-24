package live

// Managing lists. `whatskept list` and the MCP list tool hand a change
// to the running live, which writes it the way the phone does (record
// shapes observed on a real account):
//
//   - create: the list's record — custom, shown, the next ID (highest
//     existing + 1, the phone's rule) — and the filter order with the
//     new list last.
//   - rename: the whole record again, only the name changed.
//   - delete: the record marked deleted and every chat in it removed, in
//     one patch; the filter order is left as it is.
//   - add / remove: one chat's membership; a DM by its LID where known.
//
// Only the user's own (custom) lists change; WhatsApp's built-in filters
// are refused. Each change is planned from WhatsApp's lists read fresh,
// not from the replica, and recorded in the replica once WhatsApp took it.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// ListChange describes a list after a change.
type ListChange struct {
	Op   string `json:"op" jsonschema:"what was done: create, rename, delete, add or remove"`
	ID   int64  `json:"id" jsonschema:"the list's ID (v_lists.id)"`
	Name string `json:"name" jsonschema:"the list's name (after a rename, the new one)"`
	Chat string `json:"chat,omitempty" jsonschema:"for add and remove: the chat JID, as WhatsApp addresses it"`
	// Warning is set when WhatsApp took the change but the database was
	// not updated. The change stands; live corrects the rows when it
	// next rebuilds its copy of the lists.
	Warning string `json:"warning,omitempty" jsonschema:"set when the change was made on WhatsApp but not recorded in the database"`
}

type listRequest struct {
	Op   string `json:"op"`             // create | rename | delete | add | remove
	List string `json:"list,omitempty"` // a list's ID or name; not for create
	Name string `json:"name,omitempty"` // create, rename: the (new) name
	Chat string `json:"chat,omitempty"` // add, remove: phone number or JID
}

type listReply struct {
	ListChange
	Error string `json:"error,omitempty"`
}

func (r listReply) errText() string { return r.Error }

// ManageList asks the running live to change one of the user's lists.
func ManageList(ctx context.Context, root, op, list, name, chat string) (ListChange, error) {
	r, err := post[listReply](ctx, root, "list", listRequest{Op: op, List: list, Name: name, Chat: chat})
	return r.ListChange, err
}

// listOps carries out list changes. The WhatsApp halves are injected so
// the endpoint is testable without a connection.
type listOps struct {
	state  func(ctx context.Context) (listState, error)                   // the lists as WhatsApp holds them now
	target func(ctx context.Context, to types.JID) (archiveTarget, error) // a chat in the database
	forms  func(ctx context.Context, jid types.JID) []types.JID           // every JID a chat goes by
	write  func(ctx context.Context, p appstate.PatchInfo) error          // put the change on WhatsApp
	record func(ctx context.Context, muts []appstate.MutationInfo) error  // record it in the replica

	mu sync.Mutex // one change at a time: a create picks its ID from what it read
}

// badList is a request refused before WhatsApp is asked anything.
func badList(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// do carries out one request; the status is the endpoint's HTTP status.
func (o *listOps) do(ctx context.Context, req listRequest) (ListChange, int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	switch req.Op {
	case "create", "rename", "delete", "add", "remove":
	default:
		return ListChange{}, http.StatusBadRequest, fmt.Errorf("unknown list operation %q — want create, rename, delete, add or remove", req.Op)
	}
	var chat types.JID
	var t archiveTarget
	if req.Op == "add" || req.Op == "remove" {
		var err error
		if chat, err = ParseChat(req.Chat); err != nil {
			return ListChange{}, http.StatusBadRequest, err
		}
		if t, err = o.target(ctx, chat); err != nil {
			return ListChange{}, http.StatusBadRequest, fmt.Errorf("cannot %s: %w", req.Op, err)
		}
	}
	st, err := o.state(ctx)
	if err != nil {
		return ListChange{}, http.StatusBadGateway, fmt.Errorf("cannot read the lists from WhatsApp: %w", err)
	}

	var p appstate.PatchInfo
	var out ListChange
	switch req.Op {
	case "create":
		p, out, err = planCreate(st, req.Name)
	case "rename":
		p, out, err = planRename(st, req.List, req.Name)
	case "delete":
		p, out, err = planDelete(st, req.List)
	default:
		p, out, err = planMembership(st, req.List, t.JID, o.forms(ctx, t.JID), req.Op == "add")
	}
	if err != nil {
		return ListChange{}, http.StatusBadRequest, err
	}
	out.Op = req.Op
	if len(p.Mutations) == 0 {
		return out, http.StatusOK, nil
	}
	if err := o.write(ctx, p); err != nil {
		logf("list %s %d FAILED: %v", req.Op, out.ID, err)
		return ListChange{}, http.StatusBadGateway, fmt.Errorf("list %s failed: %w", req.Op, err)
	}
	if err := o.record(ctx, p.Mutations); err != nil {
		out.Warning = "changed on WhatsApp, but not recorded in the database: " + err.Error()
	}
	return out, http.StatusOK, nil
}

// findList resolves a list by ID or by name (ignoring case) among the
// lists that exist, and requires one of the user's own.
func findList(st listState, ref string) (int64, *waSyncAction.LabelEditAction, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, nil, badList("no list given — name it by its v_lists.id or name")
	}
	var id int64
	if n, err := strconv.ParseInt(ref, 10, 64); err == nil {
		if a, ok := st.lists[n]; ok && !a.GetDeleted() {
			id = n
		}
	}
	if id == 0 {
		var hits []int64
		for n, a := range st.lists {
			if !a.GetDeleted() && strings.EqualFold(strings.TrimSpace(a.GetName()), ref) {
				hits = append(hits, n)
			}
		}
		slices.Sort(hits)
		switch len(hits) {
		case 0:
			return 0, nil, badList("no list %q", ref)
		case 1:
			id = hits[0]
		default:
			return 0, nil, badList("%d lists are named %q (IDs %v) — name it by ID", len(hits), ref, hits)
		}
	}
	a := st.lists[id]
	if listKind(a) != "custom" {
		return 0, nil, badList("%q is one of WhatsApp's built-in filters (%s) — only your own lists can be changed", a.GetName(), listKind(a))
	}
	return id, a, nil
}

// checkName refuses an empty name or one another list already has.
func checkName(st listState, name string, except int64) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", badList("a list needs a name")
	}
	for n, a := range st.lists {
		if n != except && !a.GetDeleted() && strings.EqualFold(strings.TrimSpace(a.GetName()), name) {
			return "", badList("a list named %q already exists (ID %d)", a.GetName(), n)
		}
	}
	return name, nil
}

func listEditMutation(id int64, a *waSyncAction.LabelEditAction) appstate.MutationInfo {
	return appstate.MutationInfo{Index: []string{appstate.IndexLabelEdit, strconv.FormatInt(id, 10)}, Version: 3,
		Value: &waSyncAction.SyncActionValue{LabelEditAction: a}}
}

func listChatMutation(id int64, jid types.JID, labeled bool) appstate.MutationInfo {
	return appstate.MutationInfo{Index: []string{appstate.IndexLabelAssociationChat, strconv.FormatInt(id, 10), jid.String()}, Version: 3,
		Value: &waSyncAction.SyncActionValue{LabelAssociationAction: &waSyncAction.LabelAssociationAction{Labeled: proto.Bool(labeled)}}}
}

func planCreate(st listState, name string) (appstate.PatchInfo, ListChange, error) {
	name, err := checkName(st, name, 0)
	if err != nil {
		return appstate.PatchInfo{}, ListChange{}, err
	}
	var id int64
	custom := int32(0)
	for n, a := range st.lists {
		if a.GetDeleted() {
			continue
		}
		id = max(id, n)
		if listKind(a) == "custom" {
			custom++
		}
	}
	id++
	order := slices.DeleteFunc(slices.Clone(st.order), func(x int32) bool { return int64(x) == id })
	a := &waSyncAction.LabelEditAction{
		Name: proto.String(name), Color: proto.Int32(custom % 20), PredefinedID: proto.Int32(0),
		Deleted: proto.Bool(false), OrderIndex: proto.Int32(int32(len(order))), IsActive: proto.Bool(true),
		Type: waSyncAction.LabelEditAction_CUSTOM.Enum(), IsImmutable: proto.Bool(false),
	}
	return appstate.PatchInfo{Type: appstate.WAPatchRegular, Mutations: []appstate.MutationInfo{
			listEditMutation(id, a),
			{Index: []string{appstate.IndexLabelReordering}, Version: 3, Value: &waSyncAction.SyncActionValue{
				LabelReorderingAction: &waSyncAction.LabelReorderingAction{SortedLabelIDs: append(order, int32(id))}}},
		}},
		ListChange{ID: id, Name: name}, nil
}

func planRename(st listState, ref, name string) (appstate.PatchInfo, ListChange, error) {
	id, a, err := findList(st, ref)
	if err != nil {
		return appstate.PatchInfo{}, ListChange{}, err
	}
	if name, err = checkName(st, name, id); err != nil {
		return appstate.PatchInfo{}, ListChange{}, err
	}
	a = proto.Clone(a).(*waSyncAction.LabelEditAction)
	a.Name = proto.String(name)
	return appstate.PatchInfo{Type: appstate.WAPatchRegular, Mutations: []appstate.MutationInfo{listEditMutation(id, a)}},
		ListChange{ID: id, Name: name}, nil
}

func planDelete(st listState, ref string) (appstate.PatchInfo, ListChange, error) {
	id, a, err := findList(st, ref)
	if err != nil {
		return appstate.PatchInfo{}, ListChange{}, err
	}
	name := a.GetName()
	a = proto.Clone(a).(*waSyncAction.LabelEditAction)
	a.Deleted = proto.Bool(true)
	muts := []appstate.MutationInfo{listEditMutation(id, a)}
	var members []string
	for jid, in := range st.members[id] {
		if in {
			members = append(members, jid)
		}
	}
	slices.Sort(members)
	for _, m := range members {
		if jid, err := types.ParseJID(m); err == nil {
			muts = append(muts, listChatMutation(id, jid, false))
		}
	}
	return appstate.PatchInfo{Type: appstate.WAPatchRegular, Mutations: muts}, ListChange{ID: id, Name: name}, nil
}

// planMembership adds chat to a list by addr (how WhatsApp addresses it
// now), or removes it under every JID it goes by that the list holds.
// Adding a chat already there, under addr, writes nothing.
func planMembership(st listState, ref string, addr types.JID, forms []types.JID, add bool) (appstate.PatchInfo, ListChange, error) {
	id, a, err := findList(st, ref)
	if err != nil {
		return appstate.PatchInfo{}, ListChange{}, err
	}
	out := ListChange{ID: id, Name: a.GetName(), Chat: addr.String()}
	p := appstate.PatchInfo{Type: appstate.WAPatchRegular}
	if add {
		if !st.members[id][addr.String()] {
			p.Mutations = append(p.Mutations, listChatMutation(id, addr, true))
		}
		return p, out, nil
	}
	for _, f := range forms {
		if st.members[id][f.String()] {
			p.Mutations = append(p.Mutations, listChatMutation(id, f, false))
		}
	}
	if len(p.Mutations) == 0 {
		return appstate.PatchInfo{}, ListChange{}, badList("chat %s is not in list %q", addr, a.GetName())
	}
	return p, out, nil
}

// chatForms is every JID a chat goes by: as given, and its phone JID or
// LID where whatsmeow's map knows the other.
func (w *Writer) chatForms(ctx context.Context, jid types.JID) []types.JID {
	forms := []types.JID{jid}
	switch jid.Server {
	case types.HiddenUserServer:
		if w.resolvePN != nil {
			if pn, ok := w.resolvePN(ctx, jid); ok {
				forms = append(forms, pn.ToNonAD())
			}
		}
	case types.DefaultUserServer:
		if w.resolveLID != nil {
			if lid, ok := w.resolveLID(ctx, jid); ok {
				forms = append(forms, lid.ToNonAD())
			}
		}
	}
	return forms
}

// replicaState reads the lists back from the replica — what the fake
// endpoint plans from, having no WhatsApp to ask.
func (w *Writer) replicaState(ctx context.Context) (listState, error) {
	db, err := openRW(w.dbPath)
	if err != nil {
		return listState{}, err
	}
	defer db.Close()
	st := listState{lists: map[int64]*waSyncAction.LabelEditAction{}, members: map[int64]map[string]bool{}}
	rows, err := db.QueryContext(ctx, `SELECT id, name, kind, shown, position FROM wa_list ORDER BY COALESCE(position, 1e9), id`)
	if err != nil {
		return listState{}, err
	}
	for rows.Next() {
		var id int64
		var name, kind string
		var shown bool
		var pos *int64
		if err := rows.Scan(&id, &name, &kind, &shown, &pos); err != nil {
			rows.Close()
			return listState{}, err
		}
		typ := waSyncAction.LabelEditAction_CUSTOM
		if v, ok := waSyncAction.LabelEditAction_ListType_value[strings.ToUpper(kind)]; ok {
			typ = waSyncAction.LabelEditAction_ListType(v)
		}
		a := &waSyncAction.LabelEditAction{Name: proto.String(name), IsActive: proto.Bool(shown), Type: typ.Enum(), Deleted: proto.Bool(false)}
		if pos != nil {
			a.OrderIndex = proto.Int32(int32(*pos))
			st.order = append(st.order, int32(id))
		}
		st.lists[id] = a
	}
	rows.Close()
	rows, err = db.QueryContext(ctx, `SELECT list_id, jid FROM wa_list_chat`)
	if err != nil {
		return listState{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var jid string
		if err := rows.Scan(&id, &jid); err != nil {
			return listState{}, err
		}
		if st.members[id] == nil {
			st.members[id] = map[string]bool{}
		}
		st.members[id][jid] = true
	}
	return st, rows.Err()
}

// recordList records a list change in the replica directly.
func (w *Writer) recordList(ctx context.Context, muts []appstate.MutationInfo) error {
	for _, m := range muts {
		if _, err := w.ApplyListRecord(ctx, m.Index, m.Value); err != nil {
			return err
		}
	}
	return nil
}
