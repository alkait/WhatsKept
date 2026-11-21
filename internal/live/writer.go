package live

// The writer applies captured messages to the workspace's
// ChatStorage.sqlite, in the phone's own schema. Ported from the
// proven whatskept-live writer; everything here is shaped by what the
// DB actually contains, sampled from a real workspace rather than
// assumed:
//
//   - Incoming DM:   ZFROMJID = sender (a LID in current WhatsApp
//                    builds), ZTOJID empty, ZGROUPMEMBER null.
//   - Incoming group: ZFROMJID = the GROUP jid, ZTOJID empty, and the
//                    real sender lives in ZGROUPMEMBER -> ZWAGROUPMEMBER.
//   - Outgoing:      ZFROMJID empty, ZTOJID = chat jid.
//
// v_messages reads exactly those columns, so getting them wrong doesn't
// fail an insert — it silently renders the wrong sender. ZWAMESSAGE has
// no NOT NULL columns at all; nothing here is caught by the database.
//
// Every message PK the writer assigns is also recorded in wa_live_pk.
// That table is the durable "live wrote this" ledger with two readers:
// live's own enricher (only files whose PK is in it are live's to
// enrich) and re-import's merge-forward (enrichment rows attached to
// live PKs are NOT carried forward, because the same message returns
// from the phone under a different rowid).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	_ "modernc.org/sqlite"

	"whatskept/internal/backup"
	"whatskept/internal/views"
)

// cocoaEpoch is the Core Data reference date (2001-01-01) as a Unix
// timestamp. ZMESSAGEDATE is seconds since then.
const cocoaEpoch = 978307200

// Core Data entity IDs, read from a real workspace's Z_PRIMARYKEY.
const (
	entMessage     = 9
	entChatSession = 4
	entGroupMember = 6
)

// WhatsApp message status seen on ordinary delivered text rows.
const statusDelivered = 8

// coreDataEntity maps our table names to the Z_NAME values Core Data
// uses in Z_PRIMARYKEY. Spelled out rather than derived: the casing is
// Apple's ("WAChatSession", not "WAChatsession") and no string
// transformation gets it right.
var coreDataEntity = map[string]string{
	"ZWAMESSAGE":     "WAMessage",
	"ZWACHATSESSION": "WAChatSession",
	"ZWAGROUPMEMBER": "WAGroupMember",
	"ZWAMEDIAITEM":   "WAMediaItem",
}

type action int

const (
	actionSkip action = iota
	actionInsert
	actionEdit
	actionRevoke
	actionReaction
)

func (a action) String() string {
	switch a {
	case actionInsert:
		return "insert"
	case actionEdit:
		return "edit"
	case actionRevoke:
		return "revoke"
	case actionReaction:
		return "reaction"
	default:
		return "skip"
	}
}

// plan is what decide() extracts from an event: everything the DB work
// needs, and nothing that requires a database to compute. Keeping it
// separate is what makes the classification testable without SQLite.
type plan struct {
	Action     action
	SkipReason string

	StanzaID string // this message
	TargetID string // the message an edit/revoke/reaction refers to

	ChatJID   types.JID
	SenderJID types.JID
	SenderAlt types.JID // phone JID when the sender arrived as a LID
	IsFromMe  bool
	IsGroup   bool
	PushName  string
	Timestamp time.Time

	Text     string // the body, or a reaction's emoji ("" withdraws it)
	QuotedID string // stanza ID of a quoted message, for ZPARENTMESSAGE

	// Media is set when the message carries an attachment to store. Its
	// bytes are fetched by the writer before the transaction opens.
	Media *mediaPlan
}

// ftsText is what goes into messages_fts for this message.
//
// For a text message that is ZTEXT. For media it is the surface the
// FTS rebuild would have folded in — the caption (via
// ZWAMEDIAITEM.ZTITLE) or the document's filename (via wa_document) —
// because ZTEXT stays NULL on media rows and indexing it there would
// both diverge from the backup and be wiped by the next rebuild.
func (p plan) ftsText() string {
	if p.Media == nil {
		return p.Text
	}
	if p.Media.Caption != "" {
		return p.Media.Caption
	}
	return p.Media.FileName
}

// secretPayload is the decrypted contents of a secretEncryptedMessage.
//
// Newer WhatsApp builds don't send an edit as a plain protocol message:
// the new text arrives inside an encrypted envelope whose plaintext only
// the message-secret store can open. Decryption needs the client, so it
// happens in the caller and the result is handed to decide(), keeping
// decide() pure.
type secretPayload struct {
	Msg      *waE2E.Message
	TargetID string
	Kind     waE2E.SecretEncryptedMessage_SecretEncType
}

// messageFields lists the top-level fields actually set on a message.
// Used only in skip diagnostics: when the writer declines an event, the
// log should say what it was looking at rather than leaving us to guess
// at the protobuf shape.
func messageFields(m *waE2E.Message) string {
	if m == nil {
		return "<nil>"
	}
	var names []string
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		names = append(names, string(fd.Name()))
		return true
	})
	if len(names) == 0 {
		return "<empty>"
	}
	return strings.Join(names, ",")
}

