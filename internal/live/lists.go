package live

// Lists. WhatsApp's chat lists — the filters at the top of the chat
// list: Unread, Favorites, Groups and the user's own — are app-state
// records like archiving: one `label_edit` per list, one `label_jid`
// per chat in a list (labeled true, or false once removed), and one
// `label_reordering` with the filters' order, all in the `regular`
// collection; Favorites' chats are a single `favorites` record in
// `regular_high`. live keeps a replica in wa_list / wa_list_chat: rebuilt
// from WhatsApp at every connect, then kept current from the changes
// WhatsApp syncs. WhatsApp stays the source of truth.

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// isListRecord reports whether an app-state index belongs to lists.
func isListRecord(index []string) bool {
	if len(index) == 0 {
		return false
	}
	switch index[0] {
	case appstate.IndexLabelEdit, appstate.IndexLabelAssociationChat,
		appstate.IndexLabelReordering, appstate.IndexFavorites:
		return true
	}
	return false
}

// listState is what the list records say, folded.
type listState struct {
	lists     map[int64]*waSyncAction.LabelEditAction // deleted ones included
	order     []int32
	members   map[int64]map[string]bool // list → chat JID as named → in the list
	favorites []string
	// hasFavorites: a favorites record was seen (an empty one empties Favorites).
	hasFavorites bool
}

// listStateOf folds decoded mutations, in the order WhatsApp applied
// them, into the lists they describe. A REMOVE drops a record; the phone
// only removes one to set it again under a newer key.
func listStateOf(muts []appstate.Mutation) listState {
	type rec struct {
		index []string
		value *waSyncAction.SyncActionValue
	}
	final := map[string]rec{}
	var order []string
	for _, m := range muts {
		if !isListRecord(m.Index) {
			continue
		}
		key := strings.Join(m.Index, "\x00")
		if m.Operation == waServerSync.SyncdMutation_REMOVE {
			delete(final, key)
			continue
		}
		if _, seen := final[key]; !seen {
			order = append(order, key)
		}
		final[key] = rec{m.Index, m.Action}
	}
	st := listState{lists: map[int64]*waSyncAction.LabelEditAction{}, members: map[int64]map[string]bool{}}
	for _, key := range order {
		r, ok := final[key]
		if !ok {
			continue
		}
		switch r.index[0] {
		case appstate.IndexLabelEdit:
			if id, ok := listID(r.index); ok {
				st.lists[id] = r.value.GetLabelEditAction()
			}
		case appstate.IndexLabelAssociationChat:
			if id, ok := listID(r.index); ok && len(r.index) > 2 {
				if st.members[id] == nil {
					st.members[id] = map[string]bool{}
				}
				st.members[id][r.index[2]] = r.value.GetLabelAssociationAction().GetLabeled()
			}
		case appstate.IndexLabelReordering:
			st.order = r.value.GetLabelReorderingAction().GetSortedLabelIDs()
		case appstate.IndexFavorites:
			st.hasFavorites = true
			st.favorites = nil
			for _, f := range r.value.GetFavoritesAction().GetFavorites() {
				st.favorites = append(st.favorites, f.GetID())
			}
		}
	}
	return st
}

func listID(index []string) (int64, bool) {
	if len(index) < 2 {
		return 0, false
	}
	id, err := strconv.ParseInt(index[1], 10, 64)
	return id, err == nil
}

// listKind names a list's type the way v_lists shows it.
func listKind(a *waSyncAction.LabelEditAction) string {
	switch t := a.GetType(); t {
	case waSyncAction.LabelEditAction_NONE, waSyncAction.LabelEditAction_CUSTOM:
		return "custom"
	default:
		return strings.ToLower(t.String())
	}
}

// listMember is a row of wa_list_chat: the chat as the list names it,
// with a DM's phone JID when known.
func (w *Writer) listMember(ctx context.Context, jid string) (string, sql.NullString) {
	j, err := types.ParseJID(jid)
	if err != nil {
		return jid, sql.NullString{}
	}
	j = j.ToNonAD()
	switch j.Server {
	case types.DefaultUserServer:
		return j.String(), sql.NullString{String: j.String(), Valid: true}
	case types.HiddenUserServer:
		if w.resolvePN != nil {
			if pn, ok := w.resolvePN(ctx, j); ok {
				return j.String(), sql.NullString{String: pn.ToNonAD().String(), Valid: true}
			}
		}
	}
	return j.String(), sql.NullString{}
}

