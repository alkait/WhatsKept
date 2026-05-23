// Package postprocess turns a freshly-decrypted ChatStorage.sqlite
// into an agent-ready workspace: it applies the SQL view layer (the
// 12 objects from views.sql, including the messages_fts virtual
// table), and drops the supporting files (AGENTS.md, views.sql, and
// per-agent .ignore files) next to it.
//
// This is the Go port of the Python `whatskept.postprocess` module.
// Only the cheap "messages" stage is implemented for now — sidecar
// indexers (image OCR, voice transcription, contacts sync, profile
// avatars) and the merge-forward step that preserves their outputs
// across re-extracts will land as separate stages later.
package postprocess

import (
	"database/sql"
	_ "embed" // for //go:embed directives below
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"whatskept/internal/backup"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed embed/views.sql
var viewsSQL string

//go:embed embed/AGENTS.md.tmpl
var agentsTmpl string

// ignoreHeader and ignoreEntries are written into each per-agent
// ignore file. The list is kept short on purpose: every entry MUST
// be something a multimodal model would otherwise burn tokens on,
// or a secret. See the comment block in the Python flow for the
// full reasoning per entry.
const ignoreHeader = "# Added by WhatsKept. DO NOT feed these to any model — see AGENTS.md."

var ignoreEntries = []string{
	"media/",
	"voice/",
	"profiles/",
	".env",
	"ChatStorage.sqlite.new", // transient temp file from a re-sync
	"ChatStorage.sqlite.preupdate-bak",
}

// SyncResult is the per-call summary we hand back to the API layer
// (and ultimately the React UI). All fields are JSON-tagged for
// direct re-use in the SSE "done" payload.
type SyncResult struct {
	BackupPath     string    `json:"backup_path"`
	BackupTakenAt  time.Time `json:"backup_taken_at"`
	DBPath         string    `json:"db_path"`
	BytesExtracted int64     `json:"bytes_extracted"`
	MessageCount   int       `json:"message_count"`
	FTSCount       int       `json:"fts_count"`
	AgentsMDExists bool      `json:"agents_md_exists"`
	IgnoreFiles    []string  `json:"ignore_files"`
}

// ApplyViews opens the SQLite database at dbPath and runs the whole
// embedded views.sql as a single script. The script is intentionally
// idempotent (uses CREATE … IF NOT EXISTS / CREATE VIEW after a
// matching DROP), so calling this against an already-processed DB
// is safe and refreshes any stale view definitions.
func ApplyViews(dbPath string) error {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec(viewsSQL); err != nil {
		return fmt.Errorf("apply views: %w", err)
	}
	return nil
}

// WriteAssets drops the files an agent needs to use the workspace
// productively into `workspace`:
//
//   - views.sql  — overwritten on every call (we own it).
//   - AGENTS.md  — written ONLY if missing. A user-edited AGENTS.md
//     is left alone, matching the Python behavior.
//   - one ignore file per name in agentIgnoreFiles — overwritten
//     wholesale (we own these, like the Python flow).
//
// The caller is expected to derive `agentIgnoreFiles` from the
// agent registry (see app.AgentIgnoreFiles); this package
// deliberately doesn't import `app` to keep the dependency arrow
// pointing one way.
func WriteAssets(workspace string, agentIgnoreFiles []string) error {
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return fmt.Errorf("mkdir workspace: %w", err)
	}

	// 1. views.sql — always rewritten.
	if err := os.WriteFile(filepath.Join(workspace, "views.sql"), []byte(viewsSQL), 0o644); err != nil {
		return fmt.Errorf("write views.sql: %w", err)
	}

	// 2. AGENTS.md — only if missing.
	agentsPath := filepath.Join(workspace, "AGENTS.md")
	if _, err := os.Stat(agentsPath); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(agentsPath, []byte(agentsTmpl), 0o644); err != nil {
			return fmt.Errorf("write AGENTS.md: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("stat AGENTS.md: %w", err)
	}

	// 3. Per-agent ignores.
	block := append([]string{ignoreHeader}, ignoreEntries...)
	blob := []byte(strings.Join(block, "\n") + "\n")
	for _, name := range agentIgnoreFiles {
		if name == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(workspace, name), blob, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

// SyncMessages is the high-level "Sync messages" pipeline:
//
//  1. Discover encrypted iOS backups under backupRoot, pick the
//     most recently taken one.
//  2. Decrypt its WhatsApp ChatStorage.sqlite into the workspace.
//  3. Apply views.sql (creates v_messages, v_chats, messages_fts, …).
//  4. Write AGENTS.md (if missing), views.sql, and the per-agent
//     ignore files.
//  5. Return a SyncResult with final row counts.
//
// `log` is invoked with one human-readable status line per major
// step. Pass nil for headless operation.
//
// Errors are returned with enough context to render directly in the
// UI ("no encrypted iOS backups found — …", "backup password
// required …", etc.).
func SyncMessages(
	backupRoot string,
	workspace string,
	password string,
	agentIgnoreFiles []string,
	log func(string),
) (*SyncResult, error) {
	if log == nil {
		log = func(string) {}
	}

	log("Discovering iOS backups…")
	infos, err := backup.Discover(backupRoot)
	if err != nil {
		return nil, fmt.Errorf("discover backups: %w", err)
	}
	var encrypted []backup.Info
	for _, b := range infos {
		if b.IsEncrypted {
			encrypted = append(encrypted, b)
		}
	}
	if len(encrypted) == 0 {
		return nil, errors.New("no encrypted iOS backups found — run a backup from the Backups tab first")
	}
	sort.SliceStable(encrypted, func(i, j int) bool {
		return encrypted[i].LastBackup.After(encrypted[j].LastBackup)
	})
	latest := encrypted[0]
	log(fmt.Sprintf("Latest backup: %s (taken %s)", latest.DisplayName(), latest.LastBackupString()))

	if password == "" {
		return nil, errors.New("backup password required (the latest backup is encrypted)")
	}

	dbPath := filepath.Join(workspace, "ChatStorage.sqlite")

	log("Decrypting ChatStorage.sqlite from backup…")
	n, err := backup.ExtractChatStorage(latest, password, dbPath)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	log(fmt.Sprintf("Wrote %s (%d bytes)", dbPath, n))

	log("Applying views and FTS5 index…")
	if err := ApplyViews(dbPath); err != nil {
		return nil, err
	}

	log("Writing AGENTS.md, views.sql, and agent ignore files…")
	if err := WriteAssets(workspace, agentIgnoreFiles); err != nil {
		return nil, err
	}

	// Final counts — best-effort. A failure here doesn't unwind the
	// sync (the DB is on disk and applied), so swallow the error
	// and report 0 instead.
	msgCount, ftsCount := readCounts(dbPath)
	log(fmt.Sprintf("Done. %d messages indexed (%d in FTS).", msgCount, ftsCount))

	_, agentsErr := os.Stat(filepath.Join(workspace, "AGENTS.md"))

	return &SyncResult{
		BackupPath:     latest.Path,
		BackupTakenAt:  latest.LastBackup,
		DBPath:         dbPath,
		BytesExtracted: n,
		MessageCount:   msgCount,
		FTSCount:       ftsCount,
		AgentsMDExists: agentsErr == nil,
		IgnoreFiles:    agentIgnoreFiles,
	}, nil
}

// readCounts opens dbPath read-only and returns (messages, fts_rows).
// Both default to 0 if the corresponding table is missing or the
// query errors — callers are expected to treat 0 as "unknown" rather
// than blowing up the whole sync.
func readCounts(dbPath string) (msgs, fts int) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return 0, 0
	}
	defer db.Close()
	_ = db.QueryRow(`SELECT COUNT(*) FROM ZWAMESSAGE`).Scan(&msgs)
	_ = db.QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&fts)
	return msgs, fts
}