// decide classifies an event. Pure: no DB, no network. `secret` is the
// already-decrypted payload when the event carried one, else nil.
func decide(evt *events.Message, secret *secretPayload) plan {
	info := evt.Info
	// ToNonAD strips the device suffix ("110000000001:13@lid"). The
	// phone's own DB never stores it — every ZFROMJID and ZMEMBERJID in
	// a workspace is device-less — so keeping it would make these rows
	// join against nothing. In a DM that stays invisible, because
	// v_messages falls back to the chat session's ZPARTNERNAME; in a
	// group there is no such fallback and sender_name degrades to a
	// raw JID.
	p := plan{
		StanzaID:  string(info.ID),
		ChatJID:   info.Chat.ToNonAD(),
		SenderJID: info.Sender.ToNonAD(),
		SenderAlt: info.SenderAlt.ToNonAD(),
		IsFromMe:  info.IsFromMe,
		IsGroup:   info.IsGroup,
		PushName:  info.PushName,
		Timestamp: info.Timestamp,
	}

	// Noise. Status updates, broadcast lists and channels arrive as
	// ordinary message events. Storing them would manufacture chat
	// sessions that no backup ever contained. (The global stanza-ID
	// dedup also depends on this filter: broadcast fan-outs reuse one
	// stanza ID across many chats.)
	if isNoiseJID(info.Chat) {
		p.Action, p.SkipReason = actionSkip, "noise:"+info.Chat.Server
		return p
	}

	msg := evt.Message
	if msg == nil {
		p.Action, p.SkipReason = actionSkip, "no message"
		return p
	}

	// Reactions come first: withdrawing one is flagged as a sender
	// revoke, but its target is the message reacted to, which must not
	// be tombstoned.
	if r := msg.GetReactionMessage(); r != nil {
		p.Action, p.Text, p.TargetID = actionReaction, r.GetText(), r.GetKey().GetID()
		if p.TargetID == "" {
			p.Action, p.SkipReason = actionSkip, "reaction without target"
		}
		return p
	}

	// Encrypted envelope: an edit (or a poll operation, which we
	// ignore).
	if msg.GetSecretEncryptedMessage() != nil {
		if secret == nil {
			p.Action, p.SkipReason = actionSkip, "secret payload not decrypted"
			return p
		}
		if secret.Kind != waE2E.SecretEncryptedMessage_MESSAGE_EDIT {
			p.Action, p.SkipReason = actionSkip, "secret payload: "+secret.Kind.String()
			return p
		}
		text, _ := textOf(secret.Msg)
		if text == "" {
			p.Action, p.SkipReason = actionSkip,
				"decrypted edit has no text fields="+messageFields(secret.Msg)
			return p
		}
		p.Action, p.Text, p.TargetID = actionEdit, text, secret.TargetID
		if p.TargetID == "" {
			p.Action, p.SkipReason = actionSkip, "edit without target"
		}
		return p
	}

	// Revokes. The target is in the protocol message's key, not in
	// Info.ID (which identifies the revoke itself).
	if pm := msg.GetProtocolMessage(); pm != nil && pm.GetType() == waE2E.ProtocolMessage_REVOKE {
		p.Action = actionRevoke
		p.TargetID = pm.GetKey().GetID()
		if p.TargetID == "" {
			p.Action, p.SkipReason = actionSkip, "revoke without target"
		}
		return p
	}

	// Edits. whatsmeow unwraps EditedMessage before we see it, so the
	// content below is already the NEW text; Info.Edit is the signal,
	// and the target id comes from the protocol key when present.
	if info.Edit == types.EditAttributeMessageEdit || evt.IsEdit {
		// Two shapes in the wild, both seen live:
		//   1. the new text nested in protocolMessage.editedMessage
		//      (what whatsmeow's own BuildEdit produces), and
		//   2. an encrypted envelope, handled above.
		// whatsmeow only unwraps (1) on the history-sync path, never
		// for live messages, so the nesting has to be undone here.
		text, _ := textOf(msg)
		if text == "" {
			if edited := msg.GetProtocolMessage().GetEditedMessage(); edited != nil {
				text, _ = textOf(edited)
			}
		}
		if text == "" {
			p.Action, p.SkipReason = actionSkip, "edit without text fields="+messageFields(msg)
			return p
		}
		p.Action, p.Text = actionEdit, text
		p.TargetID = msg.GetProtocolMessage().GetKey().GetID()
		if p.TargetID == "" {
			p.TargetID = p.StanzaID
		}
		return p
	}
	if info.Edit == types.EditAttributeSenderRevoke || info.Edit == types.EditAttributeAdminRevoke {
		p.Action = actionRevoke
		p.TargetID = msg.GetProtocolMessage().GetKey().GetID()
		if p.TargetID == "" {
			p.TargetID = p.StanzaID
		}
		return p
	}

	// The two-event duplicate. A group message arrives twice: once
	// carrying only the sender-key distribution (no readable content),
	// then again with the actual text, ~6ms later. Inserting on the
	// first event would store a contentless row, and the ZSTANZAID
	// dedup would then correctly reject the event that actually had the
	// text. So: only content-carrying events insert.
	text, quoted := textOf(msg)
	if text == "" {
		// An attachment is content too, even though it carries no text:
		// the row and the file are stored together, and the caption or
		// filename is what becomes searchable.
		if m := classifyMedia(msg, p.ChatJID); m != nil {
			p.Action, p.Media, p.QuotedID = actionInsert, m, quotedIDOf(msg)
			return p
		}
		p.Action, p.SkipReason = actionSkip, "no text content fields="+messageFields(msg)
		return p
	}

	p.Action, p.Text, p.QuotedID = actionInsert, text, quoted
	return p
}

