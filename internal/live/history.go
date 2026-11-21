package live

// History sync: the messages the phone pushes once, right after a QR
// pairing. Each one becomes the same *events.Message a real-time
// message is (whatsmeow's ParseWebMessage) and goes through the same
// applyMsg → decide → Writer.Apply path, so a history row is
// indistinguishable from a live capture. Dedup by stanza ID makes this
// safe on a database seeded by a backup import: what the backup holds is
// skipped, what it lacks (everything since the backup was taken) is
// added.
//
// A chunk also says things about identities that real-time messages
// don't: the phone↔LID mappings, the conversation names, and (in the
// PUSH_NAME chunk) everyone's push names. whatsmeow stores the mappings
// too, but asynchronously — after our handler has already run — so the
// chunk's own copy is what chat resolution consults first.

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Core Data entity ID for WAProfilePushName, from Z_PRIMARYKEY.
const entPushName = 13

// historyHints is what the chunks seen so far say about identities.
type historyHints struct {
	mu    sync.Mutex
	pn    map[types.JID]types.JID // LID → phone JID
	names map[types.JID]string    // chat JID (any form) → conversation name
}

func newHistoryHints() *historyHints {
	return &historyHints{pn: map[types.JID]types.JID{}, names: map[types.JID]string{}}
}

// absorb records a chunk's mappings and conversation names. Called
// before the chunk's messages are applied.
func (h *historyHints) absorb(data *waHistorySync.HistorySync) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pair := func(a, b string) {
		ja, errA := types.ParseJID(a)
		jb, errB := types.ParseJID(b)
		if errA != nil || errB != nil {
			return
		}
		ja, jb = ja.ToNonAD(), jb.ToNonAD()
		switch {
		case ja.Server == types.HiddenUserServer && jb.Server == types.DefaultUserServer:
			h.pn[ja] = jb
		case jb.Server == types.HiddenUserServer && ja.Server == types.DefaultUserServer:
			h.pn[jb] = ja
		}
	}
	for _, m := range data.GetPhoneNumberToLidMappings() {
		pair(m.GetLidJID(), m.GetPnJID())
	}
	for _, c := range data.GetConversations() {
		pair(c.GetLidJID(), c.GetPnJID())
		name := c.GetName()
		if name == "" {
			name = c.GetDisplayName()
		}
		if name == "" {
			continue
		}
		for _, s := range []string{c.GetID(), c.GetPnJID(), c.GetLidJID()} {
			if j, err := types.ParseJID(s); err == nil && !j.IsEmpty() {
				h.names[j.ToNonAD()] = name
			}
		}
	}
}

func (h *historyHints) phoneFor(lid types.JID) (types.JID, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pn, ok := h.pn[lid.ToNonAD()]
	return pn, ok
}

func (h *historyHints) name(jid types.JID) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, ok := h.names[jid.ToNonAD()]
	return n, ok
}

// historyEvents turns a chunk's conversations into message events,
// oldest first within each conversation as the phone sends them. A
// message that can't be parsed is counted, not fatal.
func historyEvents(data *waHistorySync.HistorySync,
	parse func(types.JID, *waWeb.WebMessageInfo) (*events.Message, error),
	onBad func(chat string, msg *waWeb.WebMessageInfo, err error)) []*events.Message {
	var out []*events.Message
	for _, conv := range data.GetConversations() {
		chat, err := types.ParseJID(conv.GetID())
		if err != nil {
			onBad(conv.GetID(), nil, err)
			continue
		}
		for _, hm := range conv.GetMessages() {
			evt, err := parse(chat, hm.GetMessage())
			if err != nil {
				onBad(conv.GetID(), hm.GetMessage(), err)
				continue
			}
			out = append(out, evt)
		}
	}
	return out
}

// historyStats counts what storeHistory did with one chunk.
type historyStats struct {
	Messages, Inserted, Skipped, Unparseable, Errors int
	PushNames, PushNamesStored                       int
}

// touchEvery is how often, in messages, storeHistory calls touch while
// it works through a chunk: a chunk with many attachments takes minutes,
// and the status must not look stalled meanwhile.
const touchEvery = 100

// storeHistory applies one chunk: identities first (so the chunk's own
// messages resolve against them), then push names, then every message
// through apply — in live, the same applyMsg real-time messages take.
// apply reports whether the message was inserted. touch (may be nil) is
// called on arrival and every touchEvery messages, to mark the sync as
// still active.
func storeHistory(ctx context.Context, w *Writer, hints *historyHints, data *waHistorySync.HistorySync,
	parse func(types.JID, *waWeb.WebMessageInfo) (*events.Message, error),
	apply func(*events.Message) (bool, error), touch func()) historyStats {
	var st historyStats
	if touch == nil {
		touch = func() {}
	}
	touch()
	hints.absorb(data)
	if names := data.GetPushnames(); len(names) > 0 {
		st.PushNames = len(names)
		n, err := w.PutPushNames(ctx, names)
		if err != nil {
			logf("history sync: push names: ERROR %v", err)
		}
		st.PushNamesStored = n
	}
	evts := historyEvents(data, parse, func(chat string, msg *waWeb.WebMessageInfo, err error) {
		st.Unparseable++
		// What the message was, so a failure can be told apart from a
		// system event that carries nothing to store anyway.
		logf("history sync: unparseable message in %s: %v stub=%s fields=%s",
			chat, err, msg.GetMessageStubType(), messageFields(msg.GetMessage()))
	})
	st.Messages = len(evts) + st.Unparseable
	for i, evt := range evts {
		if i > 0 && i%touchEvery == 0 {
			touch()
		}
		inserted, err := apply(evt)
		switch {
		case err != nil:
			st.Errors++
		case inserted:
			st.Inserted++
		default:
			st.Skipped++
		}
	}
	return st
}

