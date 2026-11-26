package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"whatskept/internal/backup"
	"whatskept/internal/enrich"
	"whatskept/internal/live"
	"whatskept/internal/mcpserve"
)

// binPath is the whatskept binary built once for all CLI tests.
var binPath string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "whatskept-test")
	if err != nil {
		panic(err)
	}
	binPath = filepath.Join(tmp, "whatskept")
	if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		panic("build failed: " + string(out))
	}
	// This Go toolchain (1.22.x) emits an invalid ad-hoc signature when
	// linking cgo binaries on current macOS; the kernel SIGKILLs them.
	// Re-signing fixes it. Harmless once Go is upgraded.
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("codesign", "-f", "-s", "-", binPath).CombinedOutput(); err != nil {
			panic("codesign failed: " + string(out))
		}
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

// run executes the binary in dir and returns exit code, stdout, stderr.
func run(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
	// Tests must not inherit a real backup password, MCP token, or API
	// key from the developer's environment; empty counts as unset.
	cmd.Env = append(os.Environ(),
		backup.PasswordEnv+"=", mcpserve.TokenEnv+"=", enrich.APIKeyEnv+"=")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return code, stdout.String(), stderr.String()
}

func TestCLIVersion(t *testing.T) {
	for _, flag := range []string{"-v", "--version"} {
		code, stdout, _ := run(t, t.TempDir(), flag)
		if code != 0 {
			t.Errorf("%s: exit %d, want 0", flag, code)
		}
		if stdout != "dev\n" {
			t.Errorf("%s: stdout = %q, want \"dev\\n\"", flag, stdout)
		}
	}
}

func TestCLILiveRequiresWorkspace(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "live")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "not a whatskept workspace") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLILiveInUsage(t *testing.T) {
	code, stdout, _ := run(t, t.TempDir(), "--help")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "whatskept live") {
		t.Errorf("usage missing live: %q", stdout)
	}
	if !strings.Contains(stdout, "whatskept archive <chat>") {
		t.Errorf("usage missing archive: %q", stdout)
	}
	if !strings.Contains(stdout, "whatskept read <chat>") {
		t.Errorf("usage missing read: %q", stdout)
	}
}

func TestCLIInit(t *testing.T) {
	dir := t.TempDir()
	code, stdout, _ := run(t, dir, "init", "ws1")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "initialized whatskept workspace in ws1") {
		t.Errorf("stdout = %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, "ws1", ".whatskept", "settings.json")); err != nil {
		t.Errorf("settings.json missing: %v", err)
	}
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(dir, "ws1", name)); err != nil {
			t.Errorf("%s missing: %v", name, err)
		}
	}
}

func TestCLIInitIdempotent(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "ws1")
	code, stdout, _ := run(t, dir, "init", "ws1")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "already a whatskept workspace") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestCLIHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		code, stdout, _ := run(t, t.TempDir(), arg)
		if code != 0 {
			t.Errorf("%s: exit %d, want 0", arg, code)
		}
		if !strings.Contains(stdout, "Usage:") {
			t.Errorf("%s: stdout = %q", arg, stdout)
		}
	}
}