// ReplaceLists makes wa_list and wa_list_chat what st describes, in one
// transaction. Returns how many lists and memberships it wrote.
func (w *Writer) ReplaceLists(ctx context.Context, st listState) (lists, chats int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	db, err := openRW(w.dbPath)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM wa_list; DELETE FROM wa_list_chat`); err != nil {
		return 0, 0, fmt.Errorf("clear lists: %w", err)
	}
	position := map[int64]int{}
	for i, id := range st.order {
		position[int64(id)] = i
	}
	ids := make([]int64, 0, len(st.lists))
	for id := range st.lists {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		a := st.lists[id]
		if a.GetDeleted() {
			continue
		}
		if err := putList(ctx, tx, id, a, position); err != nil {
			return 0, 0, err
		}
		lists++
		in := st.members[id]
		if a.GetType() == waSyncAction.LabelEditAction_FAVORITES && st.hasFavorites {
			in = map[string]bool{}
			for _, jid := range st.favorites {
				in[jid] = true
			}
		}
		for jid, labeled := range in {
			if !labeled {
				continue
			}
			if err := w.putMember(ctx, tx, id, jid); err != nil {
				return 0, 0, err
			}
			chats++
		}
	}
	return lists, chats, tx.Commit()
}

func putList(ctx context.Context, tx *sql.Tx, id int64, a *waSyncAction.LabelEditAction, position map[int64]int) error {
	pos := sql.NullInt64{}
	if p, ok := position[id]; ok {
		pos = sql.NullInt64{Int64: int64(p), Valid: true}
	} else if a.OrderIndex != nil {
		pos = sql.NullInt64{Int64: int64(a.GetOrderIndex()), Valid: true}
	}
	shown := 0
	if a.GetIsActive() {
		shown = 1
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO wa_list (id, name, kind, shown, position) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, kind = excluded.kind,
		    shown = excluded.shown, position = COALESCE(wa_list.position, excluded.position)`,
		id, a.GetName(), listKind(a), shown, pos)
	if err != nil {
		return fmt.Errorf("record list %d: %w", id, err)
	}
	return nil
}

func (w *Writer) putMember(ctx context.Context, tx *sql.Tx, id int64, jid string) error {
	name, phone := w.listMember(ctx, jid)
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO wa_list_chat (list_id, jid, phone) VALUES (?, ?, ?)`, id, name, phone); err != nil {
		return fmt.Errorf("record list %d chat %s: %w", id, jid, err)
	}
	return nil
}

// ApplyListRecord records one list change WhatsApp synced — from the
// phone, another device, or live's own write coming back. Returns a
// line for the log; empty when the record changed nothing here.
func (w *Writer) ApplyListRecord(ctx context.Context, index []string, v *waSyncAction.SyncActionValue) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	db, err := openRW(w.dbPath)
	if err != nil {
		return "", err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var line string
	switch index[0] {
	case appstate.IndexLabelEdit:
		id, ok := listID(index)
		if !ok {
			return "", nil
		}
		a := v.GetLabelEditAction()
		if a.GetDeleted() {
			if _, err := tx.ExecContext(ctx, `DELETE FROM wa_list WHERE id = ?; DELETE FROM wa_list_chat WHERE list_id = ?`, id, id); err != nil {
				return "", fmt.Errorf("delete list %d: %w", id, err)
			}
			line = fmt.Sprintf("list %d deleted", id)
		} else {
			if err := putList(ctx, tx, id, a, nil); err != nil {
				return "", err
			}
			line = fmt.Sprintf("list %d %q (%s)", id, a.GetName(), listKind(a))
		}
	case appstate.IndexLabelAssociationChat:
		id, ok := listID(index)
		if !ok || len(index) < 3 {
			return "", nil
		}
		if v.GetLabelAssociationAction().GetLabeled() {
			if err := w.putMember(ctx, tx, id, index[2]); err != nil {
				return "", err
			}
			line = fmt.Sprintf("list %d + chat %s", id, index[2])
		} else {
			name, _ := w.listMember(ctx, index[2])
			if _, err := tx.ExecContext(ctx, `DELETE FROM wa_list_chat WHERE list_id = ? AND jid = ?`, id, name); err != nil {
				return "", fmt.Errorf("remove list %d chat %s: %w", id, index[2], err)
			}
			line = fmt.Sprintf("list %d - chat %s", id, index[2])
		}
	case appstate.IndexLabelReordering:
		order := v.GetLabelReorderingAction().GetSortedLabelIDs()
		if _, err := tx.ExecContext(ctx, `UPDATE wa_list SET position = NULL`); err != nil {
			return "", err
		}
		for i, id := range order {
			if _, err := tx.ExecContext(ctx, `UPDATE wa_list SET position = ? WHERE id = ?`, i, id); err != nil {
				return "", fmt.Errorf("record list order: %w", err)
			}
		}
		line = fmt.Sprintf("list order %v", order)
	case appstate.IndexFavorites:
		var ids []int64
		rows, err := tx.QueryContext(ctx, `SELECT id FROM wa_list WHERE kind = 'favorites'`)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return "", err
			}
			ids = append(ids, id)
		}
		rows.Close()
		favs := v.GetFavoritesAction().GetFavorites()
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM wa_list_chat WHERE list_id = ?`, id); err != nil {
				return "", err
			}
			for _, f := range favs {
				if err := w.putMember(ctx, tx, id, f.GetID()); err != nil {
					return "", err
				}
			}
		}
		line = fmt.Sprintf("favorites: %d chats", len(favs))
	default:
		return "", nil
	}
	return line, tx.Commit()
}

