package mcpserve

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"whatskept/internal/live"
	"whatskept/internal/views"
)

// writeFixtureDB builds a view-applied ChatStorage.sqlite with one DM
// chat: an incoming text, an outgoing reply, and a PDF document.
func writeFixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	const fixture = `
		CREATE TABLE ZWAMESSAGE (
			Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZMESSAGEDATE REAL,
			ZISFROMME INTEGER, ZMESSAGETYPE INTEGER, ZTEXT TEXT,
			ZPARENTMESSAGE INTEGER, ZSTANZAID TEXT, ZFROMJID TEXT, ZGROUPMEMBER INTEGER
		);
		CREATE TABLE ZWACHATSESSION (
			Z_PK INTEGER PRIMARY KEY, ZCONTACTJID TEXT, ZCONTACTIDENTIFIER TEXT,
			ZPARTNERNAME TEXT, ZMESSAGECOUNTER INTEGER, ZLASTMESSAGEDATE REAL, ZARCHIVED INTEGER
		);
		CREATE TABLE ZWAGROUPMEMBER (
			Z_PK INTEGER PRIMARY KEY, ZMEMBERJID TEXT, ZCONTACTNAME TEXT, ZFIRSTNAME TEXT
		);
		CREATE TABLE ZWAPROFILEPUSHNAME (ZJID TEXT, ZPUSHNAME TEXT);
		CREATE TABLE ZWAMEDIAITEM (
			Z_PK INTEGER PRIMARY KEY, ZMESSAGE INTEGER, ZMEDIALOCALPATH TEXT,
			ZAUTHORNAME TEXT, ZFILESIZE INTEGER, ZMEDIAURL TEXT, ZTITLE TEXT
		);
		INSERT INTO ZWACHATSESSION VALUES
			(1, '971501111111@s.whatsapp.net', NULL, 'Sarah', 3, 700000000, 0);
		INSERT INTO ZWAMESSAGE VALUES
			(1, 1, 700000000, 0, 0, 'lunch at the cafe tomorrow?', NULL, 's1', '971501111111@s.whatsapp.net', NULL),
			(2, 1, 700000100, 1, 0, 'sounds good', 1, 's2', NULL, NULL),
			(3, 1, 700000200, 0, 8, NULL, NULL, 's3', '971501111111@s.whatsapp.net', NULL);
		INSERT INTO ZWAMEDIAITEM VALUES
			(10, 3, 'Media/chat/scan.pdf', 'passport scan.pdf', 12345, NULL, NULL);`
	if _, err := db.Exec(fixture); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := views.Apply(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReactions(t *testing.T) {
	path := writeFixtureDB(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO wa_reaction VALUES
		('s1', '', 1, '❤️', '2026-01-01 10:00:00'),
		('s1', '971501111111@s.whatsapp.net', 0, '😂', '2026-01-01 10:01:00')`); err != nil {
		t.Fatal(err)
	}
	sess := connect(t, path)

	var hits searchOut
	call(t, sess, "search", map[string]any{"query": "cafe"}, &hits)
	if len(hits.Hits) != 1 || !strings.Contains(hits.Hits[0].Reactions, "❤️") || !strings.Contains(hits.Hits[0].Reactions, "😂") {
		t.Errorf("search hits = %+v", hits.Hits)
	}
	hits = searchOut{}
	call(t, sess, "search", map[string]any{"query": "passport"}, &hits)
	if len(hits.Hits) != 1 || hits.Hits[0].Reactions != "" {
		t.Errorf("unreacted hit = %+v", hits.Hits)
	}

	var out queryOut
	call(t, sess, "query", map[string]any{"sql": `SELECT sender_name, emoji, rowid, chat_title
		FROM v_reactions ORDER BY ts`}, &out)
	if len(out.Rows) != 2 || out.Rows[0][0] != "me" || out.Rows[1][0] != "Sarah" || out.Rows[1][1] != "😂" {
		t.Errorf("v_reactions = %+v", out.Rows)
	}

	var schema schemaOut
	call(t, sess, "get_schema", map[string]any{}, &schema)
	if !strings.Contains(schema.Schema, "v_reactions") {
		t.Error("get_schema doesn't show v_reactions")
	}
}

// An older database has no wa_reaction; search still has to work.
func TestSearchWithoutReactionTable(t *testing.T) {
	path := writeFixtureDB(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP VIEW v_reactions; DROP TABLE wa_reaction`); err != nil {
		t.Fatal(err)
	}
	var out searchOut
	call(t, connect(t, path), "search", map[string]any{"query": "cafe"}, &out)
	if len(out.Hits) != 1 {
		t.Errorf("hits = %+v", out.Hits)
	}
}

// connect runs the server over an in-memory transport and returns a
// connected client session.
func connect(t *testing.T, dbPath string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	go newMCPServer(dbPath).Run(ctx, serverT)
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// call invokes a tool and decodes its structured JSON output into out.
func call(t *testing.T, sess *mcp.ClientSession, tool string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError || out == nil {
		return res
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s: decode structured content: %v", tool, err)
	}
	return res
}

func errText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, " ")
}

