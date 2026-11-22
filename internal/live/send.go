package live

// Sending. The linked session allows one connection and `whatskept
// live` owns it, so nothing else can put a message on the wire: `send`
// and the MCP send tool hand the text to the running live process over
// a loopback HTTP endpoint — as do `archive` and `read` and their MCP
// tools, with an archive or read change. The address and a random token are
// advertised in .whatskept/live.json while live runs — the token is
// the credential, because any local process can reach a loopback port.
//
// WhatsApp never echoes a device's own sends back to it, so a sent
// message is recorded here, through the same decide → Apply path every
// captured message takes.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatskept/internal/backup"
	"whatskept/internal/workspace"
)

// Generous for a forward, which may wait on the phone to re-upload an
// attachment and then download it.
const sendTimeout = 60 * time.Second

// ErrNotRunning is what Send returns when no live process is listening
// in the workspace.
var ErrNotRunning = errors.New("`whatskept live` is not running in this workspace — start it, then send again")

// Sent describes a delivered message.
type Sent struct {
	ID   string `json:"id" jsonschema:"the message's stanza ID (ZSTANZAID in the database)"`
	Chat string `json:"chat" jsonschema:"the chat JID the message went to"`
	Ts   string `json:"ts" jsonschema:"send time, UTC ISO"`
	// Warning is set when the message went out but could not be written
	// to the database. Never retry on it — the recipient has the message.
	Warning string `json:"warning,omitempty" jsonschema:"set when the message was sent but not recorded in the database; do not resend"`
}

type sendRequest struct {
	To      string `json:"to"`
	Text    string `json:"text"`
	ReplyTo string `json:"reply_to,omitempty"` // stanza ID of the message to quote
	Forward string `json:"forward,omitempty"`  // stanza ID of the message to forward
}

type sendReply struct {
	Sent
	Error string `json:"error,omitempty"`
}

type archiveRequest struct {
	Chat    string `json:"chat"`
	Archive bool   `json:"archive"` // false unarchives
}

type archiveReply struct {
	Archived
	Error string `json:"error,omitempty"`
}

type readRequest struct {
	Chat string `json:"chat"`
	Read bool   `json:"read"` // false marks the chat unread
}

type readReply struct {
	MarkedRead
	Error string `json:"error,omitempty"`
}

// chatOps is what the endpoint changes a chat's app state with: find it
// in the database, put the archive or read change on the wire, record
// an archive change. The wire halves are injected so the endpoint is
// testable without a WhatsApp connection.
type chatOps struct {
	target  func(ctx context.Context, to types.JID) (archiveTarget, error)
	archive func(ctx context.Context, t archiveTarget, archive bool) error
	record  func(ctx context.Context, t archiveTarget, archive bool) error
	read    func(ctx context.Context, t archiveTarget, read bool) error
}

type endpoint struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

// deliverFunc puts a message on the wire and returns the stanza ID and
// timestamp WhatsApp assigned. Injected so the endpoint is testable
// without a WhatsApp connection.
type deliverFunc func(ctx context.Context, to types.JID, msg *waE2E.Message) (types.MessageID, time.Time, error)

// quoteFunc builds the reply context for answering the message with the
// given stanza ID in chat `to` (see Writer.quote).
type quoteFunc func(ctx context.Context, to types.JID, stanzaID string) (*waE2E.ContextInfo, error)

// ParseChat turns what a caller typed into a chat JID: a phone number
// ("+971 50 000 0001") or a full JID (…@s.whatsapp.net, …@g.us, …@lid).
func ParseChat(s string) (types.JID, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "@") {
		jid, err := types.ParseJID(s)
		if err != nil || jid.User == "" {
			return types.JID{}, fmt.Errorf("invalid chat JID %q", s)
		}
		switch jid.Server {
		case types.DefaultUserServer, types.GroupServer, types.HiddenUserServer:
			return jid.ToNonAD(), nil
		}
		return types.JID{}, fmt.Errorf("%q is not a chat — want @%s, @%s or @%s",
			s, types.DefaultUserServer, types.GroupServer, types.HiddenUserServer)
	}
	digits := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(strings.TrimPrefix(s, "+"))
	if len(digits) < 7 || len(digits) > 15 || strings.Trim(digits, "0123456789") != "" {
		return types.JID{}, fmt.Errorf("invalid chat %q — want a phone number in international format or a full JID", s)
	}
	return types.NewJID(digits, types.DefaultUserServer), nil
}