// listSync keeps the replica: rebuild serializes with incoming changes,
// so a change arriving mid-rebuild lands on top of it, not under it.
type listSync struct {
	mu      sync.Mutex
	w       *Writer
	decode  func(ctx context.Context) ([]appstate.Mutation, error)
	enabled bool // false until the first rebuild, so changes never land in a stale copy
}

// rebuild replaces the replica with what WhatsApp holds now. On failure
// the last copy stays.
func (s *listSync) rebuild(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	muts, err := s.decode(ctx)
	if err != nil {
		logf("lists: rebuild from WhatsApp FAILED (keeping the last copy): %v", err)
		return
	}
	lists, chats, err := s.w.ReplaceLists(ctx, listStateOf(muts))
	if err != nil {
		logf("lists: rebuild write FAILED (keeping the last copy): %v", err)
		return
	}
	s.enabled = true
	logf("lists: rebuilt from WhatsApp — %d lists, %d chats in them", lists, chats)
}

// apply records one change WhatsApp synced.
func (s *listSync) apply(ctx context.Context, index []string, v *waSyncAction.SyncActionValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return // the next rebuild includes it
	}
	line, err := s.w.ApplyListRecord(ctx, index, v)
	switch {
	case err != nil:
		logf("lists: %s: ERROR %v", strings.Join(index, " "), err)
	case line != "":
		logf("lists: %s", line)
	}
}

// record records list changes live just made on WhatsApp.
func (s *listSync) record(ctx context.Context, muts []appstate.MutationInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return nil // the next rebuild includes it
	}
	return s.w.recordList(ctx, muts)
}

// decodeLists reads the list records straight from WhatsApp: both
// collections downloaded whole and decoded with every MAC checked, into
// a scratch store so whatsmeow's own sync state is left alone.
func decodeLists(client *whatsmeow.Client) func(ctx context.Context) ([]appstate.Mutation, error) {
	return func(ctx context.Context) ([]appstate.Mutation, error) {
		dev := *client.Store
		dev.AppState = newMemAppState()
		proc := appstate.NewProcessor(&dev, waLog.Noop)
		var all []appstate.Mutation
		for _, name := range []appstate.WAPatchName{appstate.WAPatchRegular, appstate.WAPatchRegularHigh} {
			var state appstate.HashState
			snapshot := true
			for {
				list, err := client.DangerousInternals().FetchAppStatePatches(ctx, name, state.Version, snapshot)
				if err != nil {
					return nil, fmt.Errorf("fetch %s: %w", name, err)
				}
				snapshot = false
				muts, next, err := proc.DecodePatches(ctx, list, state, true)
				if err != nil {
					return nil, fmt.Errorf("decode %s: %w", name, err)
				}
				all = append(all, muts...)
				state = next
				if !list.HasMorePatches {
					break
				}
			}
		}
		return all, nil
	}
}

// memAppState is a throwaway app-state store for decoding on the side.
type memAppState struct {
	mu       sync.Mutex
	versions map[string]appstate.HashState
	macs     map[string]map[string][]byte // collection → index MAC → value MAC
}

func newMemAppState() *memAppState {
	return &memAppState{versions: map[string]appstate.HashState{}, macs: map[string]map[string][]byte{}}
}

func (m *memAppState) PutAppStateVersion(_ context.Context, name string, version uint64, hash [128]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.versions[name] = appstate.HashState{Version: version, Hash: hash}
	return nil
}

func (m *memAppState) GetAppStateVersion(_ context.Context, name string) (uint64, [128]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.versions[name]
	return s.Version, s.Hash, nil
}

func (m *memAppState) DeleteAppStateVersion(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.versions, name)
	delete(m.macs, name)
	return nil
}

func (m *memAppState) PutAppStateMutationMACs(_ context.Context, name string, _ uint64, muts []store.AppStateMutationMAC) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.macs[name] == nil {
		m.macs[name] = map[string][]byte{}
	}
	for _, x := range muts {
		m.macs[name][hex.EncodeToString(x.IndexMAC)] = x.ValueMAC
	}
	return nil
}

func (m *memAppState) DeleteAppStateMutationMACs(_ context.Context, name string, indexMACs [][]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, i := range indexMACs {
		delete(m.macs[name], hex.EncodeToString(i))
	}
	return nil
}

func (m *memAppState) GetAppStateMutationMAC(_ context.Context, name string, indexMAC []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.macs[name][hex.EncodeToString(indexMAC)], nil
}
