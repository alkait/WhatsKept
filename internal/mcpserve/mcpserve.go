// Package mcpserve exposes the workspace database over the Model
// Context Protocol (streamable HTTP, or stdio) — the only query surface of
// whatskept. Read-only by construction: the database is opened with
// mode=ro and PRAGMA query_only per tool call, and never held open, so
// a re-import can replace the file mid-serve without a restart.
//
// The server is deliberately schema-agnostic: get_schema hands the
// agent the real CREATE statements, query runs arbitrary read-only
// SQL, and search is a convenience wrapper over messages_fts.
//
// send, archive, read and list are the tools that are not queries: they
// hand a message, an archive, read or list change to the `whatskept
// live` process running beside the database, which makes it. The server
// itself still never writes.
package mcpserve

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"

	"whatskept/internal/live"
)

//go:embed instructions.md
var instructions string

const (
	queryTimeout  = 15 * time.Second
	defaultRowCap = 200
	maxRowCap     = 500
	cellByteCap   = 4_000
	resultByteCap = 256 * 1024
	defaultHits   = 25
	maxHits       = 100
)

type server struct {
	// dbPath is re-checked on every open so the file may be replaced
	// by a re-import while the server runs.
	dbPath string
}

// Serve runs the MCP server over streamable HTTP on addr until ctx is
// cancelled. dbPath is the workspace's ChatStorage.sqlite; it may be
// absent (tools report "run whatskept import first"). The token is
// required — the endpoint is /mcp and every request must carry it as
// `Authorization: Bearer <token>`, or an access token from the OAuth
// flow that token approves. One mode, dev and production alike.
func Serve(ctx context.Context, dbPath, addr, token string) error {
	if token == "" {
		return errors.New("set " + TokenEnv + " — every request to the MCP endpoint must carry it as a bearer token")
	}
	if len(token) < minTokenLen {
		return fmt.Errorf("%s is too short — use at least %d characters, e.g. `openssl rand -hex 16`", TokenEnv, minTokenLen)
	}
	// Startup probe: report state but serve regardless — the workspace
	// may simply not be imported yet.
	dbState := dbPath
	s := &server{dbPath: dbPath}
	if db, err := s.openDB(); err != nil {
		dbState = fmt.Sprintf("NOT available (%v) — serving anyway", err)
	} else {
		db.Close()
	}

	httpServer := &http.Server{
		Addr:    addr,
		Handler: newHandler(newMCPServer(dbPath), token, time.Now),
		// No WriteTimeout: streamable HTTP holds SSE streams open.
		ReadHeaderTimeout: 10 * time.Second,
	}

	go watchReleases(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()
	// The token is never printed — logs are not a place for secrets.
	fmt.Printf("MCP endpoint:  http://%s/mcp (send header `Authorization: Bearer $%s`, or connect with OAuth and approve with it)\n", addr, TokenEnv)
	fmt.Printf("Database:      %s\n", dbState)
	fmt.Printf("Version:       %s\n", Version)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// ServeStdio runs the same MCP server over stdin/stdout until the
// client disconnects or ctx is cancelled — for clients that launch the
// server themselves. No listener, so no token: only the parent process
// can talk to it. stdout carries the protocol, so the startup notes go
// to stderr.
func ServeStdio(ctx context.Context, dbPath string) error {
	dbState := dbPath
	if db, err := (&server{dbPath: dbPath}).openDB(); err != nil {
		dbState = fmt.Sprintf("NOT available (%v) — serving anyway", err)
	} else {
		db.Close()
	}
	fmt.Fprintln(os.Stderr, "MCP over stdio")
	fmt.Fprintf(os.Stderr, "Database:      %s\n", dbState)
	fmt.Fprintf(os.Stderr, "Version:       %s\n", Version)
	go watchReleases(ctx)
	return newMCPServer(dbPath).Run(ctx, &mcp.StdioTransport{})
}

// TokenEnv is where Serve's callers read the auth token from.
const TokenEnv = "WHATSKEPT_MCP_TOKEN"

// minTokenLen is the shortest token Serve accepts — 32 hex characters
// is 128 bits.
const minTokenLen = 32

// newHandler routes the MCP endpoint at /mcp behind the bearer token
// or an OAuth access token (oauth.go) — the Authorization header is the
// sole credential, so the URL itself is not a secret.
func newHandler(mcpServer *mcp.Server, token string, now func() time.Time) http.Handler {
	// The SDK's DNS-rebinding protection 403s requests that arrive on a
	// localhost address with a non-localhost Host header — which is
	// exactly how every tunnel (cloudflared, ssh -R) delivers traffic.
	// The token is the gate, so lift it.
	//
	// Stateless: every tool call stands alone (the database is opened per
	// call), and the SDK serves protocol 2026-07-28 — which ChatGPT
	// requires — only on stateless servers. Older clients still connect.
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true, Stateless: true},
	)
	return newOAuth(token, now).routes(handler)
}

