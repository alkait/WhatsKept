//go:build e2e

// Real end-to-end tests against the real WhatsApp account. Never part
// of a plain `go test ./...` — the build tag hides this file until you
// ask, passing the test workspace explicitly:
//
//	go test -tags e2e ./e2e-test -workspace /path/to/test-workspace
//
// Prerequisites:
//   - the tester's second number paired: `go run ./cmd/whatsapp-tester pair`
//     (session lives at e2e-test/session.db, gitignored)
//   - `whatskept live` RUNNING in the test workspace — the suite sends
//     real messages and waits for live to capture them.
//
// Hard rule: e2e traffic is strictly between the tester number and the
// workspace's own account, in chats the suite creates. The destination
// is DERIVED from the workspace's binding — there is no way to
// configure a third party.
package e2etest

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var (
	workspaceFlag = flag.String("workspace", "", "whatskept workspace `whatskept live` is running in")

	// -enrich declares that live is running WITH OPENROUTER_API_KEY.
	// It gates the paid enrichment tests on, and relaxes the
	// queued-file assertions in the capture tests (the enricher
	// consumes queue files within seconds, so their presence can no
	// longer be asserted).
	enrichFlag = flag.Bool("enrich", false, "live is running with OPENROUTER_API_KEY: run the paid enrichment tests")
)

type config struct {
	// Workspace is the whatskept workspace under test; assertions read
	// its ChatStorage.sqlite (read-only).
	Workspace string
	// Chat is the JID the tester sends into — always the workspace
	// account's own number, derived from its binding.
	Chat string
}

func loadConfig(t *testing.T) config {
	t.Helper()
	if *workspaceFlag == "" {
		t.Fatal("pass the test workspace: go test -tags e2e ./e2e-test -workspace <dir>")
	}
	ws := *workspaceFlag
	if !filepath.IsAbs(ws) {
		// The test binary runs with cwd = e2e-test/, but a relative
		// path on the command line was typed from the repo root.
		ws = filepath.Join("..", ws)
	}
	settings, err := os.ReadFile(filepath.Join(ws, ".whatskept", "settings.json"))
	if err != nil {
		t.Fatalf("read workspace settings: %v", err)
	}
	var s struct {
		WhatsAppNumber string `json:"whatsapp_number"`
	}
	if err := json.Unmarshal(settings, &s); err != nil {
		t.Fatalf("parse workspace settings: %v", err)
	}
	if s.WhatsAppNumber == "" {
		t.Fatal("workspace is not bound to a WhatsApp number — import (or pair live) first")
	}
	return config{
		Workspace: ws,
		Chat:      strings.TrimPrefix(s.WhatsAppNumber, "+") + "@s.whatsapp.net",
	}
}

// tester runs the whatsapp-tester CLI from the repo root and returns
// its combined output.
func tester(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"run", "./cmd/whatsapp-tester"}, args...)...)
	cmd.Dir = ".."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("whatsapp-tester %v: %v\n%s", args, err, out)
	}
	return string(out)
}

var sentID = regexp.MustCompile(`sent id=(\S+)`)

// openWorkspaceDB opens the test workspace's database read-only.
func openWorkspaceDB(t *testing.T, cfg config) *sql.DB {
	t.Helper()
	path := filepath.Join(cfg.Workspace, "ChatStorage.sqlite")
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// waitFor polls cond every 2s until it returns true or the deadline
// passes. Everything asserted here depends on the running live process
// having captured an event, so patience is the assertion.
func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s never happened — is `whatskept live` running in the workspace?", desc)
}

// waitForRow polls until the stanza ID appears in ZWAMESSAGE, returning
// the row's PK.
func waitForRow(t *testing.T, db *sql.DB, stanzaID string) int64 {
	t.Helper()
	var pk int64
	waitFor(t, "message "+stanzaID+" appearing in the workspace DB", func() bool {
		err := db.QueryRow(`SELECT Z_PK FROM ZWAMESSAGE WHERE ZSTANZAID = ?`, stanzaID).Scan(&pk)
		if err != nil && err != sql.ErrNoRows {
			t.Fatalf("query workspace DB: %v", err)
		}
		return err == nil
	})
	return pk
}