func TestGetSchema(t *testing.T) {
	sess := connect(t, writeFixtureDB(t))
	var out schemaOut
	call(t, sess, "get_schema", nil, &out)
	for _, want := range []string{"CREATE VIEW v_messages", "CREATE VIEW v_chats", "messages_fts", "ZWAMESSAGE"} {
		if !strings.Contains(out.Schema, want) {
			t.Errorf("schema missing %q", want)
		}
	}
}

func TestQuery(t *testing.T) {
	sess := connect(t, writeFixtureDB(t))
	var out queryOut
	call(t, sess, "query",
		map[string]any{"sql": "SELECT sender_name, text FROM v_messages ORDER BY rowid"}, &out)
	if len(out.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(out.Rows))
	}
	if out.Rows[0][0] != "Sarah" || out.Rows[1][0] != "me" {
		t.Errorf("sender resolution: %v", out.Rows)
	}
	if out.Truncated {
		t.Error("unexpected truncation")
	}
}

func TestQueryLimitTruncates(t *testing.T) {
	sess := connect(t, writeFixtureDB(t))
	var out queryOut
	call(t, sess, "query",
		map[string]any{"sql": "SELECT rowid FROM v_messages", "limit": 1}, &out)
	if len(out.Rows) != 1 || !out.Truncated {
		t.Errorf("rows = %d truncated = %v, want 1/true", len(out.Rows), out.Truncated)
	}
}

func TestQueryRejectsWrites(t *testing.T) {
	sess := connect(t, writeFixtureDB(t))
	res := call(t, sess, "query",
		map[string]any{"sql": "DELETE FROM wa_contact"}, nil)
	if !res.IsError {
		t.Fatal("write must be rejected")
	}
}

func TestSearch(t *testing.T) {
	sess := connect(t, writeFixtureDB(t))
	var out searchOut
	call(t, sess, "search", map[string]any{"query": "café"}, &out)
	if len(out.Hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(out.Hits))
	}
	h := out.Hits[0]
	if h.Rowid != 1 || h.Sender != "Sarah" || h.Chat != "Sarah" {
		t.Errorf("hit = %+v", h)
	}
	if !strings.Contains(h.Snippet, "[[cafe]]") {
		t.Errorf("snippet = %q", h.Snippet)
	}

	// Document filename surface, COALESCEd into text.
	out = searchOut{}
	call(t, sess, "search", map[string]any{"query": "passport"}, &out)
	if len(out.Hits) != 1 || out.Hits[0].Rowid != 3 || out.Hits[0].Text != "passport scan.pdf" {
		t.Errorf("document hit = %+v", out.Hits)
	}
}

