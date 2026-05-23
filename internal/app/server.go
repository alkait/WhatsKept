package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"whatskept/internal/app/web"
	"whatskept/internal/backup"
	"whatskept/internal/binding"
	"whatskept/internal/helpers"
	"whatskept/internal/postprocess"

	_ "github.com/mattn/go-sqlite3"
)

// server is the HTTP backend. It mirrors the Python FastAPI surface
// closely enough that the existing React code in index.html works
// unchanged. Only the Backups tab is wired in this iteration; the
// Database routes return safe stubs.
type server struct {
	listener   net.Listener
	httpServer *http.Server
	url        string

	ws   *workspaceState
	jobs *jobManager
	pw   *passwordStore // session-only iOS-backup password cache
}

// newServer binds a free localhost TCP port, builds the route table,
// and returns a server ready to start. Call Start() to begin serving.
func newServer() (*server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind localhost: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	s := &server{
		listener: ln,
		url:      fmt.Sprintf("http://127.0.0.1:%d/", port),
		ws:       newWorkspaceState(),
		jobs:     newJobManager(),
		pw:       newPasswordStore(),
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

// Start begins serving in a background goroutine and returns immediately.
//
// Also kicks off a background scan for orphan idevicebackup2 processes
// (e.g. one left running by a previous app crash). Any orphan found is
// adopted into the job manager so the UI can re-attach to it.
func (s *server) Start() {
	go func() {
		if err := s.httpServer.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Best-effort log; the window will surface the failure when
			// /api requests start failing.
			fmt.Fprintln(os.Stderr, "app server error:", err)
		}
	}()
	go s.jobs.adoptOrphans()
}

// Shutdown gracefully stops the HTTP server.
func (s *server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *server) URL() string { return s.url }

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

func (s *server) registerRoutes(mux *http.ServeMux) {
	// Workspace
	mux.HandleFunc("GET /api/workspace/recent", s.handleRecentWorkspaces)
	mux.HandleFunc("POST /api/workspace/create", s.handleCreateWorkspace)
	mux.HandleFunc("POST /api/workspace/open", s.handleOpenWorkspace)
	mux.HandleFunc("GET /api/workspace/current", s.handleCurrentWorkspace)
	mux.HandleFunc("DELETE /api/workspace/current", s.handleDeleteWorkspace)

	// Devices + backups
	mux.HandleFunc("GET /api/devices", s.handleListDevices)
	mux.HandleFunc("GET /api/backups", s.handleListBackups)
	mux.HandleFunc("DELETE /api/backups/{udid}", s.handleDeleteBackup)

	// Jobs (stubbed in Phase A, real in Phase D)
	mux.HandleFunc("POST /api/backup/run", s.handleRunBackup)
	mux.HandleFunc("GET /api/jobs/active", s.handleActiveJob)
	mux.HandleFunc("GET /api/stream/{job_id}", s.handleStreamJob)

	// Database routes — the Database tab fetches /status on mount and
	// POSTs /sync to kick off the messages pipeline. /sync runs the
	// work in-process (no CLI re-exec) and streams progress through
	// the same SSE machinery as backups.
	mux.HandleFunc("GET /api/database/status", s.handleDatabaseStatus)
	mux.HandleFunc("POST /api/database/sync", s.handleSyncDatabase)

	// Agents — list supported agents (with installed flag) and launch
	// one with the current workspace as its working folder.
	mux.HandleFunc("GET /api/agents", s.handleListAgents)
	mux.HandleFunc("POST /api/agents/{id}/open", s.handleOpenAgent)

	// Session password — lets the UI skip the modal when a backup
	// password is already cached, and lets it explicitly forget one
	// (e.g. after a typo). The password value is never returned.
	mux.HandleFunc("GET /api/session/password", s.handleSessionPasswordStatus)
	mux.HandleFunc("DELETE /api/session/password", s.handleSessionPasswordClear)

	// Workspace binding — the on-disk identity record (.whatskept.json).
	// Only DELETE is exposed; reads come back as part of /api/workspace/current
	// so the UI gets binding and workspace state in one round-trip.
	mux.HandleFunc("DELETE /api/binding", s.handleForgetBinding)

	// Static files (the embedded React UI). Must be registered last so
	// /api/* takes precedence on the same mux.
	sub, err := fs.Sub(web.FS, ".")
	if err != nil {
		panic(fmt.Errorf("embed sub-fs: %w", err)) // unreachable
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		_, _ = w.Write([]byte("null"))
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		// Already past WriteHeader; nothing useful we can do.
		fmt.Fprintln(os.Stderr, "json encode:", err)
	}
}

// httpError mirrors FastAPI's `{"detail": "<msg>"}` shape so the React
// `api.*` helpers see the same error format as before.
func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"detail": msg})
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