// sentEvent is the message event WhatsApp would have delivered had the
// send come from another of the user's devices.
func sentEvent(to types.JID, id types.MessageID, ts time.Time, msg *waE2E.Message) *events.Message {
	v := &events.Message{Message: msg}
	v.Info.ID = id
	v.Info.Chat = to
	v.Info.IsFromMe = true
	v.Info.IsGroup = to.Server == types.GroupServer
	v.Info.Timestamp = ts
	return v
}

// recordFunc stores a sent message. forwardOf is the Z_PK of the
// original when the message is a forward, else 0.
type recordFunc func(v *events.Message, forwardOf int64) error

// startSend serves the send endpoint for the workspace at root and
// advertises it in .whatskept/live.json. The returned stop closes the
// listener and removes the advertisement.
func startSend(root string, deliver deliverFunc, quote quoteFunc, source sourceFunc, fetch fetchFunc, record recordFunc, ops chatOps) (stop func(), err error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(raw)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for sends: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /"+token+"/send", func(rw http.ResponseWriter, r *http.Request) {
		reply := func(status int, v sendReply) { writeJSON(rw, status, v) }
		var req sendRequest
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<20)).Decode(&req); err != nil {
			reply(http.StatusBadRequest, sendReply{Error: "bad request: " + err.Error()})
			return
		}
		to, err := ParseChat(req.To)
		if err != nil {
			reply(http.StatusBadRequest, sendReply{Error: err.Error()})
			return
		}
		if req.Forward != "" && (req.Text != "" || req.ReplyTo != "") {
			reply(http.StatusBadRequest, sendReply{Error: "a forward sends the original as it is — no text or reply_to with it"})
			return
		}
		if req.Forward == "" && strings.TrimSpace(req.Text) == "" {
			reply(http.StatusBadRequest, sendReply{Error: "empty message text"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), sendTimeout)
		defer cancel()
		// A reply is the same text carrying a quote of its target — the
		// shape textOf reads back, so recording links ZPARENTMESSAGE.
		msg := &waE2E.Message{Conversation: proto.String(req.Text)}
		var forwardOf int64
		if req.Forward != "" {
			src, err := source(ctx, req.Forward)
			if err != nil {
				reply(http.StatusBadRequest, sendReply{Error: "cannot forward: " + err.Error()})
				return
			}
			var path string
			var sum []byte
			if src.Media != nil {
				if path, sum, err = fetch(ctx, src); err != nil {
					logf("forward of %s FAILED: %v", req.Forward, err)
					reply(http.StatusBadGateway, sendReply{Error: "cannot forward: " + err.Error()})
					return
				}
			}
			msg, forwardOf = forwardMessage(src, path, sum), src.PK
		} else if req.ReplyTo != "" {
			quoted, err := quote(ctx, to, req.ReplyTo)
			if err != nil {
				reply(http.StatusBadRequest, sendReply{Error: "cannot reply: " + err.Error()})
				return
			}
			msg = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String(req.Text), ContextInfo: quoted}}
		}
		id, ts, err := deliver(ctx, to, msg)
		if err != nil {
			logf("send to %s FAILED: %v", to, err)
			reply(http.StatusBadGateway, sendReply{Error: "send failed: " + err.Error()})
			return
		}
		out := sendReply{Sent: Sent{ID: string(id), Chat: to.String(), Ts: ts.UTC().Format(time.RFC3339)}}
		if err := record(sentEvent(to, id, ts, msg), forwardOf); err != nil {
			out.Warning = "sent, but not recorded in the database: " + err.Error()
		}
		reply(http.StatusOK, out)
	})
	mux.HandleFunc("POST /"+token+"/archive", func(rw http.ResponseWriter, r *http.Request) {
		reply := func(status int, v archiveReply) { writeJSON(rw, status, v) }
		var req archiveRequest
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<20)).Decode(&req); err != nil {
			reply(http.StatusBadRequest, archiveReply{Error: "bad request: " + err.Error()})
			return
		}
		verb := "unarchive"
		if req.Archive {
			verb = "archive"
		}
		to, err := ParseChat(req.Chat)
		if err != nil {
			reply(http.StatusBadRequest, archiveReply{Error: err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), sendTimeout)
		defer cancel()
		t, err := ops.target(ctx, to)
		if err != nil {
			reply(http.StatusBadRequest, archiveReply{Error: "cannot " + verb + ": " + err.Error()})
			return
		}
		if err := ops.archive(ctx, t, req.Archive); err != nil {
			logf("%s %s FAILED: %v", verb, t.JID, err)
			reply(http.StatusBadGateway, archiveReply{Error: verb + " failed: " + err.Error()})
			return
		}
		out := archiveReply{Archived: Archived{Chat: t.JID.String(), Archived: req.Archive}}
		if err := ops.record(ctx, t, req.Archive); err != nil {
			out.Warning = "changed on WhatsApp, but not recorded in the database: " + err.Error()
		}
		reply(http.StatusOK, out)
	})
	mux.HandleFunc("POST /"+token+"/read", func(rw http.ResponseWriter, r *http.Request) {
		reply := func(status int, v readReply) { writeJSON(rw, status, v) }
		var req readRequest
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<20)).Decode(&req); err != nil {
			reply(http.StatusBadRequest, readReply{Error: "bad request: " + err.Error()})
			return
		}
		verb := "mark unread"
		if req.Read {
			verb = "mark read"
		}
		to, err := ParseChat(req.Chat)
		if err != nil {
			reply(http.StatusBadRequest, readReply{Error: err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), sendTimeout)
		defer cancel()
		t, err := ops.target(ctx, to)
		if err != nil {
			reply(http.StatusBadRequest, readReply{Error: "cannot " + verb + ": " + err.Error()})
			return
		}
		if err := ops.read(ctx, t, req.Read); err != nil {
			logf("%s %s FAILED: %v", verb, t.JID, err)
			reply(http.StatusBadGateway, readReply{Error: verb + " failed: " + err.Error()})
			return
		}
		reply(http.StatusOK, readReply{MarkedRead: MarkedRead{Chat: t.JID.String(), Read: req.Read}})
	})

	data, err := json.Marshal(endpoint{Addr: ln.Addr().String(), Token: token})
	if err != nil {
		ln.Close()
		return nil, err
	}
	// Removed first: WriteFile keeps the mode of a file left by a crash.
	path := workspace.LiveEndpointPath(root)
	os.Remove(path)
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("advertise send endpoint: %w", err)
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return func() {
		srv.Close()
		os.Remove(path)
	}, nil
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	json.NewEncoder(rw).Encode(v)
}