// sendText sends a uniquely stamped text and returns its stanza ID and
// the text.
func sendText(t *testing.T, cfg config, label string) (stanza, text string) {
	t.Helper()
	text = fmt.Sprintf("whatskept e2e %s %d", label, time.Now().UnixNano())
	out := tester(t, "send-text", cfg.Chat, text)
	m := sentID.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no stanza ID in tester output: %s", out)
	}
	return m[1], text
}

// TestSendTextCaptured drives the full loop: the tester's second
// number sends a real text message, `whatskept live` (running against
// the workspace) captures it, and the row lands in the database with
// the right text, an FTS entry, and a wa_live_pk ledger mark.
func TestSendTextCaptured(t *testing.T) {
	cfg := loadConfig(t)
	stanza, text := sendText(t, cfg, "text")
	t.Logf("sent %s", stanza)

	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	var gotText string
	if err := db.QueryRow(`SELECT ZTEXT FROM ZWAMESSAGE WHERE Z_PK = ?`, pk).Scan(&gotText); err != nil {
		t.Fatal(err)
	}
	if gotText != text {
		t.Errorf("ZTEXT = %q, want %q", gotText, text)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND text = ?`, pk, text).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts rows = %d, %v", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM wa_live_pk WHERE pk = ?`, pk).Scan(&n); err != nil || n != 1 {
		t.Errorf("wa_live_pk rows = %d, %v", n, err)
	}
}

// TestEditCaptured: send, edit, and expect the row's text (and FTS) to
// become the new text.
func TestEditCaptured(t *testing.T) {
	cfg := loadConfig(t)
	stanza, _ := sendText(t, cfg, "edit-me")
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	newText := fmt.Sprintf("whatskept e2e edited %d", time.Now().UnixNano())
	tester(t, "edit", cfg.Chat, stanza, newText)

	waitFor(t, "edit applied", func() bool {
		var text sql.NullString
		if err := db.QueryRow(`SELECT ZTEXT FROM ZWAMESSAGE WHERE Z_PK = ?`, pk).Scan(&text); err != nil {
			t.Fatal(err)
		}
		return text.Valid && text.String == newText
	})
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND text = ?`, pk, newText).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts rows for edited text = %d, %v", n, err)
	}
}

// TestRevokeCaptured: send, revoke, and expect the tombstone — type 14,
// text NULL, FTS row gone.
func TestRevokeCaptured(t *testing.T) {
	cfg := loadConfig(t)
	stanza, _ := sendText(t, cfg, "revoke-me")
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	tester(t, "revoke", cfg.Chat, stanza)

	waitFor(t, "revoke applied", func() bool {
		var msgType int
		if err := db.QueryRow(`SELECT ZMESSAGETYPE FROM ZWAMESSAGE WHERE Z_PK = ?`, pk).Scan(&msgType); err != nil {
			t.Fatal(err)
		}
		return msgType == 14
	})
	var text sql.NullString
	if err := db.QueryRow(`SELECT ZTEXT FROM ZWAMESSAGE WHERE Z_PK = ?`, pk).Scan(&text); err != nil || text.Valid {
		t.Errorf("ZTEXT after revoke = %+v, want NULL (%v)", text, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = ?`, pk).Scan(&n); err != nil || n != 0 {
		t.Errorf("fts rows after revoke = %d, %v", n, err)
	}
}