// newMCPServer builds the server with its tools registered —
// split from Serve so tests can run it over an in-memory transport.
func newMCPServer(dbPath string) *mcp.Server {
	s := &server{dbPath: dbPath}

	mcpServer := mcp.NewServer(
		&mcp.Implementation{Name: "whatskept", Title: "WhatsKept", Version: Version},
		&mcp.ServerOptions{Instructions: versionedInstructions()},
	)
	mcpServer.AddReceivingMiddleware(versionMiddleware)
	addTool(mcpServer, &mcp.Tool{
		Name: "get_schema",
		Description: "Return the database schema: every CREATE TABLE / CREATE VIEW / " +
			"CREATE VIRTUAL TABLE statement. Call this once before writing SQL — the " +
			"views (v_messages, v_chats, v_lists, messages_fts, ...) are the intended query surface.",
	}, s.getSchema)
	addTool(mcpServer, &mcp.Tool{
		Name: "query",
		Description: "Run a read-only SQL statement against the WhatsApp workspace database " +
			"and return rows as JSON. Writes are blocked at the connection level. Always " +
			"aggregate or LIMIT; results are capped at " + fmt.Sprint(maxRowCap) + " rows.",
	}, s.query)
	addTool(mcpServer, &mcp.Tool{
		Name: "search",
		Description: "Full-text search across ALL surfaces — typed messages, image OCR and " +
			"descriptions, voice-note transcripts, document filenames and PDF body text. " +
			"FTS5 syntax: \"exact phrase\", AND, OR, NOT, NEAR/3, prefix*. Use this first " +
			"for any 'did anyone mention X' question; fall back to query for joins and aggregates.",
	}, s.search)
	addTool(mcpServer, &mcp.Tool{
		Name: "send",
		Description: "Send a WhatsApp text message from the user's own account, or forward one. " +
			"`to` is a phone number in international format or a chat JID (v_chats.jid — the only " +
			"way to address a group). Optional `reply_to` is the v_messages.stanza_id of a message " +
			"in that chat to quote. To forward instead, give `forward` (a v_messages.stanza_id from " +
			"any chat: text, link, image, video, GIF, sticker, voice note or document) and no `text`. Irreversible and " +
			"seen by real people: send only what the user explicitly asked for, to the chat they " +
			"named. Needs `whatskept live` running.",
	}, s.send)
	addTool(mcpServer, &mcp.Tool{
		Name: "archive",
		Description: "Archive a WhatsApp chat in the user's own account, or with `undo` unarchive it — " +
			"on their phone and every linked device. `chat` is a phone number in international format " +
			"or a chat JID (v_chats.jid — the only way to address a group); it must be a chat in " +
			"v_chats. Seen by nobody else and reversible, but it changes the user's chat list: archive " +
			"only the chats the user asked for. Needs `whatskept live` running.",
	}, s.archive)
	addTool(mcpServer, &mcp.Tool{
		Name: "read",
		Description: "Mark a WhatsApp chat as read in the user's own account, or with `undo` as unread — " +
			"clears (or sets) the unread badge on their phone and every linked device. No read receipts " +
			"are sent: senders see no blue ticks. `chat` is a phone number in international format or a " +
			"chat JID (v_chats.jid — the only way to address a group); it must be a chat in v_chats. " +
			"v_chats.unread reflects the change at once. " +
			"Mark only the chats the user asked for. Needs `whatskept live` running.",
	}, s.read)
	addTool(mcpServer, &mcp.Tool{
		Name: "list",
		Description: "Change the user's chat lists — the filters at the top of their WhatsApp chat list — " +
			"on their phone and every linked device. `op` is create (`name`), rename (`list`, `name`), " +
			"delete (`list`), add or remove (`list`, `chat`). `list` is a v_lists.id or name; only the " +
			"user's own lists (v_lists.kind = 'custom') can be changed, not WhatsApp's built-in filters. " +
			"`chat` is a phone number in international format or a chat JID; it must be a chat in v_chats. " +
			"v_lists and v_list_chats reflect the change at once. Seen by nobody else, but it changes " +
			"the user's lists: change only what the user asked for. Needs `whatskept live` running.",
	}, s.list)
	// TEMPORARY — exists only to test how a client picks up a tool a
	// release adds; remove it in the release after.
	addTool(mcpServer, &mcp.Tool{
		Name:        "ping",
		Description: "Temporary test tool: returns \"pong\". Call it only when the user asks for a ping.",
	}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, pingOut, error) {
		return nil, pingOut{Reply: "pong"}, nil
	})

	return mcpServer
}