// StartFakeSend runs the real send endpoint for root with a stand-in
// for WhatsApp: nothing reaches the wire, stanza IDs are FAKE1, FAKE2…,
// and messages and archive changes are recorded in the workspace
// database as usual. A forwarded attachment is taken as still on the
// CDN, with a fixed plaintext hash. Test support for the packages that
// drive sends from outside — the CLI and the MCP server.
func StartFakeSend(root string) (stop func(), err error) {
	w := &Writer{dbPath: filepath.Join(root, backup.ChatStorageName), root: root}
	if err := w.EnsureReady(context.Background()); err != nil {
		return nil, err
	}
	var n atomic.Int64
	return startSend(root,
		func(context.Context, types.JID, *waE2E.Message) (types.MessageID, time.Time, error) {
			return types.MessageID(fmt.Sprintf("FAKE%d", n.Add(1))), time.Now(), nil
		},
		w.quote,
		w.forwardSource,
		func(_ context.Context, src forwardSource) (string, []byte, error) {
			return src.Media.DirectPath, make([]byte, 32), nil
		},
		func(v *events.Message, forwardOf int64) error {
			_, err := w.Apply(context.Background(), forwardPlan(decide(v, nil), forwardOf))
			return err
		},
		w.chatOps(func(context.Context, archiveTarget, bool) error { return nil },
			func(context.Context, archiveTarget, bool) error { return nil }))
}

