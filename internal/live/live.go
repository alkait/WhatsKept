// Package live links to the user's WhatsApp account as a companion
// device (like WhatsApp Web) and captures new messages as they arrive,
// appending them to the workspace's ChatStorage.sqlite in the phone's
// own schema. Media lands in the .unenriched/ queue, exactly as import
// leaves it.
//
// Deliberately silent on the wire: no presence, no read receipts, no
// typing indicators. whatsmeow sends none of these unless asked, and
// nothing here asks — the user's chats are never marked read behind
// their back. The one thing live does send is a message it was
// explicitly handed (see send.go).
package live

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3" // whatsmeow's session store; our own DB code stays on modernc
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"whatskept/internal/backup"
	"whatskept/internal/workspace"
)

const heartbeatEvery = 15 * time.Minute

// Run links (or re-connects) the workspace's companion device and logs
// incoming messages until ctx is cancelled.
func Run(ctx context.Context, root string) error {
	s, err := workspace.Load(root)
	if err != nil {
		return err
	}

	// The name shown in WhatsApp → Linked Devices. Stable on purpose —
	// a changing name looks like a new device each time.
	store.SetOSInfo("whatskept", [3]uint32{0, 1, 0})

	sessionPath := workspace.SessionDBPath(root)
	dsn := fmt.Sprintf("file:%s?_foreign_keys=on&_busy_timeout=5000", sessionPath)
	container, err := sqlstore.New(ctx, "sqlite3", dsn, waLog.Stdout("store", "WARN", false))
	if err != nil {
		return fmt.Errorf("open session store %s: %w", sessionPath, err)
	}
	defer container.Close()

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("read device from session store: %w", err)
	}
	client := whatsmeow.NewClient(device, waLog.Stdout("client", "WARN", false))

	var (
		msgCount    atomic.Int64
		connects    atomic.Int64
		disconnects atomic.Int64
		loggedOut   atomic.Bool
		connectedAt atomic.Int64 // unix seconds; 0 when not connected

		inserted  atomic.Int64
		edited    atomic.Int64
		revoked   atomic.Int64
		reactions atomic.Int64
		skipped   atomic.Int64
		writeErrs atomic.Int64
	)

	// Import usually seeds the database; a workspace going straight to
	// live without a backup starts from an empty one instead. A later
	// import replaces it with the phone's full history as usual.
	dbPath := filepath.Join(root, backup.ChatStorageName)
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		if err := backup.CreateEmpty(dbPath); err != nil {
			return err
		}
		logf("no %s — created an empty one; history starts now (`whatskept import` any time adds the older messages)", backup.ChatStorageName)
	} else if err != nil {
		return err
	}
	w := &Writer{
		dbPath: dbPath,
		root:   root,
		// Consulted when an event arrives without a phone JID; whatsmeow
		// keeps this map locally, so it is a store read and not a
		// network round trip.
		self: func() types.JID {
			if id := client.Store.ID; id != nil {
				return id.ToNonAD()
			}
			return types.JID{}
		},
		resolvePN: func(ctx context.Context, lid types.JID) (types.JID, bool) {
			pn, err := client.Store.LIDs.GetPNForLID(ctx, lid)
			return pn, err == nil && !pn.IsEmpty()
		},
		groupSubject: func(ctx context.Context, jid types.JID) (string, bool) {
			info, err := client.GetGroupInfo(ctx, jid)
			if err != nil {
				logf("group subject lookup failed for %s: %v", jid, err)
				return "", false
			}
			return info.Name, info.Name != ""
		},
		// Attachments are fetched on receipt: WhatsApp's CDN blobs
		// expire, so there is no second chance at these bytes short of
		// the next backup. `downloadable` has the same method set as
		// whatsmeow.DownloadableMessage, so it passes straight through.
		download: func(ctx context.Context, msg downloadable) ([]byte, error) {
			return client.Download(ctx, msg)
		},
	}
	if err := w.EnsureReady(ctx); err != nil {
		return err
	}

	// Trickle enrichment: on only when the key is present; strictly
	// this session's captures. Starting live with the key IS the
	// consent — the backlog stays untouched regardless.
	enr, err := startEnricher(ctx, root, dbPath)
	if err != nil {
		return err
	}

	// applyMsg writes one message event and returns a log suffix saying
	// what happened, plus the write error if there was one. forwardOf
	// is the original's Z_PK when v is a forward we sent, else 0.
	applyMsg := func(v *events.Message, forwardOf int64) (string, error) {
		ctx := context.Background()
		res, err := w.Apply(ctx, forwardPlan(decide(v, decryptSecret(ctx, client, v)), forwardOf))
		if err != nil {
			writeErrs.Add(1)
			return fmt.Sprintf(" write=ERROR(%v)", err), err
		}
		switch res.Action {
		case actionInsert:
			inserted.Add(1)
			s := fmt.Sprintf(" write=insert pk=%d chat=%d", res.PK, res.ChatPK)
			if res.MediaPath != "" {
				s += " file=" + res.MediaPath
				// Enrich now rather than at the next backstop tick.
				// Non-blocking: the event handler never waits on a
				// model call.
				enr.Nudge()
			}
			if res.NewChat {
				// Worth seeing: a new session per message would mean
				// chat resolution is forking threads.
				s += " NEW-CHAT"
			}
			return s, nil
		case actionEdit:
			edited.Add(1)
			return fmt.Sprintf(" write=edit pk=%d", res.PK), nil
		case actionRevoke:
			revoked.Add(1)
			return fmt.Sprintf(" write=revoke pk=%d", res.PK), nil
		case actionReaction:
			reactions.Add(1)
			return " write=reaction", nil
		default:
			skipped.Add(1)
			return " write=skip(" + res.Reason + ")", nil
		}
	}

	// Auto-reconnect is on by default; the hook makes failures visible
	// and keeps retrying forever rather than giving up after a bad
	// network patch. A dead (logged-out) session is the one lost cause.
	client.EnableAutoReconnect = true
	client.AutoReconnectHook = func(err error) bool {
		if loggedOut.Load() {
			return false
		}
		logf("reconnect attempt failed (retry %d): %v — will keep trying",
			client.AutoReconnectErrors, err)
		return true
	}

	// Forwards may need the phone to re-upload an attachment; its answer
	// arrives as an event.
	fetcher := newMediaFetcher(client)

	client.AddEventHandler(func(evt any) {
		switch v := evt.(type) {
		case *events.MediaRetry:
			fetcher.handle(v)
		case *events.Message:
			msgCount.Add(1)
			outcome, _ := applyMsg(v, 0)
			logMessage(v, outcome)
		case *events.DeleteForMe:
			// "Delete for me" does NOT travel as a revoke — WhatsApp
			// syncs it to the user's own devices as an app-state
			// mutation, so nothing on the message path ever sees it.
			// Treated exactly like a revoke: the row is tombstoned, the
			// file and any derived text removed. The phone deletes its
			// copy entirely; a tombstone is the conservative choice for
			// a destructive action, and the next import reconciles it.
			res, err := w.Apply(context.Background(), plan{Action: actionRevoke, TargetID: string(v.MessageID)})
			if err != nil {
				writeErrs.Add(1)
				logf("delete-for-me %s: ERROR %v", v.MessageID, err)
				break
			}
			if res.Action != actionRevoke {
				// Not present is the normal case: the phone deleted it
				// long ago, so no backup ever carried it.
				break
			}
			revoked.Add(1)
			s := fmt.Sprintf("delete-for-me %s: pk=%d removed", v.MessageID, res.PK)
			if res.MediaPath != "" {
				s += " file=" + res.MediaPath
			}
			logf("%s", s)
		case *events.Connected:
			connects.Add(1)
			connectedAt.Store(time.Now().Unix())
			logf("connected as %s (connection #%d)", client.Store.ID, connects.Load())
		case *events.Disconnected:
			disconnects.Add(1)
			connectedAt.Store(0)
			logf("disconnected (#%d) — auto-reconnect will retry", disconnects.Load())
		case *events.KeepAliveTimeout:
			logf("keepalive timeout (%d in a row, last success %s ago)",
				v.ErrorCount, time.Since(v.LastSuccess).Round(time.Second))
		case *events.KeepAliveRestored:
			logf("keepalive restored")
		case *events.LoggedOut:
			loggedOut.Store(true)
			connectedAt.Store(0)
			logf("LOGGED OUT (reason=%s) — the session is dead", v.Reason)
			logf("re-pair: delete %s and run `whatskept live` again to scan a new QR", sessionPath)
		case *events.StreamReplaced:
			logf("stream replaced — another client connected with these credentials")
		case *events.TemporaryBan:
			logf("TEMPORARY BAN: %s (expires in %s)", v.Code, v.Expire)
		case *events.ClientOutdated:
			logf("server says this client is outdated — a newer whatskept release is needed")
		case *events.ConnectFailure:
			logf("connect failure: reason=%s message=%q", v.Reason, v.Message)
		case *events.PairSuccess:
			logf("paired: id=%s platform=%s", v.ID, v.Platform)
		case *events.PairError:
			logf("pair error: id=%s: %v", v.ID, v.Error)
		case *events.HistorySync:
			// History comes from backups via import; linking backfill is
			// counted for visibility but not stored.
			n := 0
			for _, conv := range v.Data.GetConversations() {
				n += len(conv.GetMessages())
			}
			logf("history sync: type=%s conversations=%d messages=%d (not stored — import owns history)",
				v.Data.GetSyncType(), len(v.Data.GetConversations()), n)
		}
	})

	if client.Store.ID == nil {
		logf("no session found — pairing required")
		if err := pair(ctx, client); err != nil {
			return err
		}
		if client.Store.ID == nil {
			return errors.New("pairing finished without a device identity")
		}
		if err := bindNumber(root, &s, client.Store.ID.User); err != nil {
			// The wrong phone scanned the QR: unlink the device from that
			// account before bailing, so no stray "whatskept" stays in
			// their Linked Devices.
			if lerr := client.Logout(ctx); lerr != nil {
				logf("could not unlink the mismatched device: %v", lerr)
			}
			return err
		}
	} else {
		// Verify the stored session before spending a connect on it.
		if err := bindNumber(root, &s, client.Store.ID.User); err != nil {
			return fmt.Errorf("%w (the session at %s belongs to a different account — delete it to re-pair)", err, sessionPath)
		}
		logf("existing session for %s — connecting", client.Store.ID)
		if err := client.Connect(); err != nil {
			return fmt.Errorf("connect: %w", err)
		}
	}

	// Sends are handed over by `whatskept send` and the MCP send tool;
	// recording goes through applyMsg like any captured message.
	stopSend, err := startSend(root,
		func(ctx context.Context, to types.JID, msg *waE2E.Message) (types.MessageID, time.Time, error) {
			resp, err := client.SendMessage(ctx, to, msg)
			return resp.ID, resp.Timestamp, err
		},
		w.quote,
		w.forwardSource,
		fetcher.fetch,
		func(v *events.Message, forwardOf int64) error {
			if id := client.Store.ID; id != nil {
				v.Info.Sender = id.ToNonAD()
			}
			outcome, err := applyMsg(v, forwardOf)
			logMessage(v, outcome)
			return err
		})
	if err != nil {
		return err
	}
	defer stopSend()

	logf("capturing into %s; media queued in %s; Ctrl-C to stop",
		backup.ChatStorageName, backup.UnenrichedDir)

	go func() {
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				state := "DISCONNECTED"
				if loggedOut.Load() {
					state = "LOGGED OUT — re-pairing needed"
				} else if since := connectedAt.Load(); since > 0 {
					state = fmt.Sprintf("connected for %s", time.Since(time.Unix(since, 0)).Round(time.Second))
				}
				logf("heartbeat: %s | messages=%d connects=%d disconnects=%d | wrote=%d edits=%d revokes=%d reactions=%d skipped=%d errors=%d | queued=%d%s",
					state, msgCount.Load(), connects.Load(), disconnects.Load(),
					inserted.Load(), edited.Load(), revoked.Load(), reactions.Load(), skipped.Load(), writeErrs.Load(),
					queueSize(root), enr.status())
			}
		}
	}()

	<-ctx.Done()
	logf("shutting down")
	client.Disconnect()
	return nil
}