// TestReactionCaptured: react, change it, withdraw it. Each step shows
// in v_reactions, and the message itself is never touched — withdrawing
// arrives flagged as a revoke.
func TestReactionCaptured(t *testing.T) {
	cfg := loadConfig(t)
	stanza, text := sendText(t, cfg, "react-to-me")
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	emoji := func() string {
		var e string
		err := db.QueryRow(`SELECT emoji FROM v_reactions WHERE stanza_id = ? AND rowid = ?`, stanza, pk).Scan(&e)
		if err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		return e
	}

	tester(t, "react", cfg.Chat, stanza, "❤️")
	waitFor(t, "reaction captured", func() bool { return emoji() == "❤️" })

	tester(t, "react", cfg.Chat, stanza, "😂")
	waitFor(t, "reaction changed", func() bool { return emoji() == "😂" })

	tester(t, "react", cfg.Chat, stanza, "")
	waitFor(t, "reaction withdrawn", func() bool { return emoji() == "" })

	var msgType int
	var got string
	if err := db.QueryRow(`SELECT ZMESSAGETYPE, ZTEXT FROM ZWAMESSAGE WHERE Z_PK = ?`, pk).Scan(&msgType, &got); err != nil {
		t.Fatal(err)
	}
	if msgType != 0 || got != text {
		t.Errorf("message after withdrawal: type %d text %q", msgType, got)
	}
}

// sendMedia sends a fixture via the given tester subcommand and
// returns the stanza ID.
func sendMedia(t *testing.T, cfg config, subcmd, fixture string, extra ...string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	out := tester(t, append([]string{subcmd, cfg.Chat, abs}, extra...)...)
	m := sentID.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no stanza ID in %s output: %s", subcmd, out)
	}
	return m[1]
}

// assertQueuedFile checks the captured blob sits in the enrichment
// queue with exactly the fixture's bytes. Skipped under -enrich: the
// enricher may have already consumed the file (that consumption is
// asserted by the enrichment tests instead).
func assertQueuedFile(t *testing.T, cfg config, rel, fixture string) {
	t.Helper()
	if *enrichFlag {
		return
	}
	got, err := os.ReadFile(filepath.Join(cfg.Workspace, rel))
	if err != nil {
		t.Fatalf("queued file: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("queued file differs from fixture: %d vs %d bytes", len(got), len(want))
	}
}

// TestImageCaptured: image with caption → ZWAMEDIAITEM row, caption in
// ZTITLE and FTS, file queued under .unenriched/media/<pk>.jpg.
func TestImageCaptured(t *testing.T) {
	cfg := loadConfig(t)
	caption := fmt.Sprintf("whatskept e2e image %d", time.Now().UnixNano())
	stanza := sendMedia(t, cfg, "send-image", "image.jpg", caption)
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	var title, localPath string
	if err := db.QueryRow(`SELECT ZTITLE, ZMEDIALOCALPATH FROM ZWAMEDIAITEM WHERE ZMESSAGE = ?`, pk).Scan(&title, &localPath); err != nil {
		t.Fatal(err)
	}
	if title != caption || !strings.HasSuffix(localPath, ".jpg") {
		t.Errorf("media item: title=%q path=%q", title, localPath)
	}
	assertQueuedFile(t, cfg, filepath.Join(".unenriched", "media", fmt.Sprintf("%d.jpg", pk)), "image.jpg")
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND text = ?`, pk, caption).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts rows for caption = %d, %v", n, err)
	}
}

// TestVoiceCaptured: PTT voice note → duration recorded, file queued
// under .unenriched/voice/<pk>.opus.
func TestVoiceCaptured(t *testing.T) {
	cfg := loadConfig(t)
	stanza := sendMedia(t, cfg, "send-voice", "voice.opus")
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	var duration int
	var localPath string
	if err := db.QueryRow(`SELECT ZMOVIEDURATION, ZMEDIALOCALPATH FROM ZWAMEDIAITEM WHERE ZMESSAGE = ?`, pk).Scan(&duration, &localPath); err != nil {
		t.Fatal(err)
	}
	if duration != 2 || !strings.HasSuffix(localPath, ".opus") {
		t.Errorf("media item: duration=%d path=%q", duration, localPath)
	}
	assertQueuedFile(t, cfg, filepath.Join(".unenriched", "voice", fmt.Sprintf("%d.opus", pk)), "voice.opus")
}

// TestPDFCaptured: document → original filename in ZAUTHORNAME (and
// FTS), file queued under .unenriched/documents/<pk>.pdf.
func TestPDFCaptured(t *testing.T) {
	cfg := loadConfig(t)
	stanza := sendMedia(t, cfg, "send-pdf", "doc.pdf")
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	var author, localPath string
	if err := db.QueryRow(`SELECT ZAUTHORNAME, ZMEDIALOCALPATH FROM ZWAMEDIAITEM WHERE ZMESSAGE = ?`, pk).Scan(&author, &localPath); err != nil {
		t.Fatal(err)
	}
	if author != "doc.pdf" || !strings.HasSuffix(localPath, ".pdf") {
		t.Errorf("media item: author=%q path=%q", author, localPath)
	}
	assertQueuedFile(t, cfg, filepath.Join(".unenriched", "documents", fmt.Sprintf("%d.pdf", pk)), "doc.pdf")
}

// TestImageRevokeCleans: revoking a captured image tombstones the row
// AND removes the queued file.
func TestImageRevokeCleans(t *testing.T) {
	if *enrichFlag {
		t.Skip("premise is an unconsumed queue file — run without -enrich (revoke-of-enriched is covered by TestImageEnrichedThenRevoked)")
	}
	cfg := loadConfig(t)
	stanza := sendMedia(t, cfg, "send-image", "image.jpg")
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)
	queued := filepath.Join(cfg.Workspace, ".unenriched", "media", fmt.Sprintf("%d.jpg", pk))
	if _, err := os.Stat(queued); err != nil {
		t.Fatalf("queued file missing before revoke: %v", err)
	}

	tester(t, "revoke", cfg.Chat, stanza)

	waitFor(t, "media revoke applied", func() bool {
		var msgType int
		if err := db.QueryRow(`SELECT ZMESSAGETYPE FROM ZWAMESSAGE WHERE Z_PK = ?`, pk).Scan(&msgType); err != nil {
			t.Fatal(err)
		}
		return msgType == 14
	})
	if _, err := os.Stat(queued); !os.IsNotExist(err) {
		t.Errorf("queued file still present after revoke: %v", err)
	}
}

// requireEnrich gates the paid enrichment tests behind -enrich.
func requireEnrich(t *testing.T) {
	t.Helper()
	if !*enrichFlag {
		t.Skip("paid enrichment test — run with -enrich while live has OPENROUTER_API_KEY")
	}
}

// waitEnriched polls until the enrichment row for pk exists in table,
// then asserts the queue file is gone (enrichment deletes on success).
func waitEnriched(t *testing.T, cfg config, db *sql.DB, table string, pk int64, queueRel string) {
	t.Helper()
	waitFor(t, table+" enrichment for pk", func() bool {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE rowid = ?`, pk).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	})
	if _, err := os.Stat(filepath.Join(cfg.Workspace, queueRel)); !os.IsNotExist(err) {
		t.Errorf("queue file still present after enrichment: %v", err)
	}
}