func expandTilde(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// ---------------------------------------------------------------------------
// Workspace handlers
// ---------------------------------------------------------------------------

func (s *server) handleRecentWorkspaces(w http.ResponseWriter, _ *http.Request) {
	paths := loadRecent()
	out := make([]workspaceInfo, 0, len(paths))
	for _, p := range paths {
		out = append(out, describeWorkspace(p))
	}
	writeJSON(w, http.StatusOK, out)
}

type createWorkspaceRequest struct {
	Path string `json:"path"`
}

func (s *server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var req createWorkspaceRequest
	if err := decodeJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Path == "" {
		httpError(w, http.StatusBadRequest, "path is required")
		return
	}
	abs, err := filepath.Abs(expandTilde(req.Path))
	if err != nil {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("invalid path: %v", err))
		return
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("Cannot create directory: %v", err))
		return
	}
	// Note: workspace creation deliberately writes nothing else into the
	// directory. ChatStorage.sqlite, views.sql, and AGENTS.md are produced
	// later by `whatskept extract` and the postprocess step. Backup
	// password persistence happens on demand from the password modal,
	// not here, since (a) most users don't know what it is at create
	// time, and (b) the modal already exists for the only operation
	// that immediately needs it (running a fresh backup).
	s.ws.set(abs)
	s.pw.clear()
	addRecent(abs)
	writeJSON(w, http.StatusOK, describeWorkspace(abs))
}

type openWorkspaceRequest struct {
	Path string `json:"path"`
}

func (s *server) handleOpenWorkspace(w http.ResponseWriter, r *http.Request) {
	var req openWorkspaceRequest
	if err := decodeJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	abs, err := filepath.Abs(expandTilde(req.Path))
	if err != nil {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("invalid path: %v", err))
		return
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		httpError(w, http.StatusNotFound, "Directory not found")
		return
	}
	s.ws.set(abs)
	s.pw.clear()
	addRecent(abs)
	writeJSON(w, http.StatusOK, describeWorkspace(abs))
}

func (s *server) handleCurrentWorkspace(w http.ResponseWriter, _ *http.Request) {
	cur := s.ws.get()
	if cur == "" {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, describeWorkspace(cur))
}

// handleDeleteWorkspace permanently removes the active workspace's
// directory tree (ChatStorage.sqlite, .whatskept.json, views.sql,
// AGENTS.md, any media/voice/profiles subfolders, the .env, plus
// anything else the user dropped in), drops it from the recent
// list, clears the active-workspace pointer, and clears the cached
// backup password.
//
// Refuses while any job is still running: deleting files out from
// under an in-flight sync would either corrupt the staging DB or
// leave half-written media on disk, both of which are worse than a
// 409 telling the user to wait.
//
// Returns 204 on success. The frontend is expected to navigate back
// to the workspace picker after observing the 204 — the in-memory
// active workspace pointer is empty by then anyway.
func (s *server) handleDeleteWorkspace(w http.ResponseWriter, _ *http.Request) {
	cur := s.ws.get()
	if cur == "" {
		httpError(w, http.StatusBadRequest, "no workspace open")
		return
	}
	if active := s.jobs.activeJob(); active != nil {
		httpError(w, http.StatusConflict,
			fmt.Sprintf("a %s job is still running; wait for it to finish before deleting the workspace", active.Task))
		return
	}

	// Sanity-guard the path. We never accept a user-supplied path here
	// (the active workspace was vetted by handleOpenWorkspace /
	// handleCreateWorkspace), but a future bug could conceivably let
	// an empty or root-y path through to RemoveAll. Refuse anything
	// not absolute or suspiciously short — a real workspace path is
	// always many segments long.
	abs, err := filepath.Abs(cur)
	if err != nil || abs != cur || len(strings.Trim(abs, "/")) < 3 {
		httpError(w, http.StatusInternalServerError, "refusing to delete suspicious workspace path")
		return
	}

	if err := os.RemoveAll(cur); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("delete workspace: %v", err))
		return
	}
	removeRecent(cur)
	s.ws.set("")
	s.pw.clear()
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Device + backup handlers
// ---------------------------------------------------------------------------

