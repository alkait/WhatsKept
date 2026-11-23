package live

// Read state. WhatsApp tells the user's devices what has been read in
// two ways, and live records both in wa_chat_read — the table behind
// v_chats.unread:
//
//   - Reading a chat: the phone sends read receipts for the messages
//     to the user's own devices — `read-self` when read receipts are
//     off in privacy settings. Per message; most reading arrives so.
//   - "Mark as read" / "Mark as unread": an app-state change, like
//     archiving, for the whole chat. The only way a chat turns unread.
//
// `whatskept read` makes the second kind of change, synced to the
// user's own devices and seen by nobody else: no read receipts go out,
// so senders get no blue ticks. Every change carries the time it was
// made, and the table keeps whichever is newest, so the order the two
// kinds arrive in — after a reconnect they come on separate channels —
// does not matter.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"whatskept/internal/views"
)

// MarkedRead describes a chat after a read change.
type MarkedRead struct {
	Chat string `json:"chat" jsonschema:"the chat JID the change was made for, as WhatsApp addresses it"`
	Read bool   `json:"read" jsonschema:"true if the chat was marked read, false if marked unread"`
	// Warning is set when WhatsApp took the change but the database was
	// not updated. The change stands; live corrects the row when
	// WhatsApp hands the change back.
	Warning string `json:"warning,omitempty" jsonschema:"set when the change was made on WhatsApp but not recorded in the database"`
}

// readChange is a change to a chat's read state that WhatsApp told
// live about.
type readChange struct {
	Chat types.JID
	Read bool      // false: marked unread
	At   time.Time // when it was made
	Via  string    // "receipt" or "mark", for the log
}

// readChangeOf picks the read-state changes out of WhatsApp's events:
// the user's own read receipts and their "Mark as read/unread".
// Receipts from other people (blue ticks on the user's messages) and
// ones without a time are not.
func readChangeOf(evt any) (readChange, bool) {
	switch v := evt.(type) {
	case *events.Receipt:
		if !v.IsFromMe || (v.Type != types.ReceiptTypeRead && v.Type != types.ReceiptTypeReadSelf) ||
			v.Timestamp.IsZero() || isNoiseJID(v.Chat) {
			return readChange{}, false
		}
		return readChange{Chat: v.Chat.ToNonAD(), Read: true, At: v.Timestamp, Via: "receipt"}, true
	case *events.MarkChatAsRead:
		if v.Timestamp.IsZero() || isNoiseJID(v.JID) {
			return readChange{}, false
		}
		return readChange{Chat: v.JID.ToNonAD(), Read: v.Action.GetRead(), At: v.Timestamp, Via: "mark"}, true
	}
	return readChange{}, false
}

// SetRead records a read change for chat, which may be named by its
// phone JID or its LID. A chat that is not in the database is left
// alone and reported with ok false.
func (w *Writer) SetRead(ctx context.Context, chat types.JID, read bool, at time.Time) (pk int64, ok bool, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	db, err := openRW(w.dbPath)
	if err != nil {
		return 0, false, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	pk, ok, err = w.findChat(ctx, tx, chat)
	if err != nil || !ok {
		return 0, false, err
	}
	if err := markRead(ctx, tx, pk, read, at); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return pk, true, nil
}

// recordRead stores the read change live just made for the chat at pk.
func (w *Writer) recordRead(ctx context.Context, pk int64, read bool, at time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	db, err := openRW(w.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := markRead(ctx, tx, pk, read, at); err != nil {
		return err
	}
	return tx.Commit()
}

// markRead applies one change. A read moves read_through forward —
// never back — and clears a mark-unread made at or before it. A
// mark-unread counts only when no read is newer.
func markRead(ctx context.Context, tx *sql.Tx, pk int64, read bool, at time.Time) error {
	q := `INSERT INTO wa_chat_read (chat_pk, read_through, unread_marked) VALUES (:pk, :at, NULL)
		ON CONFLICT (chat_pk) DO UPDATE SET
		    read_through  = MAX(COALESCE(read_through, :at), :at),
		    unread_marked = CASE WHEN unread_marked <= :at THEN NULL ELSE unread_marked END`
	if !read {
		q = `INSERT INTO wa_chat_read (chat_pk, read_through, unread_marked) VALUES (:pk, NULL, :at)
		ON CONFLICT (chat_pk) DO UPDATE SET
		    unread_marked = CASE WHEN COALESCE(read_through, 0) >= :at THEN unread_marked
		                         ELSE MAX(COALESCE(unread_marked, :at), :at) END`
	}
	if _, err := tx.ExecContext(ctx, q, sql.Named("pk", pk), sql.Named("at", views.Cocoa(at))); err != nil {
		return fmt.Errorf("record read state: %w", err)
	}
	return nil
}
