// Package postprocess turns a freshly-decrypted ChatStorage.sqlite
// into an agent-ready workspace: it applies the SQL view layer (the
// 12 objects from views.sql, including the messages_fts virtual
// table), and drops the supporting files (AGENTS.md, CLAUDE.md,
// views.sql, and per-agent .ignore files) next to it.
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
	"whatskept/internal/binding"

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
	BackupPath     string             `json:"backup_path"`
	BackupTakenAt  time.Time          `json:"backup_taken_at"`
	DBPath         string             `json:"db_path"`
	BytesExtracted int64              `json:"bytes_extracted"`
	MessageCount   int                `json:"message_count"`
	FTSCount       int                `json:"fts_count"`
	AgentsMDExists bool               `json:"agents_md_exists"`
	IgnoreFiles    []string           `json:"ignore_files"`
	ProfileSync    *ProfileSyncStats  `json:"profile_sync,omitempty"`
	ContactSync    *ContactSyncStats  `json:"contact_sync,omitempty"`
	SidecarMerge   *SidecarMergeStats `json:"sidecar_merge,omitempty"`
	OrphanPrune    *OrphanPruneStats  `json:"orphan_prune,omitempty"`
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
	// The static INSERT INTO messages_fts that used to live at the
	// bottom of views.sql moved into rebuildFTS() so it can JOIN in
	// sidecar tables (wa_image_text, wa_voice_text, wa_document)
	// when they exist. Run it here so ApplyViews's contract is
	// unchanged for callers: after this returns, messages_fts is
	// populated.
	if _, err := rebuildFTS(db); err != nil {
		return fmt.Errorf("rebuild fts: %w", err)
	}
	return nil
}