// pair renders the QR code in the terminal and blocks until the phone
// scans it. GetQRChannel must be called BEFORE Connect, or whatsmeow
// refuses (the channel is fed by the pairing handshake).
func pair(ctx context.Context, client *whatsmeow.Client) error {
	qrChan, err := client.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("open QR channel: %w", err)
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	for item := range qrChan {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			fmt.Println()
			fmt.Println("── Scan with WhatsApp → Settings → Linked Devices → Link a Device ──")
			qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stdout)
			// The raw payload too: if the block rendering is mangled by
			// the terminal, it can be turned into a QR elsewhere.
			fmt.Printf("\nraw: %s\n(expires in %s)\n\n", item.Code, item.Timeout.Round(time.Second))
		case "success":
			logf("pairing succeeded")
			return nil
		case "timeout":
			return errors.New("QR expired before it was scanned — run `whatskept live` again for a fresh code")
		case "err-client-outdated":
			return errors.New("WhatsApp rejected this client version — a newer whatskept release is needed")
		default:
			if item.Error != nil {
				return fmt.Errorf("pairing failed (%s): %w", item.Event, item.Error)
			}
			logf("pairing event: %s", item.Event)
		}
	}
	return nil
}

// bindNumber holds live to the same contract as import: the first link
// binds the workspace to the account, and any other account is refused
// thereafter. user is the linked JID's user part — bare digits.
func bindNumber(root string, s *workspace.Settings, user string) error {
	number := "+" + user
	switch {
	case s.WhatsAppNumber == "":
		s.WhatsAppNumber = number
		if err := workspace.Save(root, *s); err != nil {
			return err
		}
		logf("workspace bound to WhatsApp number %s", number)
	case s.WhatsAppNumber == number:
		logf("linked account matches workspace WhatsApp number %s", number)
	default:
		return fmt.Errorf("linked WhatsApp account is %s but this workspace is bound to %s", number, s.WhatsAppNumber)
	}
	return nil
}

