package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"whatskept/internal/app/web"
	"whatskept/internal/backup"
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
func (s *server) Start() {
	go func() {
		if err := s.httpServer.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Best-effort log; the window will surface the failure when
			// /api requests start failing.
			fmt.Fprintln(os.Stderr, "app server error:", err)
		}
	}()
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

	// Devices + backups
	mux.HandleFunc("GET /api/devices", s.handleListDevices)
	mux.HandleFunc("GET /api/backups", s.handleListBackups)
	mux.HandleFunc("DELETE /api/backups/{udid}", s.handleDeleteBackup)

	// Jobs (stubbed in Phase A, real in Phase D)
	mux.HandleFunc("POST /api/backup/run", s.handleRunBackup)
	mux.HandleFunc("GET /api/jobs/active", s.handleActiveJob)
	mux.HandleFunc("GET /api/stream/{job_id}", s.handleStreamJob)

	// Database routes — Database tab is hidden in this iteration but the
	// React code probes /api/database/status on mount; return a safe
	// "nothing to see here" rather than 404.
	mux.HandleFunc("GET /api/database/status", s.handleDatabaseStatus)
	mux.HandleFunc("POST /api/database/run", s.handleRunDatabaseTask)

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
	Path           string `json:"path"`
	BackupPassword string `json:"backup_password"`
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
	if req.BackupPassword != "" {
		if err := writeBackupPasswordEnv(abs, req.BackupPassword); err != nil {
			httpError(w, http.StatusInternalServerError, fmt.Sprintf("Cannot write .env: %v", err))
			return
		}
	}
	s.ws.set(abs)
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

// ---------------------------------------------------------------------------
// Device + backup handlers
// ---------------------------------------------------------------------------

type deviceItem struct {
	UDID string `json:"udid"`
	Name string `json:"name"`
}

func (s *server) handleListDevices(w http.ResponseWriter, _ *http.Request) {
	// Phase A: empty list. Phase C will exec the embedded `idevice_id`.
	writeJSON(w, http.StatusOK, []deviceItem{})
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
// Job handlers (Phase A: stubs)
// ---------------------------------------------------------------------------

func (s *server) handleRunBackup(w http.ResponseWriter, _ *http.Request) {
	httpError(w, http.StatusServiceUnavailable,
		"Run-backup is not yet wired in this build. Coming in the next iteration once `idevicebackup2` is embedded.")
}

func (s *server) handleActiveJob(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.jobs.activeJob())
}

func (s *server) handleStreamJob(w http.ResponseWriter, _ *http.Request) {
	httpError(w, http.StatusNotFound, "no such job")
}

// ---------------------------------------------------------------------------
// Database handlers (Database tab hidden; routes return safe shapes)
// ---------------------------------------------------------------------------

type databaseStatus struct {
	DBExists     bool  `json:"db_exists"`
	MessageCount *int  `json:"message_count,omitempty"`
	FTSCount     *int  `json:"fts_count,omitempty"`
}

func (s *server) handleDatabaseStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, databaseStatus{DBExists: false})
}

func (s *server) handleRunDatabaseTask(w http.ResponseWriter, _ *http.Request) {
	httpError(w, http.StatusServiceUnavailable, "Database tasks are not available in this build.")
}