// chatOps is the endpoint's chatOps over this writer, with archive and
// read putting each change on WhatsApp.
func (w *Writer) chatOps(archive, read func(context.Context, archiveTarget, bool) error) chatOps {
	return chatOps{
		target:  w.archiveTarget,
		archive: archive,
		record: func(ctx context.Context, t archiveTarget, archive bool) error {
			return w.recordArchived(ctx, t.PK, archive)
		},
		read: read,
	}
}

// Send asks the live process running in the workspace at root to send
// text to a chat (see ParseChat for the accepted forms). A non-empty
// replyTo is the stanza ID of a message in that chat to quote.
func Send(ctx context.Context, root, to, text, replyTo string) (Sent, error) {
	r, err := post[sendReply](ctx, root, "send", sendRequest{To: to, Text: text, ReplyTo: replyTo})
	return r.Sent, err
}

// Forward asks the running live to forward the message with the given
// stanza ID, from any chat, to chat `to`.
func Forward(ctx context.Context, root, to, stanzaID string) (Sent, error) {
	r, err := post[sendReply](ctx, root, "send", sendRequest{To: to, Forward: stanzaID})
	return r.Sent, err
}

// Archive asks the running live to archive chat `to` (see ParseChat),
// or with archive false to unarchive it. The chat must be one the
// database holds.
func Archive(ctx context.Context, root, to string, archive bool) (Archived, error) {
	r, err := post[archiveReply](ctx, root, "archive", archiveRequest{Chat: to, Archive: archive})
	return r.Archived, err
}

// MarkRead asks the running live to mark chat `to` (see ParseChat) as
// read, or with read false as unread, on the user's own devices. No
// read receipts are sent. The chat must be one the database holds.
func MarkRead(ctx context.Context, root, to string, read bool) (MarkedRead, error) {
	r, err := post[readReply](ctx, root, "read", readRequest{Chat: to, Read: read})
	return r.MarkedRead, err
}

// post hands one request to the live process running in the workspace
// at root and returns its reply. R is a reply type carrying Error.
func post[R interface{ errText() string }](ctx context.Context, root, op string, r any) (R, error) {
	var zero R
	data, err := os.ReadFile(workspace.LiveEndpointPath(root))
	if err != nil {
		return zero, ErrNotRunning
	}
	var ep endpoint
	if err := json.Unmarshal(data, &ep); err != nil {
		return zero, fmt.Errorf("parse %s: %w", workspace.LiveEndpointPath(root), err)
	}
	body, err := json.Marshal(r)
	if err != nil {
		return zero, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+ep.Addr+"/"+ep.Token+"/"+op, bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: sendTimeout + 10*time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// A refused dial is a live.json left behind by a crash.
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return zero, ErrNotRunning
		}
		return zero, err
	}
	defer resp.Body.Close()
	var reply R
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		if resp.StatusCode == http.StatusNotFound {
			// A live started from a release that predates this request.
			return zero, fmt.Errorf("the running `whatskept live` does not support %s — restart it with this release", op)
		}
		return zero, fmt.Errorf("live answered %s", resp.Status)
	}
	if msg := reply.errText(); msg != "" {
		return zero, errors.New(msg)
	}
	return reply, nil
}

func (r sendReply) errText() string    { return r.Error }
func (r archiveReply) errText() string { return r.Error }
func (r readReply) errText() string    { return r.Error }