// textOf pulls the body out of the two message shapes that carry text,
// plus the quoted stanza ID for replies.
func textOf(msg *waE2E.Message) (text, quotedID string) {
	if c := msg.GetConversation(); c != "" {
		return c, ""
	}
	if e := msg.GetExtendedTextMessage(); e != nil {
		// ZPARENTMESSAGE is an integer FK to Z_PK, but the wire only
		// gives the quoted message's stanza ID; resolving it is the
		// writer's job.
		return e.GetText(), e.GetContextInfo().GetStanzaID()
	}
	return "", ""
}

// isNoiseJID matches the surfaces that are messages on the wire but not
// conversations in the workspace: status updates (`…@status`, and the
// `…@lid.status` variant), broadcast lists, and channels.
func isNoiseJID(jid types.JID) bool {
	s := jid.Server
	return s == "broadcast" || s == "newsletter" ||
		s == "status" || strings.HasSuffix(s, ".status")
}

// ---------------------------------------------------------------------
// DB side
// ---------------------------------------------------------------------

// Writer applies plans to the workspace DB. It holds no connection:
// every call opens, acts and closes — the same discipline the other
// commands follow, so nothing sits on the file between messages.
type Writer struct {
	// mu serializes Apply: captured events arrive on whatsmeow's serial
	// dispatch, but sent messages are recorded from the send endpoint's
	// goroutines, and two transactions allocating MAX(Z_PK)+1 at once
	// would collide.
	mu     sync.Mutex
	dbPath string
	root   string // workspace root; media lands in .unenriched/ under it
	// self is the linked account's own JID — the author named when a
	// reply quotes one of our own messages.
	self func() types.JID
	// resolvePN maps a LID to a phone JID using whatsmeow's local
	// mapping store. Used only when the event itself didn't carry one.
	resolvePN func(context.Context, types.JID) (types.JID, bool)
	// groupSubject fetches a group's real name. Only called when a
	// group has to be created, i.e. it did not exist in the last
	// backup — rare, and worth a round trip to avoid titling the
	// thread after whoever happened to speak first.
	groupSubject func(context.Context, types.JID) (string, bool)
	// download fetches an attachment's bytes. Injected so the writer is
	// testable without a WhatsApp connection.
	download func(context.Context, downloadable) ([]byte, error)
}

type Result struct {
	Action  action
	Reason  string
	PK      int64
	ChatPK  int64
	NewChat bool

	// MediaPath is the stored attachment, relative to the workspace
	// root (".unenriched/voice/48213.opus") — set only on an insert
	// that carried one.
	MediaPath string

	// Staged file, renamed into place by Apply once the transaction
	// commits. Writing it inside the transaction and publishing it
	// after is what keeps a rolled-back insert from leaving a file
	// behind, and a committed one from lacking its bytes.
	mediaTmp   string
	mediaFinal string
}

func openRW(path string) (*sql.DB, error) {
	// busy_timeout: the MCP server may hold read transactions on the
	// same file; a write should wait, not fail.
	return sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
}

// EnsureReady proves the DB is a writable ChatStorage and creates the
// wa_live_pk ledger and the reaction table, which a workspace imported
// by an older version doesn't have. Called once at startup so failure is loud and
// immediate rather than appearing on the first message that matters.
func (w *Writer) EnsureReady(ctx context.Context) error {
	db, err := openRW(w.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ZWAMESSAGE'`).Scan(&n); err != nil {
		return fmt.Errorf("inspect %s: %w", w.dbPath, err)
	}
	if n == 0 {
		return fmt.Errorf("%s has no ZWAMESSAGE table — not a ChatStorage database", w.dbPath)
	}
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS wa_live_pk (pk INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create wa_live_pk (is the database writable?): %w", err)
	}
	n, err = views.EnsureReactions(db)
	if err != nil {
		return fmt.Errorf("prepare reactions: %w", err)
	}
	if n > 0 {
		logf("reactions: %d backfilled from the imported history", n)
	}
	return nil
}

