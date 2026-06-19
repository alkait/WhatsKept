package app

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// workspaceSizes is the JSON shape returned by GET /api/workspace/sizes.
// Each section reports its on-disk footprint independently so the
// Agents tab can render a per-component breakdown — useful for users
// deciding which subdirs to .ignore from a model's token budget, and
// for sanity-checking that a sync actually produced the expected
// volume of artefacts.
//
// Sections that don't exist on disk yet (never-synced workspace, or
// the user deleted them) report Exists=false with Bytes=0; the
// frontend renders an explanatory placeholder rather than a blank
// row, so the user always sees the full set of categories.
type workspaceSizes struct {
	Database  workspaceSizeEntry `json:"database"`
	Profiles  workspaceSizeEntry `json:"profiles"`
	Media     workspaceSizeEntry `json:"media"`
	Voice     workspaceSizeEntry `json:"voice"`
	Documents workspaceSizeEntry `json:"documents"`

	// Total is the sum of Bytes across every section that Exists.
	// Computed server-side so the frontend doesn't have to recompute
	// the same number in three places (Agents tab, header, startup
	// tile) and risk drifting if the row set changes.
	Total int64 `json:"total"`
}

// workspaceSizeEntry describes one section of the workspace.
//
//   - Exists: the underlying path is present on disk (regular file
//     for the database, directory for everything else). False means
//     the section hasn't been generated yet.
//   - Bytes:  total on-disk size in bytes. Zero is a legitimate value
//     (empty directory) and the frontend distinguishes "exists with
//     zero bytes" from "doesn't exist" via Exists.
type workspaceSizeEntry struct {
	Exists bool  `json:"exists"`
	Bytes  int64 `json:"bytes"`
}

// handleWorkspaceSizes returns the size breakdown of the active
// workspace. Reads the workspace path from server state — same
// invariant as handleCurrentWorkspace — so the frontend doesn't
// have to re-send it on every poll.
func (s *server) handleWorkspaceSizes(w http.ResponseWriter, _ *http.Request) {
	cur := s.ws.get()
	if cur == "" {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, computeWorkspaceSizes(cur))
}

// computeWorkspaceSizes walks each known workspace section and
// returns its size. The walks are sequential and best-effort:
// individual file/dir read errors are silently skipped so a
// transient ENOENT (mid-sync rotation) or a permission glitch on
// one entry doesn't sink the whole response.
//
// Path conventions (must match the writers in internal/postprocess
// and internal/backup):
//
//   - database:   <workspace>/ChatStorage.sqlite (regular file)
//   - profiles:   <workspace>/profiles/         (recursive; covers
//     both profiles/whatsapp/ and profiles/ios/)
//   - media:      <workspace>/media/            (recursive)
//   - voice:      <workspace>/voice/            (recursive)
//   - documents:  <workspace>/documents/        (recursive)
func computeWorkspaceSizes(workspace string) workspaceSizes {
	out := workspaceSizes{
		Database:  fileSize(filepath.Join(workspace, "ChatStorage.sqlite")),
		Profiles:  dirSize(filepath.Join(workspace, "profiles")),
		Media:     dirSize(filepath.Join(workspace, "media")),
		Voice:     dirSize(filepath.Join(workspace, "voice")),
		Documents: dirSize(filepath.Join(workspace, "documents")),
	}
	for _, e := range []workspaceSizeEntry{out.Database, out.Profiles, out.Media, out.Voice, out.Documents} {
		if e.Exists {
			out.Total += e.Bytes
		}
	}
	return out
}

// computeWorkspaceTotal returns just the total-bytes value, skipping
// the per-section breakdown. Used by /api/workspace/recent so the
// startup picker tile can render a size hint per recent workspace
// without us having to JSON-serialise five subtotals we'd ignore.
func computeWorkspaceTotal(workspace string) int64 {
	return computeWorkspaceSizes(workspace).Total
}

// revealSectionMap describes how to map a section key (the same key
// rendered as a row in the Agents-tab breakdown) to a concrete
// workspace-relative path and a hint about whether it's a file or
// a directory. The hint controls Finder behaviour:
//
//   - file:      use `open -R` so Finder opens the parent and
//     pre-selects the file. Opening a .sqlite via `open` would
//     try to launch it in the default app, which isn't what the
//     user means by "show me my database".
//   - directory: use `open` so Finder enters the directory and the
//     user sees its contents directly. `open -R` on a directory
//     would reveal the directory in its parent, one click away
//     from the contents we actually want to show.
//
// The "root" pseudo-key is a special case that resolves to the
// workspace directory itself; used by the "Total on disk" click.
var revealSectionMap = map[string]struct {
	sub    string
	isFile bool
}{
	"database":  {"ChatStorage.sqlite", true},
	"profiles":  {"profiles", false},
	"media":     {"media", false},
	"voice":     {"voice", false},
	"documents": {"documents", false},
	"root":      {"", false},
}

type revealRequest struct {
	Key string `json:"key"`
}

// handleWorkspaceReveal opens a section of the active workspace in
// macOS Finder. The request body identifies the section by key (one
// of the entries in revealSectionMap); we never accept a raw path
// from the frontend, which sidesteps the entire "can the user reveal
// /etc/passwd" class of bugs.
//
// Missing sections (the user hasn't synced media yet, etc.) report
// 404 with a human-readable message so the UI can disable the click
// or show a tooltip instead of silently no-op'ing.
func (s *server) handleWorkspaceReveal(w http.ResponseWriter, r *http.Request) {
	cur := s.ws.get()
	if cur == "" {
		httpError(w, http.StatusBadRequest, "no workspace is currently open")
		return
	}

	var req revealRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	key := strings.ToLower(strings.TrimSpace(req.Key))
	spec, ok := revealSectionMap[key]
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("unknown section %q", req.Key))
		return
	}

	target := cur
	if spec.sub != "" {
		target = filepath.Join(cur, spec.sub)
	}
	if _, err := os.Stat(target); err != nil {
		httpError(w, http.StatusNotFound, fmt.Sprintf("%s does not exist yet", spec.sub))
		return
	}

	// Reveal in the OS file manager (Finder / Explorer). isFile selects
	// "highlight within parent" vs "open the directory". See fileManagerCmd.
	cmd := fileManagerCmd(target, spec.isFile)
	if err := cmd.Start(); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("failed to open file manager: %v", err))
		return
	}
	go cmd.Wait() // reap child; we don't care about exit code

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"revealed": target,
	})
}

// fileSize reports the size of a single regular file. Missing or
// non-regular paths report Exists=false.
func fileSize(path string) workspaceSizeEntry {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return workspaceSizeEntry{}
	}
	return workspaceSizeEntry{Exists: true, Bytes: st.Size()}
}

// dirSize reports the recursive on-disk size of a directory tree.
// Symlinks are not followed (filepath.WalkDir uses lstat semantics);
// this matches what `du` would report and avoids double-counting if
// a section ever gains a symlink shortcut.
func dirSize(dir string) workspaceSizeEntry {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return workspaceSizeEntry{}
	}
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		fi, ferr := d.Info()
		if ferr != nil {
			return nil
		}
		// Only count regular files. Symlinks and other special
		// entries don't contribute meaningful bytes here.
		if fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return nil
	})
	return workspaceSizeEntry{Exists: true, Bytes: total}
}