// PutPushNames writes a PUSH_NAME chunk into ZWAPROFILEPUSHNAME, where
// the views look first for a sender or chat name. A name already stored
// for the JID is updated — the chunk is the phone's current view.
// Returns how many rows were inserted or changed.
func (w *Writer) PutPushNames(ctx context.Context, names []*waHistorySync.Pushname) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	db, err := openRW(w.dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	changed := 0
	for _, pn := range names {
		jid, err := types.ParseJID(pn.GetID())
		if err != nil || jid.IsEmpty() || pn.GetPushname() == "" {
			continue
		}
		key, name := jid.ToNonAD().String(), pn.GetPushname()
		var pk int64
		var cur sql.NullString
		err = tx.QueryRowContext(ctx,
			`SELECT Z_PK, ZPUSHNAME FROM ZWAPROFILEPUSHNAME WHERE ZJID = ? LIMIT 1`, key).Scan(&pk, &cur)
		switch {
		case err == sql.ErrNoRows:
			if pk, err = nextPK(ctx, tx, "ZWAPROFILEPUSHNAME"); err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO ZWAPROFILEPUSHNAME (Z_PK, Z_ENT, Z_OPT, ZJID, ZPUSHNAME) VALUES (?, ?, 1, ?, ?)`,
				pk, entPushName, key, name); err != nil {
				return 0, fmt.Errorf("insert push name: %w", err)
			}
			changed++
		case err != nil:
			return 0, err
		case cur.String != name:
			if _, err := tx.ExecContext(ctx,
				`UPDATE ZWAPROFILEPUSHNAME SET ZPUSHNAME = ? WHERE Z_PK = ?`, name, pk); err != nil {
				return 0, fmt.Errorf("update push name: %w", err)
			}
			changed++
		}
	}
	return changed, tx.Commit()
}

// The one-row wa_history_sync status table lets the MCP server (which
// only reads the database) tell its client a sync is under way.
const historySyncDDL = `CREATE TABLE IF NOT EXISTS wa_history_sync (
	id            INTEGER PRIMARY KEY CHECK (id = 1),
	started_at    TEXT,    -- UTC, when this pairing's sync began
	last_chunk_at TEXT,    -- UTC, last sign of activity (chunk arrived or in progress)
	progress      INTEGER, -- 0–100, the last percentage the phone reported
	inserted      INTEGER, -- messages added by this sync
	done          INTEGER  -- 1 once that percentage reached 100
)`

// ResetHistorySync opens the status for a new pairing: whatever an
// earlier pairing's sync recorded no longer describes what is arriving.
func (w *Writer) ResetHistorySync(ctx context.Context, at time.Time) error {
	return w.historyStatus(ctx, func(db *sql.DB) error {
		ts := at.UTC().Format("2006-01-02 15:04:05")
		_, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO wa_history_sync
			(id, started_at, last_chunk_at, progress, inserted, done) VALUES (1, ?, ?, 0, 0, 0)`, ts, ts)
		return err
	})
}

// RecordHistoryChunk marks the sync active at `at`, adds `inserted`
// messages, and takes the phone's percentage when it carries one. The
// phone's percentage is per chunk type — a bootstrap chunk can say 100
// with more chunks still to come — so `done` is only a hint; recent
// activity is what says a sync is still arriving.
func (w *Writer) RecordHistoryChunk(ctx context.Context, progress uint32, inserted int, at time.Time) error {
	return w.historyStatus(ctx, func(db *sql.DB) error {
		ts := at.UTC().Format("2006-01-02 15:04:05")
		_, err := db.ExecContext(ctx, `
			INSERT INTO wa_history_sync (id, started_at, last_chunk_at, progress, inserted, done)
			VALUES (1, :ts, :ts, :p, :n, :p >= 100)
			ON CONFLICT (id) DO UPDATE SET
				last_chunk_at = :ts,
				progress      = CASE WHEN :p > 0 THEN :p ELSE progress END,
				inserted      = inserted + :n,
				done          = (CASE WHEN :p > 0 THEN :p ELSE progress END) >= 100`,
			sql.Named("ts", ts), sql.Named("p", int64(progress)), sql.Named("n", inserted))
		return err
	})
}

func (w *Writer) historyStatus(ctx context.Context, fn func(*sql.DB) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	db, err := openRW(w.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, historySyncDDL); err != nil {
		return fmt.Errorf("create wa_history_sync: %w", err)
	}
	return fn(db)
}