// Apply runs one plan in a single transaction.
func (w *Writer) Apply(ctx context.Context, p plan) (Result, error) {
	if p.Action == actionSkip {
		return Result{Action: actionSkip, Reason: p.SkipReason}, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	// Fetch attachment bytes BEFORE the transaction opens. WhatsApp's
	// CDN blobs expire, so this has to happen on receipt — but a network
	// round trip inside a transaction would hold the write lock for its
	// whole duration, against a database the MCP server is reading.
	//
	// A download that fails is a skip, not an error: the message is on
	// the phone and the next backup brings the row and the file together,
	// which is strictly better than a row pointing at nothing.
	if p.Media != nil && p.Media.CopyFrom == 0 && !p.Media.RowOnly {
		if w.download == nil {
			return Result{Action: actionSkip, Reason: "media capture disabled"}, nil
		}
		data, err := w.download(ctx, p.Media.Source)
		if err != nil {
			return Result{Action: actionSkip, Reason: "download failed: " + err.Error()}, nil
		}
		if len(data) == 0 {
			return Result{Action: actionSkip, Reason: "download returned no bytes"}, nil
		}
		p.Media.Data = data
		// Trust the bytes over the sender's declared length.
		p.Media.Size = uint64(len(data))
	}

	db, err := openRW(w.dbPath)
	if err != nil {
		return Result{}, err
	}
	defer db.Close()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()

	var res Result
	switch p.Action {
	case actionRevoke:
		res, err = w.applyRevoke(ctx, tx, p)
	case actionEdit:
		res, err = applyEdit(ctx, tx, p)
	case actionReaction:
		res, err = applyReaction(ctx, tx, p)
	default:
		res, err = w.applyInsert(ctx, tx, p)
	}
	if err != nil {
		// A staged file whose transaction never committed is litter.
		if res.mediaTmp != "" {
			os.Remove(res.mediaTmp)
		}
		return Result{}, err
	}
	if err := tx.Commit(); err != nil {
		if res.mediaTmp != "" {
			os.Remove(res.mediaTmp)
		}
		return Result{}, err
	}

	// Publish the staged file. The row is already committed, so a
	// failure here leaves a message whose attachment is missing — loud,
	// and healed by the next backup, but never silent.
	if res.mediaTmp != "" {
		if err := os.Rename(res.mediaTmp, res.mediaFinal); err != nil {
			os.Remove(res.mediaTmp)
			return res, fmt.Errorf("publish media file %s: %w", res.mediaFinal, err)
		}
	}
	// A revoke removes the file, and only after the row says deleted:
	// the reverse order would destroy the bytes for a transaction that
	// then rolled back, leaving a live row pointing at nothing.
	if res.Action == actionRevoke && res.mediaFinal != "" {
		if err := os.Remove(res.mediaFinal); err != nil && !os.IsNotExist(err) {
			return res, fmt.Errorf("remove revoked media %s: %w", res.mediaFinal, err)
		}
	}
	return res, nil
}

func (w *Writer) applyInsert(ctx context.Context, tx *sql.Tx, p plan) (Result, error) {
	// Dedup. The key is exact: ZSTANZAID is 100% populated in real
	// workspaces and identical to whatsmeow's message ID. Global rather
	// than per-chat is safe only because isNoiseJID filtered broadcast
	// fan-outs already.
	if pk, ok, err := findByStanza(ctx, tx, p.StanzaID); err != nil {
		return Result{}, err
	} else if ok {
		return Result{Action: actionSkip, Reason: "already present", PK: pk}, nil
	}

	chatPK, newChat, err := w.resolveChat(ctx, tx, p)
	if err != nil {
		return Result{}, fmt.Errorf("resolve chat: %w", err)
	}

	var memberPK sql.NullInt64
	if p.IsGroup && !p.IsFromMe {
		pk, err := resolveGroupMember(ctx, tx, chatPK, p.SenderJID)
		if err != nil {
			return Result{}, fmt.Errorf("resolve group member: %w", err)
		}
		memberPK = sql.NullInt64{Int64: pk, Valid: true}
	}

	// Reply target, if we have the quoted message at all. A quote of
	// something older than the backup simply won't resolve; that's a
	// null FK, not an error.
	var parent sql.NullInt64
	if p.QuotedID != "" {
		if pk, ok, err := findByStanza(ctx, tx, p.QuotedID); err != nil {
			return Result{}, err
		} else if ok {
			parent = sql.NullInt64{Int64: pk, Valid: true}
		}
	}

	pk, err := nextPK(ctx, tx, "ZWAMESSAGE")
	if err != nil {
		return Result{}, err
	}

	// Column values mirror real rows — see the file header. Getting
	// fromJID/toJID backwards renders the wrong sender in v_messages
	// without failing anything.
	var fromJID, toJID string
	switch {
	case p.IsFromMe:
		toJID = p.ChatJID.String()
	case p.IsGroup:
		fromJID = p.ChatJID.String()
	default:
		fromJID = p.SenderJID.String()
	}

	cocoa := float64(p.Timestamp.Unix() - cocoaEpoch)
	isFromMe := 0
	if p.IsFromMe {
		isFromMe = 1
	}

	// ZMESSAGETYPE 0 is text; media carries its own. ZTEXT stays NULL
	// for media — in a sampled real workspace not one image row has it
	// set.
	msgType := 0
	if p.Media != nil {
		msgType = p.Media.MessageType
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ZWAMESSAGE
		    (Z_PK, Z_ENT, Z_OPT, ZCHATSESSION, ZGROUPMEMBER, ZISFROMME,
		     ZMESSAGETYPE, ZMESSAGESTATUS, ZMESSAGEDATE, ZSENTDATE,
		     ZFROMJID, ZTOJID, ZSTANZAID, ZTEXT, ZPARENTMESSAGE)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pk, entMessage, chatPK, memberPK, isFromMe,
		msgType, statusDelivered, cocoa, cocoa,
		nullIfEmpty(fromJID), nullIfEmpty(toJID), p.StanzaID,
		nullIfEmpty(p.Text), parent,
	); err != nil {
		return Result{}, fmt.Errorf("insert message: %w", err)
	}

	// The live ledger — see the file header for its two readers.
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO wa_live_pk (pk) VALUES (?)`, pk); err != nil {
		return Result{}, fmt.Errorf("record live pk: %w", err)
	}

	res := Result{Action: actionInsert, PK: pk, ChatPK: chatPK, NewChat: newChat}

	// The media item is where the file actually lives as far as the
	// database is concerned, and it is not optional: without its
	// ZMEDIALOCALPATH extension the enrichment queue selection cannot
	// see the attachment at all.
	if p.Media != nil {
		if err := insertMediaItem(ctx, tx, pk, p.Media); err != nil {
			return Result{}, err
		}
		switch {
		case p.Media.RowOnly:
		case p.Media.CopyFrom == 0:
			tmp, final, rel, err := w.stageMediaFile(pk, p.Media)
			if err != nil {
				return Result{}, err
			}
			res.mediaTmp, res.mediaFinal, res.MediaPath = tmp, final, rel
		default:
			if err := copyDerivedText(ctx, tx, p.Media.CopyFrom, pk); err != nil {
				return Result{}, err
			}
		}
	}

	// FTS shares the rowid with ZWAMESSAGE.Z_PK — that's the contract
	// v_messages and the search tool rely on. A forward's copied text
	// is folded in the way enrichment folds its own.
	if p.Media != nil && p.Media.CopyFrom != 0 {
		if err := refreshFTSRow(tx, pk); err != nil {
			return Result{}, err
		}
	} else if err := ftsInsert(ctx, tx, pk, p.ftsText()); err != nil {
		return Result{}, err
	}

	// Chat metadata. v_chats reads these; leaving them stale makes chat
	// lists show the wrong "last message".
	if _, err := tx.ExecContext(ctx, `
		UPDATE ZWACHATSESSION
		   SET ZMESSAGECOUNTER  = COALESCE(ZMESSAGECOUNTER, 0) + 1,
		       ZLASTMESSAGE     = ?,
		       ZLASTMESSAGEDATE = ?,
		       ZLASTMESSAGETEXT = ?
		 WHERE Z_PK = ?`, pk, cocoa, nullIfEmpty(p.ftsText()), chatPK); err != nil {
		return Result{}, fmt.Errorf("update session: %w", err)
	}

	return res, nil
}

// insertMediaItem writes the ZWAMEDIAITEM row that ties a message to its
// file. Column choice mirrors what the phone stores, because the rest
// of whatskept reads exactly these: ZMEDIALOCALPATH (its extension
// selects candidates in blob/queue selection), ZAUTHORNAME (views.sql
// builds wa_document from it), ZTITLE (folded into messages_fts by the
// FTS rebuild), plus size and duration — and the download details and
// mimetype a forward reads (ZMEDIAKEY, ZMETADATA, ZVCARDSTRING).
func insertMediaItem(ctx context.Context, tx *sql.Tx, msgPK int64, m *mediaPlan) error {
	pk, err := nextPK(ctx, tx, "ZWAMEDIAITEM")
	if err != nil {
		return err
	}
	key, meta := encodeStoredMedia(storedMedia{
		MediaKey: m.Source.GetMediaKey(), FileEncSHA256: m.Source.GetFileEncSHA256(),
		KeyTimestamp: m.KeyTime, DirectPath: m.Source.GetDirectPath(),
	})
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ZWAMEDIAITEM
		    (Z_PK, Z_ENT, Z_OPT, ZMESSAGE, ZMEDIALOCALPATH,
		     ZFILESIZE, ZMOVIEDURATION, ZAUTHORNAME, ZTITLE,
		     ZMEDIAKEY, ZMETADATA, ZVCARDSTRING)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pk, entMediaItem, msgPK, m.LocalPath,
		m.Size, m.Duration, nullIfEmpty(m.FileName), nullIfEmpty(m.Caption),
		key, meta, nullIfEmpty(m.Mimetype),
	); err != nil {
		return fmt.Errorf("insert media item: %w", err)
	}
	return nil
}

// stageMediaFile writes the downloaded bytes to a temporary file beside
// their final destination in the enrichment queue, returning the temp
// path, the final path, and the workspace-relative name for logging.
// The caller renames it into place once the transaction commits — same
// directory, so the rename is atomic rather than a copy.
func (w *Writer) stageMediaFile(pk int64, m *mediaPlan) (tmp, final, rel string, err error) {
	dir := filepath.Join(w.root, backup.UnenrichedDir, m.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", "", fmt.Errorf("create %s dir: %w", m.Dir, err)
	}
	// <Z_PK><ext> — the naming import's extractors write and the
	// enrichment queue reads back.
	name := fmt.Sprintf("%d%s", pk, m.Ext)
	final = filepath.Join(dir, name)
	tmp = filepath.Join(dir, "."+name+".part")
	if err := os.WriteFile(tmp, m.Data, 0o644); err != nil {
		return "", "", "", fmt.Errorf("stage %s: %w", name, err)
	}
	return tmp, final, filepath.Join(backup.UnenrichedDir, m.Dir, name), nil
}

// applyEdit rewrites the text of an existing row. The edit may target a
// message that came from the phone's own backup — that's the normal
// case and is exactly what we want.
func applyEdit(ctx context.Context, tx *sql.Tx, p plan) (Result, error) {
	pk, ok, err := findByStanza(ctx, tx, p.TargetID)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{Action: actionSkip, Reason: "edit target not present"}, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE ZWAMESSAGE SET ZTEXT = ? WHERE Z_PK = ?`, p.Text, pk); err != nil {
		return Result{}, err
	}
	if err := ftsReplace(ctx, tx, pk, p.Text); err != nil {
		return Result{}, err
	}
	return Result{Action: actionEdit, PK: pk}, nil
}

