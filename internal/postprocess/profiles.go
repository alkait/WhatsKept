package postprocess

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"whatskept/internal/backup"

	_ "github.com/mattn/go-sqlite3"
)

// ProfileSyncStats summarises one SyncProfiles run.
type ProfileSyncStats struct {
	FilesParseable int      `json:"files_parseable"`
	DistinctJIDs   int      `json:"distinct_jids"`
	Extracted      int      `json:"extracted"`
	Failed         int      `json:"failed"`
	Swept          int      `json:"swept"` // stale files removed from the avatar dir
	Errors         []string `json:"errors,omitempty"`
}

const profileSubdir = "profiles/whatsapp"

// JPEG / PNG magic bytes — sanity check before writing a blob out.
// WhatsApp `.thumb` files are still JPEG-encoded, just under a
// different extension; we always write `.jpg` regardless.
var (
	jpegMagic = []byte{0xFF, 0xD8, 0xFF}
	pngMagic  = []byte("\x89PNG\r\n\x1a\n")
)

// SyncProfiles extracts WhatsApp profile pictures from `b` into
// `<workspace>/profiles/whatsapp/<safe_jid>.jpg`, and upserts one
// `wa_profile_picture` row per JID in `dbPath`.
//
// Non-fatal for the surrounding sync: individual extraction failures
// (corrupt blobs, write errors) are recorded in Stats.Errors but the
// function returns nil unless something catastrophic stops *all* work
// (DB can't open, JID enumeration query errors).
//
// Expects `dbPath` to already have views.sql applied — relies on the
// `wa_jid_alias` view existing for phone↔LID canonicalization. If the
// view is missing, sync still works, just less de-duplicated.
func SyncProfiles(
	b *backup.Bundle,
	workspace, dbPath string,
	log func(string),
) (*ProfileSyncStats, error) {
	if log == nil {
		log = func(string) {}
	}
	outDir := filepath.Join(workspace, profileSubdir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", outDir, err)
	}

	// Pull the JID universe + phone→LID alias map from the live DB
	// first, so we can filter the manifest scan and canonicalize.
	knownJIDs, phoneToLID, err := loadJIDUniverse(dbPath)
	if err != nil {
		return nil, fmt.Errorf("read JID universe: %w", err)
	}

	stats := &ProfileSyncStats{}
	files := backup.ListProfileFiles(b, knownJIDs)

	// Canonicalize: collapse phone-keyed avatars onto their LID twin
	// so a contact whose iOS-Contacts photo lands under <phone> and
	// whose WhatsApp pic lands under <lid> end up in the same row.
	canonical := make(map[string]backup.ProfileFile, len(files))
	for _, f := range files {
		canon := f.JID
		if lid, ok := phoneToLID[f.JID]; ok {
			canon = lid
		}
		cf := f
		cf.JID = canon
		existing, have := canonical[canon]
		if !have || profileBetterByKind(cf, existing) {
			canonical[canon] = cf
		}
	}
	stats.FilesParseable = len(files)
	stats.DistinctJIDs = len(canonical)

	if len(canonical) == 0 {
		log("No matching profile pictures in the manifest.")
		return stats, nil
	}

	// One transaction over the whole batch. Cheap (~3,000 rows for a
	// heavy user) and gives a clean rollback if anything explodes
	// mid-flight. Each blob extraction happens *outside* the
	// transaction lock would be over-engineering — go-sqlite3
	// serializes anyway.
	rwDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db rw: %w", err)
	}
	defer rwDB.Close()
	tx, err := rwDB.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	upsert, err := tx.Prepare(`
		INSERT INTO wa_profile_picture(jid, whatsapp_path, whatsapp_kind,
		                               whatsapp_picture_id, synced_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			whatsapp_path       = excluded.whatsapp_path,
			whatsapp_kind       = excluded.whatsapp_kind,
			whatsapp_picture_id = excluded.whatsapp_picture_id,
			synced_at           = excluded.synced_at
	`)
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("prepare upsert: %w", err)
	}
	defer upsert.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	// Track filenames we wrote this run so the post-loop sweep can
	// reliably distinguish "current avatar" from "stale leftover from a
	// previous WhatsApp account / a deleted contact / an older Python
	// run that didn't filter to known JIDs".
	kept := make(map[string]struct{}, len(canonical))
	i := 0
	for _, f := range canonical {
		i++
		// Always write `.jpg` — macOS Preview can't open bare `.thumb`
		// files, and the bytes are valid JPEG regardless of what
		// WhatsApp called them in the manifest.
		filename := safeJIDFilename(f.JID) + ".jpg"
		outPath := filepath.Join(outDir, filename)

		blob, err := readBlob(b, f.Record)
		if err != nil {
			stats.Failed++
			stats.Errors = appendBoundedErr(stats.Errors, fmt.Sprintf("%s: %v", f.Record.Path, err))
			continue
		}
		if !looksLikeImage(blob) {
			stats.Failed++
			head := blob
			if len(head) > 4 {
				head = head[:4]
			}
			stats.Errors = appendBoundedErr(stats.Errors, fmt.Sprintf("%s: not a JPEG/PNG (got %x)", f.Record.Path, head))
			continue
		}
		if err := os.WriteFile(outPath, blob, 0o644); err != nil {
			stats.Failed++
			stats.Errors = appendBoundedErr(stats.Errors, fmt.Sprintf("%s: write failed: %v", f.Record.Path, err))
			continue
		}
		// Workspace-relative, forward-slashed — portable.
		relPath := profileSubdir + "/" + filename
		if _, err := upsert.Exec(f.JID, relPath, f.Kind, f.PictureID, now); err != nil {
			stats.Failed++
			stats.Errors = appendBoundedErr(stats.Errors, fmt.Sprintf("%s: upsert: %v", f.JID, err))
			continue
		}
		kept[filename] = struct{}{}
		stats.Extracted++
		if i%500 == 0 {
			log(fmt.Sprintf("  WhatsApp avatars: %d/%d…", i, len(canonical)))
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Sweep stale files. The profile dir is owned by this sync — it
	// represents the current truth of wa_profile_picture and nothing
	// else. Anything not in `kept` is either:
	//   - an avatar for a JID no longer referenced in ChatStorage
	//     (deleted chat / removed group member), or
	//   - a leftover from a previous workspace owner / Python-era
	//     run that filtered differently, or
	//   - junk (.DS_Store, an editor swap file, an OS thumbnail).
	// All three are unreachable through v_avatar and just clutter the
	// dir. Sweep is best-effort: individual unlink errors are logged
	// but don't abort the sync.
	if swept, err := sweepStaleProfiles(outDir, kept); err != nil {
		log(fmt.Sprintf("Avatar sweep failed: %v", err))
	} else if swept > 0 {
		log(fmt.Sprintf("Swept %d stale file(s) from profile dir.", swept))
		stats.Swept = swept
	}

	return stats, nil
}

// sweepStaleProfiles removes every entry in dir not in `kept` so the
// directory is a faithful mirror of the current wa_profile_picture
// rows. Subdirectories are left alone — we never create them, but if
// the user dropped one in deliberately, blowing it away unannounced
// would be too surprising. Returns the number of files removed.
func sweepStaleProfiles(dir string, kept map[string]struct{}) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var removed int
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if _, ok := kept[name]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err == nil {
			removed++
		}
	}
	return removed, nil
}