func TestCLINoArgs(t *testing.T) {
	code, _, stderr := run(t, t.TempDir())
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIUnknownCommand(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "bogus")
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown command: bogus") || !strings.Contains(stderr, "Usage:") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIInitFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, permission checks don't apply")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	code, _, stderr := run(t, dir, "init", "ws1")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "error:") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIImportNoArg(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "import")
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr, "import requires the path") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIImportOutsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	b := writeImportBackup(t, dir, fakeUDID)
	code, _, stderr := run(t, dir, "import", b)
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "not a whatskept workspace") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIImportBindsThenNeedsPassword(t *testing.T) {
	ws := t.TempDir()
	run(t, ws, "init")
	b := writeImportBackup(t, t.TempDir(), fakeUDID)

	// Real decryption needs a password, so the run stops there (exit 1) —
	// but only after checkpoint 1 has bound the device.
	code, stdout, stderr := run(t, ws, "import", b)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (missing password)", code)
	}
	if !strings.Contains(stdout, "workspace bound to device "+fakeUDID) {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, backup.PasswordEnv) {
		t.Errorf("stderr should point at %s: %q", backup.PasswordEnv, stderr)
	}

	other := writeImportBackup(t, t.TempDir(), "99998888-FFFFEEEEDDDDCCCC")
	code, _, stderr = run(t, ws, "import", other)
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "bound to device "+fakeUDID) {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIImportUnencryptedBackup(t *testing.T) {
	ws := t.TempDir()
	run(t, ws, "init")
	b := writeImportBackup(t, t.TempDir(), fakeUDID)
	manifest := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>IsEncrypted</key><false/></dict></plist>
`
	if err := os.WriteFile(filepath.Join(b, "Manifest.plist"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, ws, "import", b)
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "not encrypted") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIEnrichOutsideWorkspace(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "enrich")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "not a whatskept workspace") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIEnrichNoDatabase(t *testing.T) {
	ws := t.TempDir()
	run(t, ws, "init")
	code, _, stderr := run(t, ws, "enrich")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "whatskept import") {
		t.Errorf("stderr = %q, want import hint", stderr)
	}
}

// --concurrency overrides the built-in in-flight cap. A negative value
// is rejected before any workspace or API work; a positive one parses
// and lets the run proceed to the usual checks.
func TestCLIEnrichConcurrency(t *testing.T) {
	ws := t.TempDir()
	run(t, ws, "init")

	code, _, stderr := run(t, ws, "enrich", "--concurrency", "-1")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "--concurrency must be positive") {
		t.Errorf("stderr = %q, want rejection of a negative value", stderr)
	}

	code, _, stderr = run(t, ws, "enrich", "--concurrency", "4")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "whatskept import") {
		t.Errorf("stderr = %q, want the flag accepted and the run to reach the database check", stderr)
	}
}

// --help on a subcommand prints its flags (with real defaults) and
// exits 0 — agents discover flags this way.
func TestCLISubcommandHelp(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "enrich", "--help")
	if code != 0 {
		t.Errorf("enrich --help: exit %d, want 0", code)
	}
	if !strings.Contains(stderr, "concurrency") || !strings.Contains(stderr, "48") {
		t.Errorf("enrich --help must show the flag and its default: %q", stderr)
	}
	code, _, stderr = run(t, t.TempDir(), "mcp", "--help")
	if code != 0 {
		t.Errorf("mcp --help: exit %d, want 0", code)
	}
	if !strings.Contains(stderr, "database") {
		t.Errorf("mcp --help must show --database: %q", stderr)
	}
}

func TestCLIMCPRequiresDatabase(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "mcp")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "--database") {
		t.Errorf("stderr = %q, want mention of --database", stderr)
	}
}

func TestCLIMCPRequiresToken(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "mcp", "--database", "ChatStorage.sqlite")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, mcpserve.TokenEnv) {
		t.Errorf("stderr = %q, want mention of %s", stderr, mcpserve.TokenEnv)
	}
}

// TestCLIMCPNeverPrintsToken: the startup lines name the endpoint, not
// the secret.
func TestCLIMCPNeverPrintsToken(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	cmd := exec.Command(binPath, "mcp", "--database", "ChatStorage.sqlite", "--addr", "127.0.0.1:0")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), mcpserve.TokenEnv+"="+token)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Both startup lines, then stop the server.
	var out string
	sc := bufio.NewScanner(stdout)
	for i := 0; i < 2 && sc.Scan(); i++ {
		out += sc.Text() + "\n"
	}
	cmd.Process.Kill()
	cmd.Wait()
	if !strings.Contains(out, "/mcp") {
		t.Errorf("stdout = %q, want the endpoint", out)
	}
	if strings.Contains(out, token) {
		t.Errorf("stdout leaks the token: %q", out)
	}
}

// TestCLIMCPStdio: --stdio speaks MCP over stdin/stdout with no token,
// serves even with no database yet, and keeps stdout pure protocol.
func TestCLIMCPStdio(t *testing.T) {
	cmd := exec.Command(binPath, "mcp", "--stdio", "--database", "ChatStorage.sqlite")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), mcpserve.TokenEnv+"=")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	// next reads one stdout line; every line must be a JSON-RPC message.
	next := func() map[string]any {
		t.Helper()
		if !sc.Scan() {
			t.Fatalf("no response on stdout (stderr: %q)", stderr.String())
		}
		var msg map[string]any
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil || msg["jsonrpc"] != "2.0" {
			t.Fatalf("stdout line is not JSON-RPC: %q", sc.Text())
		}
		return msg
	}
	send := func(s string) {
		t.Helper()
		if _, err := stdin.Write([]byte(s + "\n")); err != nil {
			t.Fatal(err)
		}
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	if init := next(); init["result"] == nil {
		t.Fatalf("initialize failed: %v", init)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list := next()
	result, _ := list["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if got, want := strings.Join(names, ","), "archive,get_schema,list,ping,query,read,search,send"; got != want {
		t.Errorf("tools = %s, want %s", got, want)
	}
}

func TestCLIList(t *testing.T) {
	root := t.TempDir()
	writeListBackup(t, root, "ABC123", "Test iPhone", "2026-08-20T10:00:00Z", true)
	code, stdout, _ := run(t, t.TempDir(), "import", "--list", root)
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	for _, want := range []string{"Test iPhone", "encrypted", "2026-08-20", "ABC123"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q: %q", want, stdout)
		}
	}
}

func TestCLIListEmpty(t *testing.T) {
	root := filepath.Join(t.TempDir(), "empty-root")
	code, stdout, _ := run(t, t.TempDir(), "import", "--list", root)
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "no backups found") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestCLISendUsage(t *testing.T) {
	for _, args := range [][]string{{"send"}, {"send", "+971500000001"}, {"send", "+971500000001", "hi", "extra"},
		{"send", "+971500000001", "hi", "--reply-to"}, {"send", "+971500000001", "hi", "--reply", "X"},
		{"send", "+971500000001", "--forward"}, {"send", "+971500000001", "--forward", "X", "extra"}} {
		code, _, stderr := run(t, t.TempDir(), args...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if !strings.Contains(stderr, "send requires") {
			t.Errorf("%v: stderr = %q", args, stderr)
		}
	}
}

func TestCLISendOutsideWorkspace(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "send", "+971500000001", "hi")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "not a whatskept workspace") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLISendLiveNotRunning(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init")
	check := func(when string) {
		code, stdout, stderr := run(t, dir, "send", "+971500000001", "hi")
		if code != 1 {
			t.Errorf("%s: exit %d, want 1", when, code)
		}
		if stdout != "" || !strings.Contains(stderr, "`whatskept live` is not running") {
			t.Errorf("%s: stdout = %q, stderr = %q", when, stdout, stderr)
		}
	}
	check("no live.json")

	// A live that crashed leaves its advertisement pointing at a dead port.
	if err := os.WriteFile(filepath.Join(dir, ".whatskept", "live.json"),
		[]byte(`{"addr":"127.0.0.1:1","token":"00000000000000000000000000000000"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	check("stale live.json")
}

// sendWorkspace is an initialized workspace with the slice of
// ChatStorage a text message touches, and the real send endpoint
// running over it with a stand-in for WhatsApp.
func sendWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init")
	db, err := sql.Open("sqlite", filepath.Join(dir, backup.ChatStorageName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER,
			ZCHATSESSION INTEGER, ZGROUPMEMBER INTEGER, ZISFROMME INTEGER,
			ZMESSAGETYPE INTEGER, ZMESSAGESTATUS INTEGER, ZMESSAGEDATE REAL, ZSENTDATE REAL,
			ZFROMJID TEXT, ZTOJID TEXT, ZSTANZAID TEXT, ZTEXT TEXT, ZPARENTMESSAGE INTEGER);
		CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER,
			ZSESSIONTYPE INTEGER, ZCONTACTJID TEXT, ZCONTACTIDENTIFIER TEXT,
			ZPARTNERNAME TEXT, ZMESSAGECOUNTER INTEGER,
			ZLASTMESSAGE INTEGER, ZLASTMESSAGEDATE REAL, ZLASTMESSAGETEXT TEXT, ZARCHIVED INTEGER);
		CREATE TABLE ZWAGROUPMEMBER (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZMEMBERJID TEXT);
		CREATE TABLE ZWAMEDIAITEM (Z_PK INTEGER PRIMARY KEY, ZMESSAGE INTEGER, ZMEDIALOCALPATH TEXT,
			ZFILESIZE INTEGER, ZMOVIEDURATION INTEGER, ZAUTHORNAME TEXT, ZTITLE TEXT,
			ZMEDIAKEY BLOB, ZMETADATA BLOB, ZVCARDSTRING TEXT);
		CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER, Z_NAME TEXT, Z_SUPER INTEGER, Z_MAX INTEGER);
		CREATE VIRTUAL TABLE messages_fts USING fts5(text);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	stop, err := live.StartFakeSend(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return dir
}

func TestCLISendThroughLive(t *testing.T) {
	dir := sendWorkspace(t)
	code, stdout, stderr := run(t, dir, "send", "+971 50 000 0001", "see you at 8")
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr = %q", code, stderr)
	}
	if !strings.HasPrefix(stdout, "sent id=FAKE1 chat=971500000001@s.whatsapp.net ts=") || stderr != "" {
		t.Errorf("stdout = %q, stderr = %q", stdout, stderr)
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, backup.ChatStorageName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var text, to string
	var fromMe int
	if err := db.QueryRow(`SELECT ZTEXT, ZTOJID, ZISFROMME FROM ZWAMESSAGE WHERE ZSTANZAID = 'FAKE1'`).
		Scan(&text, &to, &fromMe); err != nil {
		t.Fatal(err)
	}
	if text != "see you at 8" || to != "971500000001@s.whatsapp.net" || fromMe != 1 {
		t.Errorf("row: text=%q to=%q fromMe=%d", text, to, fromMe)
	}
}

func TestCLISendReply(t *testing.T) {
	dir := sendWorkspace(t)
	run(t, dir, "send", "+971500000001", "see you at 8")
	code, stdout, stderr := run(t, dir, "send", "+971500000001", "make it 9", "--reply-to", "FAKE1")
	if code != 0 || !strings.HasPrefix(stdout, "sent id=FAKE2 ") || stderr != "" {
		t.Fatalf("exit %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, backup.ChatStorageName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var parent string
	if err := db.QueryRow(`SELECT p.ZSTANZAID FROM ZWAMESSAGE m JOIN ZWAMESSAGE p ON p.Z_PK = m.ZPARENTMESSAGE
		WHERE m.ZSTANZAID = 'FAKE2'`).Scan(&parent); err != nil || parent != "FAKE1" {
		t.Errorf("parent = %q, err = %v, want FAKE1", parent, err)
	}

	// A target that isn't there sends nothing.
	code, stdout, stderr = run(t, dir, "send", "+971500000001", "hi", "--reply-to", "NOPE")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "cannot reply: no message with stanza ID") {
		t.Errorf("exit %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestCLISendForward(t *testing.T) {
	dir := sendWorkspace(t)
	run(t, dir, "send", "+971500000001", "see you at 8")
	code, stdout, stderr := run(t, dir, "send", "+971500000002", "--forward", "FAKE1")
	if code != 0 || !strings.HasPrefix(stdout, "sent id=FAKE2 chat=971500000002@s.whatsapp.net ") || stderr != "" {
		t.Fatalf("exit %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, backup.ChatStorageName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var text, to string
	if err := db.QueryRow(`SELECT ZTEXT, ZTOJID FROM ZWAMESSAGE WHERE ZSTANZAID = 'FAKE2'`).Scan(&text, &to); err != nil {
		t.Fatal(err)
	}
	if text != "see you at 8" || to != "971500000002@s.whatsapp.net" {
		t.Errorf("row: text=%q to=%q", text, to)
	}

	code, stdout, stderr = run(t, dir, "send", "+971500000002", "--forward", "NOPE")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "cannot forward: no message with stanza ID") {
		t.Errorf("exit %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestCLISendRejectsBadChat(t *testing.T) {
	dir := sendWorkspace(t)
	code, stdout, stderr := run(t, dir, "send", "Sarah", "hi")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if stdout != "" || !strings.Contains(stderr, "invalid chat") {
		t.Errorf("stdout = %q, stderr = %q", stdout, stderr)
	}
}

func TestCLIArchiveUsage(t *testing.T) {
	for _, args := range [][]string{{"archive"}, {"archive", "--undo"}, {"archive", "+971500000001", "extra"},
		{"archive", "+971500000001", "--undo", "extra"}, {"archive", "--undo", "+971500000001"}} {
		code, _, stderr := run(t, t.TempDir(), args...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if !strings.Contains(stderr, "archive requires") {
			t.Errorf("%v: stderr = %q", args, stderr)
		}
	}
}

func TestCLIArchiveOutsideWorkspace(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "archive", "+971500000001")
	if code != 1 || !strings.Contains(stderr, "not a whatskept workspace") {
		t.Errorf("exit %d, stderr = %q", code, stderr)
	}
}

func TestCLIArchiveLiveNotRunning(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init")
	code, stdout, stderr := run(t, dir, "archive", "+971500000001")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "`whatskept live` is not running") {
		t.Errorf("exit %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestCLIArchiveThroughLive(t *testing.T) {
	dir := sendWorkspace(t)
	run(t, dir, "send", "+971500000001", "see you at 8") // the chat now exists
	db, err := sql.Open("sqlite", filepath.Join(dir, backup.ChatStorageName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	archived := func() int {
		t.Helper()
		var v int
		if err := db.QueryRow(`SELECT COALESCE(ZARCHIVED, -1) FROM ZWACHATSESSION
			WHERE ZCONTACTJID = '971500000001@s.whatsapp.net'`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	code, stdout, stderr := run(t, dir, "archive", "+971 50 000 0001")
	if code != 0 || stdout != "archived chat=971500000001@s.whatsapp.net\n" || stderr != "" {
		t.Fatalf("exit %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if v := archived(); v != 1 {
		t.Errorf("ZARCHIVED = %d, want 1", v)
	}

	code, stdout, stderr = run(t, dir, "archive", "971500000001@s.whatsapp.net", "--undo")
	if code != 0 || stdout != "unarchived chat=971500000001@s.whatsapp.net\n" || stderr != "" {
		t.Fatalf("exit %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if v := archived(); v != 0 {
		t.Errorf("ZARCHIVED = %d, want 0", v)
	}

	// Archiving never creates a chat.
	code, stdout, stderr = run(t, dir, "archive", "+971500000002")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "cannot archive: chat 971500000002@s.whatsapp.net is not in the database") {
		t.Errorf("exit %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	code, _, stderr = run(t, dir, "archive", "Sarah")
	if code != 1 || !strings.Contains(stderr, "invalid chat") {
		t.Errorf("exit %d, stderr = %q", code, stderr)
	}
}

func TestCLIReadUsage(t *testing.T) {
	for _, args := range [][]string{{"read"}, {"read", "--undo"}, {"read", "+971500000001", "extra"},
		{"read", "+971500000001", "--undo", "extra"}, {"read", "--undo", "+971500000001"}} {
		code, _, stderr := run(t, t.TempDir(), args...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if !strings.Contains(stderr, "read requires") {
			t.Errorf("%v: stderr = %q", args, stderr)
		}
	}
}

func TestCLIReadOutsideWorkspace(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "read", "+971500000001")
	if code != 1 || !strings.Contains(stderr, "not a whatskept workspace") {
		t.Errorf("exit %d, stderr = %q", code, stderr)
	}
}

func TestCLIReadLiveNotRunning(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init")
	code, stdout, stderr := run(t, dir, "read", "+971500000001")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "`whatskept live` is not running") {
		t.Errorf("exit %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestCLIReadThroughLive(t *testing.T) {
	dir := sendWorkspace(t)
	run(t, dir, "send", "+971500000001", "see you at 8") // the chat now exists

	db, err := sql.Open("sqlite", filepath.Join(dir, backup.ChatStorageName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Whether the chat's read state is recorded as read through some
	// time, and as marked unread.
	state := func() (read, marked bool) {
		t.Helper()
		if err := db.QueryRow(`SELECT r.read_through IS NOT NULL, r.unread_marked IS NOT NULL
			FROM wa_chat_read r JOIN ZWACHATSESSION cs ON cs.Z_PK = r.chat_pk
			WHERE cs.ZCONTACTJID = '971500000001@s.whatsapp.net'`).Scan(&read, &marked); err != nil {
			t.Fatal(err)
		}
		return read, marked
	}

	code, stdout, stderr := run(t, dir, "read", "+971 50 000 0001")
	if code != 0 || stdout != "marked read chat=971500000001@s.whatsapp.net\n" || stderr != "" {
		t.Fatalf("exit %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if read, marked := state(); !read || marked {
		t.Errorf("after read: read=%v marked=%v, want read and not marked", read, marked)
	}
	code, stdout, stderr = run(t, dir, "read", "971500000001@s.whatsapp.net", "--undo")
	if code != 0 || stdout != "marked unread chat=971500000001@s.whatsapp.net\n" || stderr != "" {
		t.Fatalf("exit %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if _, marked := state(); !marked {
		t.Errorf("after mark unread: not marked")
	}

	code, stdout, stderr = run(t, dir, "read", "+971500000002")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "cannot mark read: chat 971500000002@s.whatsapp.net is not in the database") {
		t.Errorf("exit %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	code, _, stderr = run(t, dir, "read", "Sarah")
	if code != 1 || !strings.Contains(stderr, "invalid chat") {
		t.Errorf("exit %d, stderr = %q", code, stderr)
	}
}

func TestCLIListUsage(t *testing.T) {
	for _, args := range [][]string{{"list"}, {"list", "create"}, {"list", "create", "a", "b"},
		{"list", "rename", "a"}, {"list", "delete"}, {"list", "add", "a"}, {"list", "remove", "a", "b", "c"},
		{"list", "archive", "a"}} {
		code, _, stderr := run(t, t.TempDir(), args...)
		if code != 2 || !strings.Contains(stderr, "list requires one of") {
			t.Errorf("%v: exit %d, stderr = %q", args, code, stderr)
		}
	}
	code, stdout, _ := run(t, t.TempDir(), "--help")
	if code != 0 || !strings.Contains(stdout, "whatskept list create <name>") || !strings.Contains(stdout, "whatskept list remove <list> <chat>") {
		t.Errorf("usage missing list: %q", stdout)
	}
}

func TestCLIListOutsideWorkspace(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "list", "create", "Clients")
	if code != 1 || !strings.Contains(stderr, "not a whatskept workspace") {
		t.Errorf("exit %d, stderr = %q", code, stderr)
	}
}

func TestCLIListLiveNotRunning(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init")
	code, stdout, stderr := run(t, dir, "list", "create", "Clients")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "`whatskept live` is not running") {
		t.Errorf("exit %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestCLIListThroughLive(t *testing.T) {
	dir := sendWorkspace(t)
	run(t, dir, "send", "+971500000001", "see you at 8") // the chat now exists
	db, err := sql.Open("sqlite", filepath.Join(dir, backup.ChatStorageName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The lists live last heard from WhatsApp.
	if _, err := db.Exec(`INSERT INTO wa_list (id, name, kind, shown, position) VALUES
		(1, 'Unread', 'unread', 1, 0), (2, 'Favorites', 'favorites', 1, 1), (3, 'Groups', 'groups', 1, 2)`); err != nil {
		t.Fatal(err)
	}
	lists := func() string {
		t.Helper()
		var s sql.NullString
		if err := db.QueryRow(`SELECT GROUP_CONCAT(id || ':' || name || ':' || COALESCE(position, '-'), ',')
			FROM (SELECT * FROM wa_list ORDER BY id)`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s.String
	}
	members := func() string {
		t.Helper()
		var s sql.NullString
		if err := db.QueryRow(`SELECT GROUP_CONCAT(list_id || ':' || jid, ',') FROM wa_list_chat`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s.String
	}
	expect := func(want string, args ...string) {
		t.Helper()
		code, stdout, stderr := run(t, dir, append([]string{"list"}, args...)...)
		if code != 0 || stdout != want+"\n" || stderr != "" {
			t.Fatalf("%v: exit %d, stdout = %q, stderr = %q", args, code, stdout, stderr)
		}
	}

	expect(`created list id=4 name="Clients"`, "create", "Clients")
	if got := lists(); got != "1:Unread:0,2:Favorites:1,3:Groups:2,4:Clients:3" {
		t.Fatalf("lists after create: %q", got)
	}
	expect(`added chat=971500000001@s.whatsapp.net to list id=4 name="Clients"`, "add", "clients", "+971 50 000 0001")
	if got := members(); got != "4:971500000001@s.whatsapp.net" {
		t.Fatalf("members after add: %q", got)
	}
	expect(`removed chat=971500000001@s.whatsapp.net from list id=4 name="Clients"`, "remove", "4", "971500000001@s.whatsapp.net")
	if got := members(); got != "" {
		t.Fatalf("members after remove: %q", got)
	}
	expect(`renamed list id=4 name="Customers"`, "rename", "Clients", "Customers")
	expect(`deleted list id=4 name="Customers"`, "delete", "Customers")
	if got := lists(); got != "1:Unread:0,2:Favorites:1,3:Groups:2" {
		t.Fatalf("lists after delete: %q", got)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"rename", "Favorites", "Faves"}, `"Favorites" is one of WhatsApp's built-in filters`},
		{[]string{"delete", "Nope"}, `no list "Nope"`},
		{[]string{"create", "groups"}, `a list named "Groups" already exists`},
		{[]string{"add", "Unread", "+971500000002"}, "is not in the database"},
	} {
		code, stdout, stderr := run(t, dir, append([]string{"list"}, c.args...)...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, c.want) {
			t.Errorf("%v: exit %d, stdout = %q, stderr = %q", c.args, code, stdout, stderr)
		}
	}
}