// applyReaction records, replaces or withdraws one person's reaction.
// The target needn't be stored: reactions are keyed by stanza ID, so
// one that arrives first simply attaches when its message does.
//
// The phone's blobs name a person by phone JID or LID, and live may see
// the other one, so the row is replaced under either name. The user's
// own reactions have no sender, matching the blobs.
func applyReaction(ctx context.Context, tx *sql.Tx, p plan) (Result, error) {
	sender, fromMe := p.SenderJID.String(), 0
	if p.IsFromMe {
		sender, fromMe = "", 1
	}
	// Without a second name, alt repeats the first: an empty string here
	// would match the user's own row.
	alt := p.SenderAlt.String()
	if alt == "" {
		alt = sender
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM wa_reaction WHERE target_stanza = ? AND sender_jid IN (?, ?)`,
		p.TargetID, sender, alt); err != nil {
		return Result{}, err
	}
	if p.Text != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO wa_reaction (target_stanza, sender_jid, is_from_me, emoji, ts)
			 VALUES (?, ?, ?, ?, ?)`,
			p.TargetID, sender, fromMe, p.Text,
			p.Timestamp.UTC().Format("2006-01-02 15:04:05")); err != nil {
			return Result{}, err
		}
	}
	return Result{Action: actionReaction}, nil
}