type pingOut struct {
	Reply string `json:"reply"`
}

// openDB opens the database read-only for a single tool call. The
// caller must Close it.
func (s *server) openDB() (*sql.DB, error) {
	if info, err := os.Stat(s.dbPath); err != nil {
		return nil, errors.New("the workspace has no database yet — run `whatskept import <ios-backup-path>` first")
	} else if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not a database", s.dbPath)
	}
	dsn := "file:" + s.dbPath + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return db, nil
}

// existingTables reports which of the named tables/views exist, so
// search can adapt to a workspace whose enrichment hasn't run yet.
// Checked per call: a re-import or enrichment run may add tables and
// the server should pick that up without a restart.
func existingTables(ctx context.Context, db *sql.DB, names []string) (map[string]bool, error) {
	placeholders := strings.TrimRight(strings.Repeat("?,", len(names)), ",")
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type IN ('table','view') AND name IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// --- get_schema ---------------------------------------------------------

type schemaOut struct {
	Schema string `json:"schema" jsonschema:"the full DDL: CREATE TABLE / VIEW / VIRTUAL TABLE statements, semicolon-separated"`
}

func (s *server) getSchema(ctx context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, schemaOut, error) {
	db, err := s.openDB()
	if err != nil {
		return nil, schemaOut{}, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		`SELECT sql FROM sqlite_master WHERE sql IS NOT NULL
		 ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'view' THEN 1 ELSE 2 END, name`)
	if err != nil {
		return nil, schemaOut{}, err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var ddl string
		if err := rows.Scan(&ddl); err != nil {
			return nil, schemaOut{}, err
		}
		b.WriteString(ddl)
		b.WriteString(";\n\n")
	}
	return nil, schemaOut{Schema: b.String()}, rows.Err()
}

// --- query --------------------------------------------------------------

type queryIn struct {
	SQL   string `json:"sql" jsonschema:"a single read-only SQL statement (SELECT / WITH); always aggregate or LIMIT"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum rows to return, default 200, max 500"`
}

type queryOut struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated,omitempty" jsonschema:"true when the row or byte cap cut the result short — narrow the query"`
}