// TestImageEnrichedThenRevoked: the full paid loop — a captured image
// is described within seconds (doorbell, not backstop), its FTS row
// folds the new text while keeping the caption, and a later revoke
// removes the derived text again.
func TestImageEnrichedThenRevoked(t *testing.T) {
	requireEnrich(t)
	cfg := loadConfig(t)
	caption := fmt.Sprintf("whatskept e2e enrich %d", time.Now().UnixNano())
	stanza := sendMedia(t, cfg, "send-image", "image.jpg", caption)
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	waitEnriched(t, cfg, db, "wa_image_text", pk, filepath.Join(".unenriched", "media", fmt.Sprintf("%d.jpg", pk)))
	var desc string
	if err := db.QueryRow(`SELECT description FROM wa_image_text WHERE rowid = ?`, pk).Scan(&desc); err != nil {
		t.Fatal(err)
	}
	if desc == "" {
		t.Error("description is empty")
	}
	t.Logf("description: %s", desc)
	var fts string
	if err := db.QueryRow(`SELECT text FROM messages_fts WHERE rowid = ?`, pk).Scan(&fts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fts, caption) {
		t.Errorf("fts lost the caption: %q", fts)
	}
	if strings.TrimSpace(strings.ReplaceAll(fts, caption, "")) == "" {
		t.Errorf("fts has no enrichment text beyond the caption: %q", fts)
	}

	// Revoke the enriched message: derived text must go too.
	tester(t, "revoke", cfg.Chat, stanza)
	waitFor(t, "revoke of enriched image", func() bool {
		var msgType int
		if err := db.QueryRow(`SELECT ZMESSAGETYPE FROM ZWAMESSAGE WHERE Z_PK = ?`, pk).Scan(&msgType); err != nil {
			t.Fatal(err)
		}
		return msgType == 14
	})
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wa_image_text WHERE rowid = ?`, pk).Scan(&n); err != nil || n != 0 {
		t.Errorf("wa_image_text after revoke = %d, %v", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = ?`, pk).Scan(&n); err != nil || n != 0 {
		t.Errorf("fts after revoke = %d, %v", n, err)
	}
}