// applyRevoke marks a message deleted the same way the phone's own DB
// does: ZMESSAGETYPE 14, which v_messages renders as 'deleted'.
//
// For an attachment, the row is only half of it. The phone deletes the
// media too, so a backup never carries a revoked message's file — and
// more to the point, "I deleted that" has to mean the image or audio
// goes, not just the text. So this also drops the captured file and any
// description, transcript or extracted text already derived from it.
func (w *Writer) applyRevoke(ctx context.Context, tx *sql.Tx, p plan) (Result, error) {
	pk, ok, err := findByStanza(ctx, tx, p.TargetID)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{Action: actionSkip, Reason: "revoke target not present"}, nil
	}

	// Read the type BEFORE overwriting it — 14 tells us nothing about
	// where the file was stored.
	var msgType int
	var localPath sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT m.ZMESSAGETYPE, mi.ZMEDIALOCALPATH
		FROM   ZWAMESSAGE m
		LEFT JOIN ZWAMEDIAITEM mi ON mi.ZMESSAGE = m.Z_PK
		WHERE  m.Z_PK = ?`, pk).Scan(&msgType, &localPath); err != nil {
		return Result{}, err
	}
	rel := mediaPathFor(pk, msgType, localPath.String)

	if _, err := tx.ExecContext(ctx,
		`UPDATE ZWAMESSAGE SET ZMESSAGETYPE = 14, ZTEXT = NULL WHERE Z_PK = ?`, pk); err != nil {
		return Result{}, err
	}
	if rel != "" {
		if err := forgetDerivedText(ctx, tx, pk); err != nil {
			return Result{}, err
		}
	}
	if err := ftsReplace(ctx, tx, pk, ""); err != nil {
		return Result{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM wa_reaction WHERE target_stanza = ?`, p.TargetID); err != nil {
		return Result{}, err
	}

	res := Result{Action: actionRevoke, PK: pk}
	if rel != "" {
		res.MediaPath = rel
		res.mediaFinal = filepath.Join(w.root, rel)
	}
	return res, nil
}

// copyDerivedText gives message `to` a copy of whatever enrichment
// produced for message `from` — a forward is the same image, voice note
// or document, so paying to describe it again would buy the same text.
// Columns are read from the table itself, so this follows the schema.
func copyDerivedText(ctx context.Context, tx *sql.Tx, from, to int64) error {
	for _, t := range []string{"wa_image_text", "wa_voice_text", "wa_document_text"} {
		rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, t)
		if err != nil {
			return err
		}
		var cols []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				return err
			}
			if c != "rowid" {
				cols = append(cols, c)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(cols) == 0 { // the table doesn't exist
			continue
		}
		list := strings.Join(cols, ", ")
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO `+t+` (rowid, `+list+`)
			SELECT ?, `+list+` FROM `+t+` WHERE rowid = ?`, to, from); err != nil {
			return fmt.Errorf("copy %s: %w", t, err)
		}
	}
	return nil
}

// forgetDerivedText removes anything an enrichment pass produced for a
// message. Each table is optional — a workspace that never enriched has
// none of them — so absence is skipped rather than treated as an error.
func forgetDerivedText(ctx context.Context, tx *sql.Tx, pk int64) error {
	for _, t := range []string{"wa_image_text", "wa_voice_text", "wa_document_text"} {
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, t).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE rowid = ?`, pk); err != nil {
			return fmt.Errorf("clear %s: %w", t, err)
		}
	}
	return nil
}