type deviceItem struct {
	UDID string `json:"udid"`
	Name string `json:"name"`
}

func (s *server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	network := r.URL.Query().Get("network") == "true"

	args := []string{"-l"}
	if network {
		args = append(args, "-n")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	cmd, err := helpers.Command(ctx, helpers.IdeviceID, args...)
	if err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("idevice_id unavailable: %v", err))
		return
	}
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		httpError(w, http.StatusInternalServerError,
			fmt.Sprintf("idevice_id failed: %v %s", err, stderr))
		return
	}

	udids := strings.Split(strings.TrimSpace(string(out)), "\n")
	items := make([]deviceItem, 0, len(udids))
	for _, u := range udids {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		items = append(items, deviceItem{UDID: u, Name: deviceName(r.Context(), u)})
	}
	writeJSON(w, http.StatusOK, items)
}

// deviceName runs `idevice_id <udid>` to get the device's display name.
// Returns the UDID if the lookup fails (for any reason).
func deviceName(ctx context.Context, udid string) string {
	subCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd, err := helpers.Command(subCtx, helpers.IdeviceID, udid)
	if err != nil {
		return udid
	}
	out, err := cmd.Output()
	if err != nil {
		return udid
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return udid
	}
	return name
}

type backupItem struct {
	UDID           string `json:"udid"`
	Path           string `json:"path"`
	DeviceName     string `json:"device_name"`
	ProductType    string `json:"product_type"`
	ProductVersion string `json:"product_version"`
	LastBackup     string `json:"last_backup"`
	IsEncrypted    bool   `json:"is_encrypted"`
}