// decryptSecret opens a secretEncryptedMessage envelope when one is
// present. Returns nil for ordinary messages and when decryption fails
// — the caller then records a skip with the reason rather than guessing
// at the contents.
func decryptSecret(ctx context.Context, client *whatsmeow.Client, v *events.Message) *secretPayload {
	enc := v.Message.GetSecretEncryptedMessage()
	if enc == nil {
		return nil
	}
	msg, err := client.DecryptSecretEncryptedMessage(ctx, v)
	if err != nil {
		logf("decrypt secret payload for %s failed: %v", v.Info.ID, err)
		return nil
	}
	return &secretPayload{
		Msg:      msg,
		TargetID: enc.GetTargetMessageKey().GetID(),
		Kind:     enc.GetSecretEncType(),
	}
}

func logMessage(v *events.Message, outcome string) {
	info := v.Info
	direction := "recv"
	if info.IsFromMe {
		direction = "sent" // from another of the user's devices, or by `send`
	}
	scope := "dm"
	if info.IsGroup {
		scope = "group"
	}
	sender := info.Sender.String()
	if !info.SenderAlt.IsEmpty() {
		sender = fmt.Sprintf("%s (alt %s)", sender, info.SenderAlt)
	}
	kind, detail := describeContent(v)
	logf("msg id=%s %s %s chat=%s sender=%s push=%q ts=%s type=%s%s%s",
		info.ID, direction, scope, info.Chat, sender, info.PushName,
		info.Timestamp.UTC().Format(time.RFC3339), kind, detail, outcome)
}