// resolveChat is the one that silently forks threads if done wrong.
//
// The event labels the chat with a LID; ZWACHATSESSION keys DMs on the
// PHONE jid (the vast majority) with the LID in ZCONTACTIDENTIFIER —
// except for a minority keyed on the LID itself. So three candidate
// columns, tried in priority order.
//
// Binding an empty string instead of NULL is the trap inside the trap:
// ZCONTACTIDENTIFIER = ” matches every group session, so a LID-less DM
// would land in a random group.
func (w *Writer) resolveChat(ctx context.Context, tx *sql.Tx, p plan) (int64, bool, error) {
	// Groups are stored verbatim; none of the LID dance applies.
	if p.IsGroup {
		pk, ok, err := lookupChat(ctx, tx, nullJID(p.ChatJID), sql.NullString{})
		if err != nil {
			return 0, false, err
		}
		if ok {
			return pk, false, nil
		}
		// v_chats titles a group from ZPARTNERNAME. The push name here
		// belongs to whoever sent this message — for our own messages,
		// us — so using it would name the group after a person. Ask
		// WhatsApp for the real subject instead.
		var subject string
		if w.groupSubject != nil {
			if name, ok := w.groupSubject(ctx, p.ChatJID); ok {
				subject = name
			}
		}
		pk, err = createChat(ctx, tx, p.ChatJID.String(), "", subject, 1)
		return pk, true, err
	}

	// For a DM the "other side" is the chat JID; the sender's alt
	// address is the same person's phone JID when the chat arrived as a
	// LID. For our own outgoing messages SenderAlt describes us, not
	// the partner, so it must not be used as the partner's phone JID.
	var phone, lid types.JID
	if p.ChatJID.Server == types.HiddenUserServer {
		lid = p.ChatJID
		if !p.IsFromMe && !p.SenderAlt.IsEmpty() &&
			p.SenderAlt.Server == types.DefaultUserServer {
			phone = p.SenderAlt
		}
	} else {
		phone = p.ChatJID
	}

	// Fall back to whatsmeow's local LID↔phone map when the event
	// didn't carry the phone JID (our own sent messages never do).
	if phone.IsEmpty() && !lid.IsEmpty() && w.resolvePN != nil {
		if pn, ok := w.resolvePN(ctx, lid); ok {
			phone = pn
		}
	}

	pk, ok, err := lookupChat(ctx, tx, nullJID(phone), nullJID(lid))
	if err != nil {
		return 0, false, err
	}
	if ok {
		return pk, false, nil
	}

	// Genuinely new: key it the way the majority of rows are keyed.
	key := phone
	if key.IsEmpty() {
		key = lid
	}
	// Same trap as groups: on an OUTGOING message the push name is our
	// own, so a new thread would be titled after us. Only an incoming
	// message tells us anything about the other party. Left NULL,
	// v_chats falls back to the push-name tables and then the JID.
	partner := ""
	if !p.IsFromMe {
		partner = p.PushName
	}
	pk, err = createChat(ctx, tx, key.String(), lid.String(), partner, 0)
	return pk, true, err
}

func lookupChat(ctx context.Context, tx *sql.Tx, phone, lid sql.NullString) (int64, bool, error) {
	var pk int64
	err := tx.QueryRowContext(ctx, `
		SELECT Z_PK FROM ZWACHATSESSION
		 WHERE (:phone IS NOT NULL AND ZCONTACTJID = :phone)
		    OR (:lid   IS NOT NULL AND ZCONTACTIDENTIFIER = :lid)
		    OR (:lid   IS NOT NULL AND ZCONTACTJID = :lid)
		 ORDER BY CASE
		     WHEN :phone IS NOT NULL AND ZCONTACTJID        = :phone THEN 1
		     WHEN :lid   IS NOT NULL AND ZCONTACTIDENTIFIER = :lid   THEN 2
		     ELSE 3
		 END
		 LIMIT 1`,
		sql.Named("phone", phone), sql.Named("lid", lid)).Scan(&pk)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return pk, err == nil, err
}

func createChat(ctx context.Context, tx *sql.Tx, contactJID, identifier, name string, sessionType int) (int64, error) {
	pk, err := nextPK(ctx, tx, "ZWACHATSESSION")
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO ZWACHATSESSION
		    (Z_PK, Z_ENT, Z_OPT, ZSESSIONTYPE, ZCONTACTJID,
		     ZCONTACTIDENTIFIER, ZPARTNERNAME, ZMESSAGECOUNTER)
		VALUES (?, ?, 1, ?, ?, ?, ?, 0)`,
		pk, entChatSession, sessionType, contactJID,
		nullIfEmpty(identifier), nullIfEmpty(name))
	return pk, err
}

// resolveGroupMember finds or creates the ZWAGROUPMEMBER row that
// v_messages joins for group sender names. Members are keyed by LID in
// real workspaces, which is what the event hands us directly.
func resolveGroupMember(ctx context.Context, tx *sql.Tx, chatPK int64, sender types.JID) (int64, error) {
	var pk int64
	err := tx.QueryRowContext(ctx,
		`SELECT Z_PK FROM ZWAGROUPMEMBER WHERE ZCHATSESSION = ? AND ZMEMBERJID = ? LIMIT 1`,
		chatPK, sender.String()).Scan(&pk)
	if err == nil {
		return pk, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if pk, err = nextPK(ctx, tx, "ZWAGROUPMEMBER"); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO ZWAGROUPMEMBER (Z_PK, Z_ENT, Z_OPT, ZCHATSESSION, ZISACTIVE, ZMEMBERJID)
		VALUES (?, ?, 1, ?, 1, ?)`, pk, entGroupMember, chatPK, sender.String())
	return pk, err
}