// TestVoiceEnriched: the spoken fixture comes back as a transcript.
func TestVoiceEnriched(t *testing.T) {
	requireEnrich(t)
	cfg := loadConfig(t)
	stanza := sendMedia(t, cfg, "send-voice", "voice.opus")
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	waitEnriched(t, cfg, db, "wa_voice_text", pk, filepath.Join(".unenriched", "voice", fmt.Sprintf("%d.opus", pk)))
	var transcript string
	if err := db.QueryRow(`SELECT transcript FROM wa_voice_text WHERE rowid = ?`, pk).Scan(&transcript); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(transcript) == "" {
		t.Error("transcript is empty")
	}
	t.Logf("transcript: %s", transcript)
	// The fixture says "hello, this is the whatskept end to end test
	// voice note" — expect at least one distinctive word to survive.
	if lower := strings.ToLower(transcript); !strings.Contains(lower, "hello") && !strings.Contains(lower, "test") {
		t.Errorf("transcript unrecognizable: %q", transcript)
	}
}

// TestPDFEnriched: the stamped text layer is extracted and searchable.
func TestPDFEnriched(t *testing.T) {
	requireEnrich(t)
	cfg := loadConfig(t)
	stanza := sendMedia(t, cfg, "send-pdf", "doc.pdf")
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	waitEnriched(t, cfg, db, "wa_document_text", pk, filepath.Join(".unenriched", "documents", fmt.Sprintf("%d.pdf", pk)))
	var text string
	if err := db.QueryRow(`SELECT text FROM wa_document_text WHERE rowid = ?`, pk).Scan(&text); err != nil {
		t.Fatal(err)
	}
	t.Logf("extracted: %s", text)
	if !strings.Contains(strings.ToLower(text), "whatskept") {
		t.Errorf("stamped text not extracted: %q", text)
	}
	// And it is searchable: the FTS row folds the document text.
	var fts string
	if err := db.QueryRow(`SELECT text FROM messages_fts WHERE rowid = ?`, pk).Scan(&fts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(fts), "whatskept") {
		t.Errorf("fts missing document text: %q", fts)
	}
}

// TestReplyCaptured: send A, reply to it, and expect the reply's
// ZPARENTMESSAGE to resolve to A's PK.
func TestReplyCaptured(t *testing.T) {
	cfg := loadConfig(t)
	stanzaA, _ := sendText(t, cfg, "quote-me")
	db := openWorkspaceDB(t, cfg)
	pkA := waitForRow(t, db, stanzaA)

	replyText := fmt.Sprintf("whatskept e2e reply %d", time.Now().UnixNano())
	out := tester(t, "reply", cfg.Chat, stanzaA, replyText)
	m := sentID.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no stanza ID in reply output: %s", out)
	}
	pkB := waitForRow(t, db, m[1])

	var text string
	var parent sql.NullInt64
	if err := db.QueryRow(`SELECT ZTEXT, ZPARENTMESSAGE FROM ZWAMESSAGE WHERE Z_PK = ?`, pkB).Scan(&text, &parent); err != nil {
		t.Fatal(err)
	}
	if text != replyText {
		t.Errorf("reply ZTEXT = %q, want %q", text, replyText)
	}
	if !parent.Valid || parent.Int64 != pkA {
		t.Errorf("ZPARENTMESSAGE = %+v, want %d", parent, pkA)
	}
}