func TestSearchVideoShowsDescriptionAndSpeech(t *testing.T) {
	path := writeFixtureDB(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS wa_image_text (rowid INTEGER PRIMARY KEY, ocr_text TEXT, description TEXT,
			language TEXT, source TEXT, model TEXT, generated_at TEXT);
		CREATE TABLE IF NOT EXISTS wa_voice_text (rowid INTEGER PRIMARY KEY, transcript TEXT,
			language TEXT, duration_sec REAL, model TEXT, generated_at TEXT);
		INSERT INTO ZWAMESSAGE (Z_PK, ZCHATSESSION, ZMESSAGETYPE) VALUES (70, 1, 2);
		INSERT INTO wa_image_text VALUES (70, '', 'a hand points at a laptop', '', 'cloud', 'm', 't');
		INSERT INTO wa_voice_text VALUES (70, 'this is my keyboard', '', NULL, 'm', 't');
		INSERT INTO messages_fts (rowid, text) VALUES (70, 'a hand points at a laptop this is my keyboard');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var out searchOut
	call(t, connect(t, path), "search", map[string]any{"query": "keyboard"}, &out)
	if len(out.Hits) != 1 || out.Hits[0].Text != "a hand points at a laptop\nthis is my keyboard" {
		t.Errorf("hits = %+v", out.Hits)
	}
}

func TestToolsWithoutDatabase(t *testing.T) {
	sess := connect(t, filepath.Join(t.TempDir(), "ChatStorage.sqlite"))
	res := call(t, sess, "query", map[string]any{"sql": "SELECT 1"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "whatskept import") {
		t.Errorf("want import hint, got %q", errText(res))
	}
}

// bearer sets a fixed Authorization header on every request.
type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// TestHTTPTransport drives the handler over real HTTP: the bearer
// token is the gate, and the old token-in-path URL is gone.
func TestHTTPTransport(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	ts := httptest.NewServer(newHandler(newMCPServer(writeFixtureDB(t)), token, time.Now))
	t.Cleanup(ts.Close)

	for _, tc := range []struct {
		name, path, auth string
		want             int
	}{
		{"no header", "/mcp", "", http.StatusUnauthorized},
		{"wrong token", "/mcp", "Bearer wrong", http.StatusUnauthorized},
		{"empty bearer", "/mcp", "Bearer ", http.StatusUnauthorized},
		{"token in path", "/" + token + "/mcp", "", http.StatusNotFound},
	} {
		req, _ := http.NewRequest("POST", ts.URL+tc.path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}

	// Full MCP session, with and without the "Bearer " prefix.
	for _, auth := range []string{"Bearer " + token, "bearer " + token, token} {
		sess, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(
			context.Background(),
			&mcp.StreamableClientTransport{
				Endpoint:   ts.URL + "/mcp",
				HTTPClient: &http.Client{Transport: bearer(auth)},
			},
			nil,
		)
		if err != nil {
			t.Fatalf("auth %q: %v", auth, err)
		}
		var out searchOut
		call(t, sess, "search", map[string]any{"query": "café"}, &out)
		if len(out.Hits) != 1 || out.Hits[0].Rowid != 1 {
			t.Errorf("auth %q: hits over HTTP = %+v", auth, out.Hits)
		}
		sess.Close()
	}
}

// TestServeRequiresToken: no token, or a weak one, no server — one
// mode only.
func TestServeRequiresToken(t *testing.T) {
	for _, token := range []string{"", "tooshort"} {
		err := Serve(context.Background(), writeFixtureDB(t), "127.0.0.1:0", token)
		if err == nil || !strings.Contains(err.Error(), TokenEnv) {
			t.Errorf("token %q: err = %v, want mention of %s", token, err, TokenEnv)
		}
	}
}

// writeSendWorkspace grows the fixture into something live can write
// to: the columns its writer fills, and the .whatskept/ directory its
// send endpoint is advertised in.
func writeSendWorkspace(t *testing.T) string {
	t.Helper()
	path := writeFixtureDB(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		ALTER TABLE ZWAMESSAGE ADD COLUMN Z_ENT INTEGER;
		ALTER TABLE ZWAMESSAGE ADD COLUMN Z_OPT INTEGER;
		ALTER TABLE ZWAMESSAGE ADD COLUMN ZMESSAGESTATUS INTEGER;
		ALTER TABLE ZWAMESSAGE ADD COLUMN ZSENTDATE REAL;
		ALTER TABLE ZWAMESSAGE ADD COLUMN ZTOJID TEXT;
		ALTER TABLE ZWACHATSESSION ADD COLUMN Z_ENT INTEGER;
		ALTER TABLE ZWACHATSESSION ADD COLUMN Z_OPT INTEGER;
		ALTER TABLE ZWACHATSESSION ADD COLUMN ZSESSIONTYPE INTEGER;
		ALTER TABLE ZWACHATSESSION ADD COLUMN ZLASTMESSAGE INTEGER;
		ALTER TABLE ZWACHATSESSION ADD COLUMN ZLASTMESSAGETEXT TEXT;
		ALTER TABLE ZWAMEDIAITEM ADD COLUMN ZMOVIEDURATION INTEGER;
		ALTER TABLE ZWAMEDIAITEM ADD COLUMN ZMEDIAKEY BLOB;
		ALTER TABLE ZWAMEDIAITEM ADD COLUMN ZMETADATA BLOB;
		ALTER TABLE ZWAMEDIAITEM ADD COLUMN ZVCARDSTRING TEXT;
		CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER, Z_NAME TEXT, Z_SUPER INTEGER, Z_MAX INTEGER);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.Mkdir(filepath.Join(filepath.Dir(path), ".whatskept"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSend(t *testing.T) {
	path := writeSendWorkspace(t)
	stop, err := live.StartFakeSend(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	sess := connect(t, path)

	var sent live.Sent
	if res := call(t, sess, "send", map[string]any{"to": "+971501111111", "text": "running late, order for me"}, &sent); res.IsError {
		t.Fatalf("send: %s", errText(res))
	}
	if sent.ID != "FAKE1" || sent.Chat != "971501111111@s.whatsapp.net" || sent.Warning != "" {
		t.Errorf("sent = %+v", sent)
	}

	// The sent message is in the history at once — in Sarah's existing
	// chat, as ours, and searchable.
	var out queryOut
	call(t, sess, "query", map[string]any{
		"sql": "SELECT chat_title, is_from_me FROM v_messages WHERE text = 'running late, order for me'"}, &out)
	if len(out.Rows) != 1 || out.Rows[0][0] != "Sarah" || out.Rows[0][1] != float64(1) {
		t.Errorf("rows = %v", out.Rows)
	}
	var hits searchOut
	call(t, sess, "search", map[string]any{"query": "late"}, &hits)
	if len(hits.Hits) != 1 || hits.Hits[0].Chat != "Sarah" {
		t.Errorf("hits = %+v", hits.Hits)
	}

	// A reply names its target by stanza_id and shows up linked to it.
	if res := call(t, sess, "send", map[string]any{"to": "+971501111111", "text": "make it noon", "reply_to": "s1"}, &sent); res.IsError {
		t.Fatalf("reply: %s", errText(res))
	}
	call(t, sess, "query", map[string]any{
		"sql": "SELECT reply_to_id FROM v_messages WHERE text = 'make it noon'"}, &out)
	if len(out.Rows) != 1 || out.Rows[0][0] != float64(1) {
		t.Errorf("reply rows = %v, want reply_to_id 1", out.Rows)
	}
	res := call(t, sess, "send", map[string]any{"to": "+971501111111", "text": "hi", "reply_to": "nope"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "cannot reply") {
		t.Errorf("bad reply target: want an error, got %q", errText(res))
	}

	res = call(t, sess, "send", map[string]any{"to": "Sarah", "text": "hi"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "invalid chat") {
		t.Errorf("bad chat: want an error, got %q", errText(res))
	}

	// A forward names the original by stanza_id, from any chat, and
	// lands in the destination chat.
	if res := call(t, sess, "send", map[string]any{"to": "+971502222222", "forward": "s1"}, &sent); res.IsError {
		t.Fatalf("forward: %s", errText(res))
	}
	call(t, sess, "query", map[string]any{"sql": "SELECT text, is_from_me FROM v_messages WHERE stanza_id = '" + sent.ID + "'"}, &out)
	if len(out.Rows) != 1 || out.Rows[0][0] != "lunch at the cafe tomorrow?" || out.Rows[0][1] != float64(1) {
		t.Errorf("forward rows = %v", out.Rows)
	}
	res = call(t, sess, "send", map[string]any{"to": "+971502222222", "forward": "s1", "text": "look"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "leave text and reply_to empty") {
		t.Errorf("forward with text: want an error, got %q", errText(res))
	}
	res = call(t, sess, "send", map[string]any{"to": "+971502222222", "forward": "s3"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "cannot forward") {
		t.Errorf("forward without stored download details: want an error, got %q", errText(res))
	}
}

func TestArchive(t *testing.T) {
	path := writeSendWorkspace(t)
	stop, err := live.StartFakeSend(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	sess := connect(t, path)

	var got live.Archived
	if res := call(t, sess, "archive", map[string]any{"chat": "+971501111111"}, &got); res.IsError {
		t.Fatalf("archive: %s", errText(res))
	}
	if got.Chat != "971501111111@s.whatsapp.net" || !got.Archived || got.Warning != "" {
		t.Errorf("archive = %+v", got)
	}
	// The change is in v_chats at once.
	var out queryOut
	call(t, sess, "query", map[string]any{"sql": "SELECT archived FROM v_chats WHERE title = 'Sarah'"}, &out)
	if len(out.Rows) != 1 || out.Rows[0][0] != float64(1) {
		t.Errorf("rows = %v, want archived 1", out.Rows)
	}

	if res := call(t, sess, "archive", map[string]any{"chat": "971501111111@s.whatsapp.net", "undo": true}, &got); res.IsError {
		t.Fatalf("unarchive: %s", errText(res))
	}
	if got.Archived {
		t.Errorf("unarchive = %+v", got)
	}
	call(t, sess, "query", map[string]any{"sql": "SELECT archived FROM v_chats WHERE title = 'Sarah'"}, &out)
	if len(out.Rows) != 1 || out.Rows[0][0] != float64(0) {
		t.Errorf("rows = %v, want archived 0", out.Rows)
	}

	res := call(t, sess, "archive", map[string]any{"chat": "+971509999999"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "is not in the database") {
		t.Errorf("unknown chat: want an error, got %q", errText(res))
	}
	res = call(t, sess, "archive", map[string]any{"chat": "Sarah"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "invalid chat") {
		t.Errorf("bad chat: want an error, got %q", errText(res))
	}
}

func TestArchiveWithoutLive(t *testing.T) {
	sess := connect(t, writeSendWorkspace(t))
	res := call(t, sess, "archive", map[string]any{"chat": "+971501111111"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "`whatskept live` is not running") {
		t.Errorf("want not-running error, got %q", errText(res))
	}
}

// Without live there is nobody to send: the tool says so, and the
// query tools carry on.
func TestSendWithoutLive(t *testing.T) {
	sess := connect(t, writeSendWorkspace(t))
	res := call(t, sess, "send", map[string]any{"to": "+971501111111", "text": "hi"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "`whatskept live` is not running") {
		t.Errorf("want not-running error, got %q", errText(res))
	}
	var out queryOut
	call(t, sess, "query", map[string]any{"sql": "SELECT COUNT(*) FROM v_messages"}, &out)
	if len(out.Rows) != 1 || out.Rows[0][0] != float64(3) {
		t.Errorf("rows = %v, want the 3 fixture messages and nothing new", out.Rows)
	}
}

// TestHTTPProtocolVersions drives both handshakes over raw HTTP: the
// 2026-07-28 server/discover flow (ChatGPT) and the older initialize
// flow (claude.ai and most clients). Both must reach the tools.
func TestHTTPProtocolVersions(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	ts := httptest.NewServer(newHandler(newMCPServer(writeFixtureDB(t)), token, time.Now))
	t.Cleanup(ts.Close)

	// rpc posts one JSON-RPC message and returns the status and the
	// decoded result (SSE or JSON body).
	rpc := func(version, method string, id any, params map[string]any) (int, map[string]any) {
		t.Helper()
		msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
		if id != nil {
			msg["id"] = id
		}
		b, _ := json.Marshal(msg)
		req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", version)
		req.Header.Set("Mcp-Method", method)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		body := string(raw)
		if i := strings.Index(body, "data: "); i >= 0 {
			body = strings.TrimSpace(strings.SplitN(body[i+6:], "\n", 2)[0])
		}
		var out struct {
			Result map[string]any `json:"result"`
			Error  any            `json:"error"`
		}
		json.Unmarshal([]byte(body), &out)
		if out.Error != nil {
			t.Errorf("%s %s: error %v", version, method, out.Error)
		}
		return resp.StatusCode, out.Result
	}
	toolNames := func(res map[string]any) []string {
		var names []string
		tools, _ := res["tools"].([]any)
		for _, tool := range tools {
			names = append(names, tool.(map[string]any)["name"].(string))
		}
		return names
	}

	// ChatGPT: stateless discover, then tools/list, each carrying _meta.
	meta := map[string]any{"_meta": map[string]any{
		"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "openai-mcp", "version": "1.0.0"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}}
	status, res := rpc("2026-07-28", "server/discover", "d", meta)
	if versions, _ := res["supportedVersions"].([]any); status != 200 || len(versions) == 0 || versions[0] != "2026-07-28" {
		t.Errorf("discover: %d supportedVersions=%v, want 2026-07-28 first", status, res["supportedVersions"])
	}
	if status, res := rpc("2026-07-28", "tools/list", 2, meta); status != 200 || !slices.Contains(toolNames(res), "search") {
		t.Errorf("2026-07-28 tools/list: %d %v", status, toolNames(res))
	}

	// Older clients: initialize, then tools/list with no session id.
	status, res = rpc("2025-06-18", "initialize", 1, map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "claude-ai", "version": "1"},
	})
	if status != 200 || res["protocolVersion"] != "2025-06-18" {
		t.Errorf("initialize: %d protocolVersion=%v", status, res["protocolVersion"])
	}
	if status, res := rpc("2025-06-18", "tools/list", 2, map[string]any{}); status != 200 || len(toolNames(res)) != 5 {
		t.Errorf("2025-06-18 tools/list: %d %v", status, toolNames(res))
	}
}