// quote builds the reply context for answering the message with the
// given stanza ID in chat `to`. The target must be a message we hold,
// in that chat and not deleted: a reply is addressed by stanza ID, and
// one that names the wrong chat would put a quote of one conversation
// in front of another.
//
// The quoted body is what the recipient's client shows above the reply:
// the target's text, or for an attachment its caption or filename.
func (w *Writer) quote(ctx context.Context, to types.JID, stanzaID string) (*waE2E.ContextInfo, error) {
	db, err := openRW(w.dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var chatPK sql.NullInt64
	var msgType, fromMe int
	var sender, body string
	err = tx.QueryRowContext(ctx, `
		SELECT m.ZCHATSESSION, COALESCE(m.ZMESSAGETYPE, 0), COALESCE(m.ZISFROMME, 0),
		       COALESCE(gm.ZMEMBERJID, m.ZFROMJID, ''),
		       COALESCE(NULLIF(m.ZTEXT, ''),
		                (SELECT COALESCE(NULLIF(mi.ZTITLE, ''), mi.ZAUTHORNAME)
		                   FROM ZWAMEDIAITEM mi WHERE mi.ZMESSAGE = m.Z_PK LIMIT 1),
		                '')
		FROM   ZWAMESSAGE m
		LEFT JOIN ZWAGROUPMEMBER gm ON gm.Z_PK = m.ZGROUPMEMBER
		WHERE  m.ZSTANZAID = ? LIMIT 1`, stanzaID).Scan(&chatPK, &msgType, &fromMe, &sender, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no message with stanza ID %q in the database", stanzaID)
	}
	if err != nil {
		return nil, err
	}
	if msgType == 14 {
		return nil, fmt.Errorf("message %s was deleted", stanzaID)
	}

	// The same phone/LID candidates resolveChat tries for a DM.
	var phone, lid types.JID
	if to.Server == types.HiddenUserServer {
		lid = to
		if w.resolvePN != nil {
			if pn, ok := w.resolvePN(ctx, lid); ok {
				phone = pn
			}
		}
	} else {
		phone = to
	}
	toPK, ok, err := lookupChat(ctx, tx, nullJID(phone), nullJID(lid))
	if err != nil {
		return nil, err
	}
	if !ok || !chatPK.Valid || toPK != chatPK.Int64 {
		return nil, fmt.Errorf("message %s is not in chat %s", stanzaID, to)
	}

	if fromMe != 0 {
		sender = ""
		if w.self != nil {
			sender = w.self().String()
		}
	}
	return &waE2E.ContextInfo{
		StanzaID:      proto.String(stanzaID),
		Participant:   proto.String(sender),
		QuotedMessage: &waE2E.Message{Conversation: proto.String(body)},
	}, nil
}

func findByStanza(ctx context.Context, tx *sql.Tx, stanzaID string) (int64, bool, error) {
	if stanzaID == "" {
		return 0, false, nil
	}
	var pk int64
	err := tx.QueryRowContext(ctx,
		`SELECT Z_PK FROM ZWAMESSAGE WHERE ZSTANZAID = ? LIMIT 1`, stanzaID).Scan(&pk)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return pk, err == nil, err
}

// nextPK allocates the next primary key, and keeps Core Data's own
// Z_PRIMARYKEY counter in step so the table stays internally consistent.
// Sequential, not negative: our numbering and the phone's never coexist,
// because the phone's only ever arrive via a full file replacement.
func nextPK(ctx context.Context, tx *sql.Tx, table string) (int64, error) {
	var maxPK sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT MAX(Z_PK) FROM %s`, table)).Scan(&maxPK); err != nil {
		return 0, err
	}
	next := maxPK.Int64 + 1

	if name, ok := coreDataEntity[table]; ok {
		_, _ = tx.ExecContext(ctx,
			`UPDATE Z_PRIMARYKEY SET Z_MAX = ? WHERE Z_NAME = ? AND Z_MAX < ?`,
			next, name, next)
	}
	return next, nil
}

func ftsInsert(ctx context.Context, tx *sql.Tx, rowid int64, text string) error {
	// A message with no indexable surface — a voice note with no
	// caption, say — gets no FTS row at all. The rebuild filters the
	// same way, so an empty entry here would be a row the next rebuild
	// removes.
	if text == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO messages_fts(rowid, text) VALUES (?, ?)`, rowid, text)
	return err
}

// ftsReplace rewrites an FTS row. fts5 external-content tables need the
// delete-then-insert dance; this one is a plain fts5 table, so a DELETE
// by rowid followed by INSERT is correct and simple.
func ftsReplace(ctx context.Context, tx *sql.Tx, rowid int64, text string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages_fts WHERE rowid = ?`, rowid); err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	return ftsInsert(ctx, tx, rowid, text)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullJID(j types.JID) sql.NullString {
	if j.IsEmpty() {
		return sql.NullString{}
	}
	return sql.NullString{String: j.String(), Valid: true}
}