func (s *server) handleListBackups(w http.ResponseWriter, _ *http.Request) {
	root := backup.DefaultRoot()
	backups, err := backup.Discover(root)
	if err != nil {
		if errors.Is(err, backup.ErrAccessDenied) {
			httpError(w, http.StatusForbidden, fmt.Sprintf("Cannot read backup directory: %v", err))
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]backupItem, 0, len(backups))
	for _, b := range backups {
		out = append(out, backupItem{
			UDID:           filepath.Base(b.Path),
			Path:           b.Path,
			DeviceName:     b.DeviceName,
			ProductType:    b.ProductType,
			ProductVersion: b.ProductVersion,
			LastBackup:     b.LastBackupString(),
			IsEncrypted:    b.IsEncrypted,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	udid := r.PathValue("udid")
	if udid == "" {
		httpError(w, http.StatusBadRequest, "udid is required")
		return
	}
	root := backup.DefaultRoot()
	target := filepath.Join(root, udid)

	// Defence in depth: refuse if `target` would escape `root` after
	// path cleaning.
	rootAbs, _ := filepath.Abs(root)
	targetAbs, _ := filepath.Abs(target)
	if filepath.Dir(targetAbs) != rootAbs {
		httpError(w, http.StatusBadRequest, "Invalid backup path")
		return
	}
	if _, err := os.Stat(filepath.Join(target, "Info.plist")); err != nil {
		httpError(w, http.StatusNotFound, "Backup not found")
		return
	}
	if err := os.RemoveAll(target); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("Could not delete backup: %v", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Job handlers
// ---------------------------------------------------------------------------

type runBackupRequest struct {
	UDID     string `json:"udid"`
	Network  bool   `json:"network"`
	Password string `json:"password"`
}

type jobResponse struct {
	JobID string `json:"job_id"`
}

func (s *server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	var req runBackupRequest
	if err := decodeJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	root := backup.DefaultRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("cannot create backup root: %v", err))
		return
	}

	// Determine the password to attempt without yet committing to the
	// session cache. If the user supplied one, prefer it (this is the
	// "retry with a different password" path); otherwise fall back to
	// whatever's currently cached from a previous successful run.
	//
	// We deliberately do NOT call s.pw.set(req.Password) here. Caching
	// before idevicebackup2 has verified the password means a typo
	// poisons the cache: the modal-skip logic on the frontend then
	// short-circuits subsequent attempts straight to the same wrong
	// password, with no way for the user to enter a correct one
	// without manually clicking "Use a different password".
	attempted := req.Password
	if attempted == "" {
		attempted = s.pw.get()
	}

	// Commit the password to the session cache only once the backup
	// process exits cleanly (status=="ok"). pumpProcess invokes this
	// hook synchronously between the wait-returns and the SSE "done"
	// event, so the GET /api/session/password call the frontend
	// issues on "done" observes the post-success state.
	onDone := func(ok bool) {
		if ok && attempted != "" {
			s.pw.set(attempted)
		}
	}

	jobID, err := s.jobs.startBackup(req.UDID, req.Network, attempted, root, helpers.Command, onDone)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, jobResponse{JobID: jobID})
}

func (s *server) handleActiveJob(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.jobs.activeJob())
}

// handleStreamJob serves a Server-Sent Events feed of one job's
// output. Subscribes to the job's event broadcaster, writes each
// event as `data: <json>\n\n`, flushes, and closes when the channel
// closes (i.e. the job finishes) or the client disconnects.
func (s *server) handleStreamJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	j, ok := s.jobs.get(jobID)
	if !ok {
		httpError(w, http.StatusNotFound, "no such job")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming not supported by this server")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := j.subscribe()

	// SSE heartbeat. Without this, a long-running in-process step that
	// emits no log lines (e.g. ApplyViews populating a 250k-row FTS5
	// index, which is a single blocking SQLite call) will let the
	// connection sit silent for tens of seconds. WebKit / intermediate
	// buffers can close idle SSE streams, which manifests in the UI
	// as a false "sync failed" — the goroutine actually completed,
	// but the browser EventSource fired onerror before the terminal
	// "done" event arrived.
	//
	// SSE comment lines (anything starting with ":") are silently
	// dropped by EventSource clients, so they're free to use as
	// keepalives — they don't show up in the log, but they keep the
	// TCP pipe warm and give us a write to detect a client hangup.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case ev, ok := <-sub:
			if !ok {
				return // job done; channel closed by jobManager
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return // client disconnected
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return // client disconnected; bail so we stop emitting
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Database handlers
// ---------------------------------------------------------------------------

// databaseStatus is the JSON shape consumed by the Database tab. It
// gives the UI enough state to:
//   - Show row counts when the DB exists.
//   - Distinguish "never synced" vs "synced once but a newer backup
//     exists" vs "fully up to date" (via the is_stale flag, computed
//     server-side so the UI doesn't need to parse two timestamps).
//   - Tell the user to run a backup first when has_backups is false.
type databaseStatus struct {
	DBExists       bool   `json:"db_exists"`
	DBPath         string `json:"db_path,omitempty"`
	MessageCount   *int   `json:"message_count,omitempty"`
	FTSCount       *int   `json:"fts_count,omitempty"`
	AvatarCount    *int   `json:"avatar_count,omitempty"`     // wa_profile_picture row count
	LastSyncedAt   string `json:"last_synced_at,omitempty"`   // RFC3339, ChatStorage.sqlite mtime
	LatestBackupAt string `json:"latest_backup_at,omitempty"` // RFC3339, max(b.LastBackup) over encrypted backups
	IsStale        bool   `json:"is_stale"`                   // last_synced_at < latest_backup_at
	HasBackups     bool   `json:"has_backups"`                // any encrypted backup found at all
}

func (s *server) handleDatabaseStatus(w http.ResponseWriter, _ *http.Request) {
	out := databaseStatus{}

	cur := s.ws.get()

	// If the workspace is bound, scope "latest backup" and "has backups"
	// to that device's UDID. An iPad backup taken five minutes ago is
	// not relevant to a workspace bound to an iPhone, and surfacing it
	// as "new data available" would be misleading.
	var bound *binding.Binding
	if cur != "" {
		bound, _ = binding.Load(cur)
	}

	if infos, err := backup.Discover(backup.DefaultRoot()); err == nil {
		var latest time.Time
		for _, b := range infos {
			if !b.IsEncrypted {
				continue
			}
			if bound != nil && filepath.Base(b.Path) != bound.UDID {
				continue
			}
			out.HasBackups = true
			if b.LastBackup.After(latest) {
				latest = b.LastBackup
			}
		}
		if !latest.IsZero() {
			out.LatestBackupAt = latest.Format(time.RFC3339)
		}
	}

	if cur == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}

	dbPath := filepath.Join(cur, "ChatStorage.sqlite")
	st, err := os.Stat(dbPath)
	if err != nil {
		// Workspace exists but no DB yet. Stale = there's a backup we
		// haven't ingested.
		out.IsStale = out.HasBackups
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.DBExists = true
	out.DBPath = dbPath
	out.LastSyncedAt = st.ModTime().Format(time.RFC3339)

	// is_stale: a newer backup exists than our last sync. Compare in
	// time.Time space to avoid string-comparison subtleties (RFC3339
	// happens to be lexicographically comparable, but only for the
	// same offset — relying on that is fragile).
	if out.LatestBackupAt != "" {
		if t, err := time.Parse(time.RFC3339, out.LatestBackupAt); err == nil {
			if st.ModTime().Before(t) {
				out.IsStale = true
			}
		}
	}

	// Best-effort counts. A failure here doesn't sink the response —
	// the UI just hides those fields.
	msgs, fts, avatars := readDBCounts(dbPath)
	if msgs >= 0 {
		out.MessageCount = &msgs
	}
	if fts >= 0 {
		out.FTSCount = &fts
	}
	if avatars >= 0 {
		out.AvatarCount = &avatars
	}

	writeJSON(w, http.StatusOK, out)
}

// readDBCounts opens dbPath read-only and returns (messages, fts,
// avatars). Returns (-1, -1, -1) entirely on open failure; any field
// returns -1 individually if its table is missing (FTS may be absent
// on a DB that has been decrypted but not yet had views applied;
// wa_profile_picture is absent until the first sync run).
func readDBCounts(dbPath string) (msgs, fts, avatars int) {
	msgs, fts, avatars = -1, -1, -1
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ZWAMESSAGE`).Scan(&n); err == nil {
		msgs = n
	}
	var f int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&f); err == nil {
		fts = f
	}
	var a int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wa_profile_picture`).Scan(&a); err == nil {
		avatars = a
	}
	return
}

type syncDatabaseRequest struct {
	Password string `json:"password"`
}

// handleSyncDatabase kicks off postprocess.SyncMessages as an
// in-process SSE job. No `task` discriminator — there's exactly
// one operation this endpoint runs.
//
// The UI subscribes to /api/stream/{job_id} immediately and renders
// each "line" event in the Database card's live log.
func (s *server) handleSyncDatabase(w http.ResponseWriter, r *http.Request) {
	cur := s.ws.get()
	if cur == "" {
		httpError(w, http.StatusBadRequest, "no workspace open")
		return
	}

	var req syncDatabaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Same defer-cache rule as handleRunBackup: a freshly-supplied
	// password is only persisted to the session cache after the sync
	// (which decrypts the backup as its very first step) succeeds.
	// Caching upfront let typos lock the user out of retrying.
	attempted := req.Password
	if attempted == "" {
		attempted = s.pw.get()
	}
	if attempted == "" {
		httpError(w, http.StatusBadRequest, "backup password is required")
		return
	}

	jobID := s.jobs.startInProcess("messages-sync", func(log func(string)) error {
		_, err := postprocess.SyncMessages(
			backup.DefaultRoot(),
			cur,
			attempted,
			AgentIgnoreFiles(),
			log,
		)
		if err == nil {
			s.pw.set(attempted)
		}
		return err
	})

	writeJSON(w, http.StatusOK, map[string]string{"job_id": jobID})
}

// ---------------------------------------------------------------------------
// Session password handlers
// ---------------------------------------------------------------------------

// handleSessionPasswordStatus returns whether the session cache is
// currently populated. The password itself is never sent to the UI —
// only a boolean. The frontend uses this to decide whether to skip the
// password modal before kicking off a backup or sync.
func (s *server) handleSessionPasswordStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"has": s.pw.has()})
}

// handleSessionPasswordClear forgets the cached password. Called when
// the user clicks "Use a different password" after a sync/backup
// failure that was likely a wrong-password error.
func (s *server) handleSessionPasswordClear(w http.ResponseWriter, _ *http.Request) {
	s.pw.clear()
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Workspace binding
// ---------------------------------------------------------------------------

// handleForgetBinding removes the active workspace's .whatskept.json.
// The next sync will re-bind from whatever the latest backup says.
// This is the user-facing escape hatch behind the "Forget this
// workspace's identity" link in the mismatch modal.
func (s *server) handleForgetBinding(w http.ResponseWriter, _ *http.Request) {
	cur := s.ws.get()
	if cur == "" {
		httpError(w, http.StatusBadRequest, "no workspace open")
		return
	}
	if err := binding.Delete(cur); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("forget binding: %v", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