// describeContent returns a coarse type label and a short, hard-clipped
// preview — these lines run to a terminal the user reads.
func describeContent(v *events.Message) (kind string, detail string) {
	m := v.Message
	switch {
	case m == nil:
		return "empty", ""
	case m.GetConversation() != "":
		return "text", " text=" + clip(m.GetConversation())
	case m.GetExtendedTextMessage() != nil:
		return "text", " text=" + clip(m.GetExtendedTextMessage().GetText())
	case m.GetImageMessage() != nil:
		return "image", captionOf(m.GetImageMessage().GetCaption())
	case m.GetVideoMessage() != nil:
		return "video", captionOf(m.GetVideoMessage().GetCaption())
	case m.GetAudioMessage() != nil:
		a := m.GetAudioMessage()
		return "audio", fmt.Sprintf(" ptt=%v seconds=%d", a.GetPTT(), a.GetSeconds())
	case m.GetDocumentMessage() != nil:
		d := m.GetDocumentMessage()
		return "document", fmt.Sprintf(" filename=%q pages=%d", d.GetFileName(), d.GetPageCount())
	case m.GetStickerMessage() != nil:
		return "sticker", ""
	case m.GetLocationMessage() != nil:
		return "location", ""
	case m.GetContactMessage() != nil:
		return "contact", ""
	case m.GetReactionMessage() != nil:
		r := m.GetReactionMessage()
		return "reaction", fmt.Sprintf(" emoji=%q target=%s", r.GetText(), r.GetKey().GetID())
	case m.GetSecretEncryptedMessage() != nil:
		return "secret:" + m.GetSecretEncryptedMessage().GetSecretEncType().String(), ""
	case m.GetProtocolMessage() != nil:
		return "protocol", fmt.Sprintf(" subtype=%s", m.GetProtocolMessage().GetType())
	case m.GetPollCreationMessageV3() != nil:
		return "poll", ""
	default:
		return "other", ""
	}
}

func captionOf(c string) string {
	if c == "" {
		return ""
	}
	return " caption=" + clip(c)
}

func clip(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", "⏎")
	const max = 80
	if len([]rune(s)) <= max {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q…", string([]rune(s)[:max]))
}

func logf(format string, args ...any) {
	fmt.Printf("%s [live] %s\n", time.Now().UTC().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}
