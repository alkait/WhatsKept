package live

// Archiving. A chat's archived state is not a message: WhatsApp keeps
// it in the account's synced app state, beside pins and mutes, and
// every device applies the same mutation. live records each archive
// change it is told about in ZWACHATSESSION.ZARCHIVED — the column
// v_chats.archived reads — whichever device made it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// errNoChat is an archive request for a chat the database doesn't hold.
var errNoChat = errors.New("not in the database")

// Archived describes a chat after an archive change.
type Archived struct {
	Chat     string `json:"chat" jsonschema:"the chat JID the change was made for, as WhatsApp addresses it"`
	Archived bool   `json:"archived" jsonschema:"the chat's archived state now"`
	// Warning is set when WhatsApp took the change but the database was
	// not updated. The change stands; live corrects the row when
	// WhatsApp hands the change back.
	Warning string `json:"warning,omitempty" jsonschema:"set when the change was made on WhatsApp but not recorded in the database"`
}

// archiveTarget is a chat as an archive change addresses it.
type archiveTarget struct {
	PK  int64     // ZWACHATSESSION row
	JID types.JID // as WhatsApp addresses the chat: a DM by its LID when known
	// The chat's latest message, which WhatsApp's own clients send with
	// the change; nil (and LastTs zero) when the chat has none.
	Last   *waCommon.MessageKey
	LastTs time.Time
}

// archiveTarget finds chat `to` in the database for an archive change.
// It must be there: archiving never creates a chat.
func (w *Writer) archiveTarget(ctx context.Context, to types.JID) (archiveTarget, error) {
	db, err := openRW(w.dbPath)
	if err != nil {
		return archiveTarget{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return archiveTarget{}, err
	}
	defer tx.Rollback()

	pk, ok, err := w.findChat(ctx, tx, to)
	if err != nil {
		return archiveTarget{}, err
	}
	if !ok {
		return archiveTarget{}, fmt.Errorf("chat %s is %w", to, errNoChat)
	}
	var contact, ident sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT ZCONTACTJID, ZCONTACTIDENTIFIER FROM ZWACHATSESSION WHERE Z_PK = ?`, pk).
		Scan(&contact, &ident); err != nil {
		return archiveTarget{}, err
	}
	t := archiveTarget{PK: pk, JID: w.archiveJID(ctx, to, contact.String, ident.String)}

	var stanza, member sql.NullString
	var fromMe int
	var date sql.NullFloat64
	err = tx.QueryRowContext(ctx, `
		SELECT m.ZSTANZAID, COALESCE(m.ZISFROMME, 0), m.ZMESSAGEDATE, gm.ZMEMBERJID
		FROM   ZWAMESSAGE m
		LEFT JOIN ZWAGROUPMEMBER gm ON gm.Z_PK = m.ZGROUPMEMBER
		WHERE  m.ZCHATSESSION = ? AND m.ZSTANZAID IS NOT NULL AND m.ZSTANZAID != ''
		   AND m.ZMESSAGEDATE IS NOT NULL
		ORDER BY m.ZMESSAGEDATE DESC, m.Z_PK DESC LIMIT 1`, pk).Scan(&stanza, &fromMe, &date, &member)
	if errors.Is(err, sql.ErrNoRows) {
		return t, nil
	}
	if err != nil {
		return archiveTarget{}, err
	}
	t.LastTs = time.Unix(int64(math.Floor(date.Float64))+cocoaEpoch, 0)
	t.Last = &waCommon.MessageKey{
		RemoteJID: proto.String(t.JID.String()),
		FromMe:    proto.Bool(fromMe != 0),
		ID:        proto.String(stanza.String),
	}
	if t.JID.Server == types.GroupServer && fromMe == 0 && member.String != "" {
		t.Last.Participant = proto.String(member.String)
	}
	return t, nil
}

// archiveJID is the JID an archive change names chat `to` by. Groups
// are addressed as they are stored. WhatsApp now keys a DM's app state
// on its LID — the phone JID only for chats untouched since the move —
// so a DM goes by its LID wherever the session or whatsmeow's map
// knows one, else by its phone JID.
func (w *Writer) archiveJID(ctx context.Context, to types.JID, contact, ident string) types.JID {
	if to.Server == types.GroupServer {
		return to
	}
	for _, s := range []string{contact, ident} {
		if j, err := types.ParseJID(s); err == nil && j.Server == types.HiddenUserServer {
			return j.ToNonAD()
		}
	}
	if to.Server == types.HiddenUserServer {
		return to
	}
	phone := to
	if j, err := types.ParseJID(contact); err == nil && j.Server == types.DefaultUserServer {
		phone = j.ToNonAD()
	}
	if w.resolveLID != nil {
		if lid, ok := w.resolveLID(ctx, phone); ok {
			return lid.ToNonAD()
		}
	}
	return phone
}

// SetArchived records chat's archived state. A chat that is not in the
// database is left alone — archiving never creates one — and reported
// with ok false.
func (w *Writer) SetArchived(ctx context.Context, chat types.JID, archived bool) (pk int64, ok bool, err error) {
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
	if err := markArchived(ctx, tx, pk, archived); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return pk, true, nil
}

// recordArchived stores the state live just set for the chat at pk —
// by row, since archiveTarget already found it.
func (w *Writer) recordArchived(ctx context.Context, pk int64, archived bool) error {
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
	if err := markArchived(ctx, tx, pk, archived); err != nil {
		return err
	}
	return tx.Commit()
}

func markArchived(ctx context.Context, tx *sql.Tx, pk int64, archived bool) error {
	flag := 0
	if archived {
		flag = 1
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE ZWACHATSESSION SET ZARCHIVED = ? WHERE Z_PK = ?`, flag, pk); err != nil {
		return fmt.Errorf("record archived state: %w", err)
	}
	return nil
}