// TestForwardSent drives a forward the other way: the tester sends a
// text, then `whatskept send --forward` sends it back to the tester's
// chat through the running live, which records the forward there.
func TestForwardSent(t *testing.T) {
	cfg := loadConfig(t)
	stanza, text := sendText(t, cfg, "forward-me")
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	// The destination is the tester's own chat, read from the captured
	// row — never configured.
	var testerChat string
	if err := db.QueryRow(`SELECT cs.ZCONTACTJID FROM ZWAMESSAGE m
		JOIN ZWACHATSESSION cs ON cs.Z_PK = m.ZCHATSESSION WHERE m.Z_PK = ?`, pk).Scan(&testerChat); err != nil {
		t.Fatal(err)
	}

	out := whatskept(t, cfg, buildWhatskept(t), "send", testerChat, "--forward", stanza)
	m := sentID.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no stanza ID in send output: %s", out)
	}
	fwd := waitForRow(t, db, m[1])

	var got string
	var fromMe int
	var chat int64
	var origChat int64
	if err := db.QueryRow(`SELECT ZTEXT, ZISFROMME, ZCHATSESSION FROM ZWAMESSAGE WHERE Z_PK = ?`, fwd).Scan(&got, &fromMe, &chat); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT ZCHATSESSION FROM ZWAMESSAGE WHERE Z_PK = ?`, pk).Scan(&origChat); err != nil {
		t.Fatal(err)
	}
	if got != text || fromMe != 1 || chat != origChat {
		t.Errorf("forward row: text=%q fromMe=%d chat=%d, want %q, 1, %d", got, fromMe, chat, text, origChat)
	}
}

// buildWhatskept builds the CLI from this checkout and returns its path.
func buildWhatskept(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "whatskept")
	build := exec.Command("go", "build", "-o", bin, "./cmd/whatskept")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build whatskept: %v\n%s", err, out)
	}
	return bin
}

// whatskept runs the built CLI in the test workspace and returns its
// combined output.
func whatskept(t *testing.T, cfg config, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = cfg.Workspace
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("whatskept %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestArchiveSent archives the tester's chat in the workspace account
// with `whatskept archive`, then unarchives it with --undo, through the
// running live. Exit 0 means WhatsApp accepted the app-state change;
// the database follows each one. Whether the phone shows it is beyond
// what the tester number can see.
func TestArchiveSent(t *testing.T) {
	cfg := loadConfig(t)
	stanza, _ := sendText(t, cfg, "archive-me")
	bin := buildWhatskept(t)
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	var testerChat string
	var chatPK int64
	var was sql.NullInt64
	if err := db.QueryRow(`SELECT cs.ZCONTACTJID, cs.Z_PK, cs.ZARCHIVED FROM ZWAMESSAGE m
		JOIN ZWACHATSESSION cs ON cs.Z_PK = m.ZCHATSESSION WHERE m.Z_PK = ?`, pk).Scan(&testerChat, &chatPK, &was); err != nil {
		t.Fatal(err)
	}
	archived := func() int64 {
		var v sql.NullInt64
		if err := db.QueryRow(`SELECT ZARCHIVED FROM ZWACHATSESSION WHERE Z_PK = ?`, chatPK).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v.Int64
	}
	// Leave the chat as it was found, whatever happens below.
	t.Cleanup(func() {
		args := []string{"archive", testerChat, "--undo"}
		if was.Int64 == 1 {
			args = args[:2]
		}
		whatskept(t, cfg, bin, args...)
	})

	out := whatskept(t, cfg, bin, "archive", testerChat)
	if !strings.HasPrefix(out, "archived chat=") {
		t.Fatalf("archive output = %q", out)
	}
	if v := archived(); v != 1 {
		t.Errorf("after archive: ZARCHIVED = %d, want 1", v)
	}

	out = whatskept(t, cfg, bin, "archive", testerChat, "--undo")
	if !strings.HasPrefix(out, "unarchived chat=") {
		t.Fatalf("unarchive output = %q", out)
	}
	if v := archived(); v != 0 {
		t.Errorf("after unarchive: ZARCHIVED = %d, want 0", v)
	}
}