func (s *server) query(ctx context.Context, _ *mcp.CallToolRequest, in queryIn) (*mcp.CallToolResult, queryOut, error) {
	db, err := s.openDB()
	if err != nil {
		return nil, queryOut{}, err
	}
	defer db.Close()

	limit := in.Limit
	if limit <= 0 {
		limit = defaultRowCap
	}
	if limit > maxRowCap {
		limit = maxRowCap
	}

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	rows, err := db.QueryContext(ctx, in.SQL)
	if err != nil {
		return nil, queryOut{}, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, queryOut{}, err
	}

	out := queryOut{Columns: cols, Rows: [][]any{}}
	scan := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range scan {
		ptrs[i] = &scan[i]
	}
	sizeBudget := resultByteCap
	for rows.Next() {
		if len(out.Rows) >= limit {
			out.Truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, queryOut{}, err
		}
		row := make([]any, len(cols))
		for i, v := range scan {
			cell, n := normalizeCell(v)
			row[i] = cell
			sizeBudget -= n
		}
		out.Rows = append(out.Rows, row)
		if sizeBudget <= 0 {
			out.Truncated = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, queryOut{}, err
	}
	return nil, out, nil
}

// normalizeCell makes a driver value JSON-friendly and returns an
// approximate encoded size for the result byte cap. Long text cells are
// clipped so one verbatim OCR blob can't eat the whole budget.
func normalizeCell(v any) (any, int) {
	switch t := v.(type) {
	case nil:
		return nil, 4
	case []byte:
		return clipString(string(t))
	case string:
		return clipString(t)
	case time.Time:
		s := t.UTC().Format(time.RFC3339)
		return s, len(s)
	default:
		return t, 20
	}
}

func clipString(s string) (string, int) {
	if len(s) > cellByteCap {
		return s[:cellByteCap] + "…[clipped]", cellByteCap
	}
	return s, len(s)
}

// --- send ---------------------------------------------------------------

type sendIn struct {
	To   string `json:"to" jsonschema:"phone number in international format (+971501234567) or a chat JID from v_chats.jid"`
	Text string `json:"text,omitempty" jsonschema:"the message text; omit when forwarding"`
	// ReplyTo quotes an earlier message, the way a reply does in the app.
	ReplyTo string `json:"reply_to,omitempty" jsonschema:"optional: v_messages.stanza_id of a message in the same chat to reply to (quote)"`
	// Forward sends an existing message on, marked forwarded.
	Forward string `json:"forward,omitempty" jsonschema:"optional: v_messages.stanza_id of a message (any chat) to forward to 'to' — instead of text"`
}

// send hands the message to the live process of the workspace the
// database sits in; live owns the WhatsApp connection.
func (s *server) send(ctx context.Context, _ *mcp.CallToolRequest, in sendIn) (*mcp.CallToolResult, live.Sent, error) {
	root := filepath.Dir(s.dbPath)
	if in.Forward == "" {
		sent, err := live.Send(ctx, root, in.To, in.Text, in.ReplyTo)
		return nil, sent, err
	}
	if in.Text != "" || in.ReplyTo != "" {
		return nil, live.Sent{}, errors.New("a forward sends the original as it is — leave text and reply_to empty")
	}
	sent, err := live.Forward(ctx, root, in.To, in.Forward)
	return nil, sent, err
}

// --- archive ------------------------------------------------------------

type archiveIn struct {
	Chat string `json:"chat" jsonschema:"phone number in international format (+971501234567) or a chat JID from v_chats.jid"`
	Undo bool   `json:"undo,omitempty" jsonschema:"optional: true unarchives the chat instead"`
}

// archive hands the change to the live process of the workspace the
// database sits in; live owns the WhatsApp connection.
func (s *server) archive(ctx context.Context, _ *mcp.CallToolRequest, in archiveIn) (*mcp.CallToolResult, live.Archived, error) {
	got, err := live.Archive(ctx, filepath.Dir(s.dbPath), in.Chat, !in.Undo)
	return nil, got, err
}

// --- read ---------------------------------------------------------------

type readIn struct {
	Chat string `json:"chat" jsonschema:"phone number in international format (+971501234567) or a chat JID from v_chats.jid"`
	Undo bool   `json:"undo,omitempty" jsonschema:"optional: true marks the chat unread instead"`
}

// read hands the change to the live process of the workspace the
// database sits in; live owns the WhatsApp connection.
func (s *server) read(ctx context.Context, _ *mcp.CallToolRequest, in readIn) (*mcp.CallToolResult, live.MarkedRead, error) {
	got, err := live.MarkRead(ctx, filepath.Dir(s.dbPath), in.Chat, !in.Undo)
	return nil, got, err
}

// --- list ---------------------------------------------------------------

type listIn struct {
	Op   string `json:"op" jsonschema:"create, rename, delete, add or remove"`
	List string `json:"list,omitempty" jsonschema:"the list to change: its v_lists.id or name (not for create)"`
	Name string `json:"name,omitempty" jsonschema:"create and rename: the list's (new) name"`
	Chat string `json:"chat,omitempty" jsonschema:"add and remove: phone number in international format (+971501234567) or a chat JID from v_chats.jid"`
}

// list hands the change to the live process of the workspace the
// database sits in; live owns the WhatsApp connection.
func (s *server) list(ctx context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, live.ListChange, error) {
	got, err := live.ManageList(ctx, filepath.Dir(s.dbPath), in.Op, in.List, in.Name, in.Chat)
	return nil, got, err
}

// --- search -------------------------------------------------------------

type searchIn struct {
	Query string `json:"query" jsonschema:"FTS5 match expression, e.g. 'kitchen AND budget', '\"exact phrase\"', 'pizz*'"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum hits, default 25, max 100"`
}

type searchHit struct {
	Rowid   int64  `json:"rowid"`
	Ts      string `json:"ts"`
	Chat    string `json:"chat"`
	Sender  string `json:"sender"`
	Type    string `json:"type" jsonschema:"message type: text, image, audio, document, link, ..."`
	Text    string `json:"text" jsonschema:"the matched surface: typed text, image description/OCR, voice transcript, document text, or filename"`
	Snippet string `json:"snippet" jsonschema:"FTS snippet with [[ ]] around matched terms"`

	Reactions string `json:"reactions,omitempty" jsonschema:"emoji reactions to this message, space-separated; v_reactions says who"`
}

type searchOut struct {
	Hits []searchHit `json:"hits"`
}

func (s *server) search(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
	db, err := s.openDB()
	if err != nil {
		return nil, searchOut{}, err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	has, err := existingTables(ctx, db, []string{
		"messages_fts", "v_messages", "wa_image_text", "wa_voice_text", "wa_document", "wa_document_text", "wa_reaction",
	})
	if err != nil {
		return nil, searchOut{}, err
	}
	if !has["v_messages"] || !has["messages_fts"] {
		return nil, searchOut{}, errors.New("this database has no v_messages/messages_fts — re-run `whatskept import`")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultHits
	}
	if limit > maxHits {
		limit = maxHits
	}

	// The COALESCE chain surfaces whichever text layer produced the FTS
	// hit; the optional enrichment joins are included only when the
	// tables exist (SQLite errors on a missing table, it does not null it).
	sel := []string{"NULLIF(m.text, '')"}
	joins := []string{}
	if has["wa_image_text"] && has["wa_voice_text"] {
		// A video has both; one alone would hide the other.
		sel = append(sel, "NULLIF(t.description, '') || char(10) || NULLIF(v.transcript, '')")
	}
	if has["wa_image_text"] {
		sel = append(sel, "NULLIF(t.description, '')", "NULLIF(t.ocr_text, '')")
		joins = append(joins, "LEFT JOIN wa_image_text t ON t.rowid = m.rowid")
	}
	if has["wa_voice_text"] {
		sel = append(sel, "NULLIF(v.transcript, '')")
		joins = append(joins, "LEFT JOIN wa_voice_text v ON v.rowid = m.rowid")
	}
	if has["wa_document_text"] {
		sel = append(sel, "NULLIF(dt.text, '')")
		joins = append(joins, "LEFT JOIN wa_document_text dt ON dt.rowid = m.rowid")
	}
	if has["wa_document"] {
		sel = append(sel, "d.filename")
		joins = append(joins, "LEFT JOIN wa_document d ON d.rowid = m.rowid")
	}
	sel = append(sel, "m.link_title", "''")

	// A database no live or import has touched since reactions landed
	// has no table yet.
	reactions := "NULL"
	if has["wa_reaction"] {
		reactions = "(SELECT group_concat(r.emoji, ' ') FROM wa_reaction r WHERE r.target_stanza = m.stanza_id)"
	}

	q := fmt.Sprintf(`
		SELECT m.rowid, m.ts, m.chat_title, m.sender_name, m.message_type_name,
		       COALESCE(%s) AS hit_text,
		       snippet(messages_fts, 0, '[[', ']]', '…', 12) AS hit_snippet,
		       %s AS reactions
		FROM   messages_fts f
		JOIN   v_messages m ON m.rowid = f.rowid
		%s
		WHERE  messages_fts MATCH ?
		ORDER  BY m.ts DESC
		LIMIT  ?`, strings.Join(sel, ",\n"), reactions, strings.Join(joins, "\n"))

	rows, err := db.QueryContext(ctx, q, in.Query, limit)
	if err != nil {
		return nil, searchOut{}, err
	}
	defer rows.Close()

	out := searchOut{Hits: []searchHit{}}
	for rows.Next() {
		var h searchHit
		var ts, chat, sender, typ, text, snip, react sql.NullString
		if err := rows.Scan(&h.Rowid, &ts, &chat, &sender, &typ, &text, &snip, &react); err != nil {
			return nil, searchOut{}, err
		}
		h.Ts, h.Chat, h.Sender, h.Type = ts.String, chat.String, sender.String, typ.String
		h.Text, _ = clipString(text.String)
		h.Snippet = snip.String
		h.Reactions = react.String
		out.Hits = append(out.Hits, h)
	}
	return nil, out, rows.Err()
}