// profileBetterByKind: full-res beats thumb; same-kind tie broken by
// higher picture_id (more recent upload). Used during canonicalization
// when two manifest entries collapse to the same JID.
func profileBetterByKind(cand, existing backup.ProfileFile) bool {
	if cand.Kind == "jpg" && existing.Kind != "jpg" {
		return true
	}
	if cand.Kind != "jpg" && existing.Kind == "jpg" {
		return false
	}
	return atoiSafe(cand.PictureID) > atoiSafe(existing.PictureID)
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func readBlob(b *backup.Bundle, rec backup.Record) ([]byte, error) {
	rd, err := b.FileReader(rec)
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	return io.ReadAll(rd)
}

func looksLikeImage(blob []byte) bool {
	if len(blob) >= 3 && blob[0] == jpegMagic[0] && blob[1] == jpegMagic[1] && blob[2] == jpegMagic[2] {
		return true
	}
	if len(blob) >= len(pngMagic) && string(blob[:len(pngMagic)]) == string(pngMagic) {
		return true
	}
	return false
}

// safeJIDFilename keeps the JID human-readable on every OS by swapping
// '@' for '_at_'. JIDs only contain digits, '-', '@', '.' so no other
// substitutions are needed.
func safeJIDFilename(jid string) string {
	return strings.ReplaceAll(jid, "@", "_at_")
}

// loadJIDUniverse returns the set of JIDs that appear anywhere in the
// chat data plus the phone→LID alias map from the `wa_jid_alias` view.
// The alias map is empty (not an error) when the view doesn't exist —
// caller falls back to raw, non-canonical JIDs.
func loadJIDUniverse(dbPath string) (map[string]struct{}, map[string]string, error) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()

	jids := map[string]struct{}{}
	for _, q := range []string{
		"SELECT DISTINCT ZFROMJID    FROM ZWAMESSAGE",
		"SELECT DISTINCT ZTOJID      FROM ZWAMESSAGE",
		"SELECT DISTINCT ZMEMBERJID  FROM ZWAGROUPMEMBER",
		"SELECT DISTINCT ZCONTACTJID FROM ZWACHATSESSION",
	} {
		rows, err := db.Query(q)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", q, err)
		}
		for rows.Next() {
			var j sql.NullString
			if err := rows.Scan(&j); err != nil {
				rows.Close()
				return nil, nil, err
			}
			if j.Valid && j.String != "" {
				jids[j.String] = struct{}{}
			}
		}
		rows.Close()
	}

	phoneToLID := map[string]string{}
	if rows, err := db.Query(
		"SELECT phone_jid, lid_jid FROM wa_jid_alias " +
			"WHERE phone_jid IS NOT NULL AND lid_jid IS NOT NULL",
	); err == nil {
		for rows.Next() {
			var p, l sql.NullString
			if err := rows.Scan(&p, &l); err == nil && p.Valid && l.Valid {
				phoneToLID[p.String] = l.String
			}
		}
		rows.Close()
	}
	// Missing view -> empty map, caller copes. Not an error.

	return jids, phoneToLID, nil
}

// appendBoundedErr keeps Errors bounded so a manifest with thousands of
// duplicate failures (e.g. a corrupted backup) doesn't blow up memory
// or the SSE payload. First 25 entries kept.
func appendBoundedErr(slice []string, e string) []string {
	if len(slice) >= 25 {
		return slice
	}
	return append(slice, e)
}