// WriteAssets drops the files an agent needs to use the workspace
// productively into `workspace`:
//
//   - views.sql              — overwritten on every call (we own it).
//   - AGENTS.md / CLAUDE.md  — written ONLY if missing. Both files
//     get the same template content; the dual filenames let agents
//     that look for one or the other (Claude Code reads CLAUDE.md,
//     most others follow the AGENTS.md convention) work out of the
//     box. A user-edited copy of either is left alone, matching the
//     Python behavior.
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

	// 2. AGENTS.md and CLAUDE.md — only if missing. Same content; the
	// twin files exist purely so Claude Code (which keys off CLAUDE.md)
	// and the broader AGENTS.md ecosystem both pick up our schema notes
	// without the user having to symlink anything.
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		p := filepath.Join(workspace, name)
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			if err := os.WriteFile(p, []byte(agentsTmpl), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", name, err)
			}
		} else if err != nil {
			return fmt.Errorf("stat %s: %w", name, err)
		}
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
//  1. Discover encrypted iOS backups. If the workspace already has a
//     binding, filter the pool to that device's UDID; otherwise the
//     latest backup of any device is fair game (first-sync case).
//  2. Decrypt into a staging file (ChatStorage.sqlite.new) — the
//     live DB is never touched until identity is verified.
//  3. Probe the staging DB for the WhatsApp owner JID + a chat-
//     session fingerprint.
//  4. First sync: persist a fresh binding from {UDID, device name,
//     JID, fingerprint}. Subsequent sync: compare against the bound
//     identity — JID mismatch returns *ErrIdentityMismatch and
//     leaves the live DB untouched.
//  5. Atomic rename: staging → live ChatStorage.sqlite.
//  6. Apply views.sql, write AGENTS.md + CLAUDE.md (if missing),
//     views.sql, and the per-agent ignore files.
//  7. Return a SyncResult with final row counts.
//
// `log` is invoked with one human-readable status line per major
// step. Pass nil for headless operation.
//
// Errors are returned with enough context to render directly in the
// UI ("no encrypted iOS backups found — …", "backup password
// required …", etc.). The mismatch case is a typed *ErrIdentityMismatch
// so the API layer can render it differently from a generic failure.
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

	existing, err := binding.Load(workspace)
	if err != nil {
		return nil, fmt.Errorf("read workspace binding: %w", err)
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

	// Pick the latest backup. When the workspace is already bound,
	// constrain the pool to its UDID so a stray backup from another
	// phone (e.g. the user plugging in a friend's device) can't ever
	// be selected, even silently.
	var latest backup.Info
	if existing != nil {
		var matches []backup.Info
		for _, b := range encrypted {
			if filepath.Base(b.Path) == existing.UDID {
				matches = append(matches, b)
			}
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf(
				"this workspace is bound to %s (UDID %s) but no backup of that device is on this Mac. "+
					"Plug it in and run a backup from the Backups tab.",
				existing.DeviceName, existing.UDID,
			)
		}
		latest = matches[0]
	} else {
		latest = encrypted[0]
	}
	log(fmt.Sprintf("Latest backup: %s (taken %s)", latest.DisplayName(), latest.LastBackupString()))

	if password == "" {
		return nil, errors.New("backup password required (the latest backup is encrypted)")
	}

	livePath := filepath.Join(workspace, "ChatStorage.sqlite")
	tempPath := livePath + ".new"
	// Defensive: a previous crashed sync may have left a stale
	// staging file. Don't trust its contents.
	_ = os.Remove(tempPath)

	log("Unlocking iOS backup…")
	bundle, err := backup.Open(latest, password)
	if err != nil {
		return nil, fmt.Errorf("unlock backup: %w", err)
	}

	log("Decrypting ChatStorage.sqlite from backup…")
	n, err := backup.ExtractChatStorageFrom(bundle, tempPath)
	if err != nil {
		_ = os.Remove(tempPath)
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	log(fmt.Sprintf("Decrypted %d bytes to staging file", n))

	log("Verifying WhatsApp account identity…")
	fresh, err := ReadIdentity(tempPath, latest)
	if err != nil {
		_ = os.Remove(tempPath)
		return nil, err
	}

	if existing == nil {
		// First sync. Whatever identity we just read becomes the
		// binding — there's nothing prior to compare against.
		fresh.BoundAt = time.Now().UTC()
		fresh.LastSyncedAt = fresh.BoundAt
		if err := binding.Save(workspace, fresh); err != nil {
			_ = os.Remove(tempPath)
			return nil, fmt.Errorf("save workspace binding: %w", err)
		}
		log(fmt.Sprintf("Bound workspace to %s%s", fresh.DeviceName, phoneSuffix(fresh)))
	} else {
		// Subsequent sync. JID mismatch is a hard stop — the live DB
		// must not be overwritten with a different account's data.
		if err := CompareIdentity(existing, fresh); err != nil {
			_ = os.Remove(tempPath)
			return nil, err
		}
		// Refresh dynamic fields (device name and OS version may have
		// changed; OwnerJID becomes available if a previously
		// signal-less account now has groups; fingerprint always
		// drifts as the chat list evolves). UDID stays bound to what
		// the workspace was originally tied to — the filter above
		// guarantees we never confuse it with another device.
		updated := *existing
		updated.DeviceName = fresh.DeviceName
		updated.ProductType = fresh.ProductType
		if fresh.OwnerJID != "" {
			updated.OwnerJID = fresh.OwnerJID
		}
		if fresh.Fingerprint != "" {
			updated.Fingerprint = fresh.Fingerprint
		}
		updated.LastSyncedAt = time.Now().UTC()
		if err := binding.Save(workspace, &updated); err != nil {
			_ = os.Remove(tempPath)
			return nil, fmt.Errorf("save workspace binding: %w", err)
		}
		log(fmt.Sprintf("Identity verified: %s%s", existing.DeviceName, phoneSuffix(existing)))
	}

	// Sidecar merge-forward. The freshly-decrypted tempPath has none
	// of the user's hard-earned wa_image_text / media_index /
	// wa_voice_text / voice_index rows from previous syncs — the
	// backup carries device data only, not WhatsKept-derived data.
	// Copy those rows forward (filtering to messages that still
	// exist) BEFORE the rename, so the about-to-be-promoted DB has
	// them. If livePath doesn't exist yet (first sync) this is a
	// no-op. Failures are non-fatal: a corrupted prior DB shouldn't
	// block a fresh sync from succeeding — we just lose OCR
	// continuity and the user can re-run media-index.
	var mergeStats *SidecarMergeStats
	if _, err := os.Stat(livePath); err == nil {
		log("Carrying OCR/transcript state forward…")
		ms, mErr := mergeSidecarsForward(livePath, tempPath)
		if mErr != nil {
			log(fmt.Sprintf("Sidecar merge failed (continuing): %v", mErr))
		} else {
			mergeStats = ms
			if ms.OCRPreserved > 0 || ms.VoicePreserved > 0 {
				log(fmt.Sprintf(
					"Preserved %d OCR rows, %d voice rows (dropped: %d OCR, %d voice for deleted messages).",
					ms.OCRPreserved, ms.VoicePreserved, ms.OCRDropped, ms.VoiceDropped,
				))
			}
		}
	}

	// Identity OK — atomically promote staging to live. After this
	// point a partial failure leaves us with a fresh DB that just
	// hasn't had views re-applied; ApplyViews is idempotent, so the
	// user re-clicking Sync resolves it.
	if err := os.Rename(tempPath, livePath); err != nil {
		_ = os.Remove(tempPath)
		return nil, fmt.Errorf("promote staging db: %w", err)
	}

	log("Applying views and FTS5 index…")
	if err := ApplyViews(livePath); err != nil {
		return nil, err
	}

	// Orphan prune. mergeSidecarsForward only carries forward rows
	// whose rowid is in the new ZWAMESSAGE, so DB-side this is
	// usually a no-op — but it also walks ./media/ and deletes any
	// <rowid>.jpg whose message no longer exists on the device. That
	// gives us the "media folder mirrors device state" invariant the
	// user asked for. Non-fatal: failures bump stats and the sync
	// still succeeds.
	mediaDir := filepath.Join(workspace, "media")
	voiceDir := filepath.Join(workspace, "voice")
	pruneStats, pruneErr := pruneOrphans(livePath, mediaDir, voiceDir)
	if pruneErr != nil {
		log(fmt.Sprintf("Orphan prune failed (continuing): %v", pruneErr))
	} else if pruneStats != nil {
		total := pruneStats.MediaFilesDeleted + pruneStats.OCRRowsDeleted +
			pruneStats.MediaIndexRowsDeleted + pruneStats.VoiceRowsDeleted +
			pruneStats.VoiceIndexRowsDeleted + pruneStats.VoiceFilesDeleted
		if total > 0 {
			log(fmt.Sprintf(
				"Pruned: %d media files / %d voice files · %d wa_image_text / %d wa_voice_text rows · %d media_index / %d voice_index rows.",
				pruneStats.MediaFilesDeleted, pruneStats.VoiceFilesDeleted,
				pruneStats.OCRRowsDeleted, pruneStats.VoiceRowsDeleted,
				pruneStats.MediaIndexRowsDeleted, pruneStats.VoiceIndexRowsDeleted,
			))
		}
	}

	// iOS-Contacts sync. Runs before SyncProfiles because it's the
	// faster of the two (~2–5 s vs 10–30 s) and gives the user a
	// visible win sooner — names start resolving in the UI before
	// the avatar dump finishes. Non-fatal: a failure here is
	// captured in stats.Errors and the surrounding sync still
	// succeeds. Fail-soft if the backup has no AddressBook record
	// (rare — empty device, privacy reset, non-iOS source).
	log("Syncing iOS Contacts…")
	contactStats, contactErr := SyncContacts(bundle, workspace, livePath, log)
	if contactErr != nil {
		log(fmt.Sprintf("Contacts sync failed: %v", contactErr))
		contactStats = &ContactSyncStats{
			Errors: []string{contactErr.Error()},
		}
	}

	// Profile-avatar sync. Non-fatal: a failure here doesn't unwind
	// the message sync — the DB is already on disk and applied. The
	// stats (and any per-file errors) are surfaced in the SyncResult
	// so the UI can render "X avatars synced, Y failed" without
	// blowing up the success path.
	log("Syncing WhatsApp profile pictures…")
	profileStats, profileErr := SyncProfiles(bundle, workspace, livePath, log)
	if profileErr != nil {
		log(fmt.Sprintf("Profile sync failed: %v", profileErr))
		profileStats = &ProfileSyncStats{
			Errors: []string{profileErr.Error()},
		}
	} else if profileStats != nil {
		if profileStats.Failed > 0 {
			log(fmt.Sprintf("Wrote %d avatars (%d failed).", profileStats.Extracted, profileStats.Failed))
		} else {
			log(fmt.Sprintf("Wrote %d avatars.", profileStats.Extracted))
		}
	}

	log("Writing AGENTS.md, CLAUDE.md, views.sql, and agent ignore files…")
	if err := WriteAssets(workspace, agentIgnoreFiles); err != nil {
		return nil, err
	}

	// Final counts — best-effort. A failure here doesn't unwind the
	// sync (the DB is on disk and applied), so swallow the error
	// and report 0 instead.
	msgCount, ftsCount := readCounts(livePath)
	log(fmt.Sprintf("Done. %d messages indexed (%d in FTS).", msgCount, ftsCount))

	_, agentsErr := os.Stat(filepath.Join(workspace, "AGENTS.md"))

	return &SyncResult{
		BackupPath:     latest.Path,
		BackupTakenAt:  latest.LastBackup,
		DBPath:         livePath,
		BytesExtracted: n,
		MessageCount:   msgCount,
		FTSCount:       ftsCount,
		AgentsMDExists: agentsErr == nil,
		IgnoreFiles:    agentIgnoreFiles,
		ProfileSync:    profileStats,
		ContactSync:    contactStats,
		SidecarMerge:   mergeStats,
		OrphanPrune:    pruneStats,
	}, nil
}

// phoneSuffix is a tiny formatting helper so the log lines read
// naturally: "Bound workspace to Aiman's iPhone (+971504320432)" if
// we have a JID, or just "Bound workspace to Aiman's iPhone" if not.
func phoneSuffix(b *binding.Binding) string {
	if p := b.Phone(); p != "" {
		return " (" + p + ")"
	}
	return ""
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
