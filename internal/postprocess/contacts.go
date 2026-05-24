package postprocess

// Contact-name resolution. Pulls the user's iOS Address Book
// (`Library/AddressBook/AddressBook.sqlitedb` in the `HomeDomain` of
// the encrypted iOS backup), matches each phone number to a WhatsApp
// JID seen in `ZWAMESSAGE` / `ZWAGROUPMEMBER`, and writes the matches
// into a `wa_contact(jid, display_name, source, synced_at)` table
// inside the same `ChatStorage.sqlite`.
//
// After sync, `v_messages.sender_name` (defined in `views.sql`)
// prefers the iOS-Contacts display name over WhatsApp's own
// `ZCONTACTNAME`, the sender's self-chosen `ZPUSHNAME`, or the raw
// JID. The view-level `wa_jid_alias` join handles LID lookups so a
// saved name surfaces even when a group message arrives keyed by
// `<digits>@lid` instead of `<digits>@s.whatsapp.net`.
//
// Privacy: the address book contains the user's *entire* phone
// contact list, including people they've never messaged. We only
// persist contacts whose normalized phone matches a JID actually
// present in `ZWAMESSAGE`. The decrypted `AddressBook.sqlitedb` is
// deleted from disk as soon as the sync finishes.
//
// Source-of-truth: the Python `whatskept.contacts` module.

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

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	// HomeDomain entries in the manifest that hold the user's
	// address book. Stable across iOS 13–26.
	addressBookDomain     = "HomeDomain"
	addressBookPath       = "Library/AddressBook/AddressBook.sqlitedb"
	addressBookImagesPath = "Library/AddressBook/AddressBookImages.sqlitedb"

	// ABMultiValue.property code for "phone number". Stable across
	// every iOS version we care about.
	abPropertyPhone = 3

	// E.164 sanity bounds (excluding the leading '+'). Country codes
	// are 1–3 digits, subscriber numbers usually 7–12.
	phoneMinDigits = 7
	phoneMaxDigits = 15

	// Provenance string written to wa_contact.source.
	contactsSource = "ios-contacts"

	// iOS-Contacts thumbnails are written under
	// <workspace>/profiles/ios/<safe_jid>.jpg.
	iosAvatarSubdir = "profiles/ios"

	// Re-detection threshold: require at least N JIDs sharing the
	// same leading 1–3 digit prefix before we trust it as the user's
	// "default" country code. Keeps a 3-message inbox from getting a
	// confidently wrong default applied.
	defaultCCMinHits = 20
)

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// ContactSyncStats summarises one SyncContacts run. JSON-tagged for
// direct re-use in the SSE "done" payload and the SyncResult that
// the React UI consumes.
type ContactSyncStats struct {
	ContactsTotal      int      `json:"contacts_total"`
	PhonesTotal        int      `json:"phones_total"`
	PhonesNormalized   int      `json:"phones_normalized"`
	PhonesMatched      int      `json:"phones_matched"`
	JIDsResolved       int      `json:"jids_resolved"`
	DefaultCountryCode string   `json:"default_country_code,omitempty"`
	AvatarsMatched     int      `json:"avatars_matched"`
	AvatarsWritten     int      `json:"avatars_written"`
	AvatarsFailed      int      `json:"avatars_failed"`
	AvatarsNoThumb     int      `json:"avatars_no_thumb"`
	AddressBookMissing bool     `json:"address_book_missing,omitempty"`
	Errors             []string `json:"errors,omitempty"`
}

// SyncContacts is the top-level orchestrator. Mirrors the Python
// `contacts.sync` entry point.
//
// Steps:
//
//  1. Locate AddressBook.sqlitedb (and AddressBookImages.sqlitedb,
//     if present) in the manifest under HomeDomain.
//  2. Stream-decrypt both to a temp dir.
//  3. Read ABPerson + ABMultiValue (phones, property=3).
//  4. Build the JID set from ChatStorage and detect the user's
//     default country code from JID plurality.
//  5. Match contact phones to JIDs (E.164-as-is, default-CC fixup,
//     raw fallback).
//  6. Snapshot-rebuild wa_contact in one transaction.
//  7. For matched JIDs, read ABThumbnailImage and snapshot-rebuild
//     wa_ios_avatar plus the workspace's profiles/ios/ directory.
//  8. Wipe the decrypted address-book copies — they contain the
//     user's full contact list and we only retain the WhatsApp
//     intersection.
//
// Fail-soft: if the backup has no AddressBook record (rare — empty
// device, privacy-reset, or a non-iOS source), we log a warning,
// mark the result `AddressBookMissing`, and return nil so the
// surrounding sync succeeds. Per-step errors (corrupt rows, write
// failures) are accumulated into Stats.Errors but don't abort.
//
// Expects `dbPath` to already have views.sql applied; reads
// ZWAMESSAGE etc. directly so the wa_jid_alias view isn't required
// here, but the views.sql-declared wa_contact / wa_ios_avatar
// schemas are. CREATE IF NOT EXISTS shadow definitions live in
// ensureContactSchema() so this module also works on a workspace
// where views.sql hasn't been refreshed since an upgrade.
func SyncContacts(
	b *backup.Bundle,
	workspace, dbPath string,
	log func(string),
) (*ContactSyncStats, error) {
	if log == nil {
		log = func(string) {}
	}

	// 1. Manifest scan.
	abRec, abiRec := locateAddressBookRecords(b)
	if abRec == nil {
		log("AddressBook.sqlitedb not found in this backup — skipping contacts sync.")
		return &ContactSyncStats{AddressBookMissing: true}, nil
	}

	// 2. Decrypt to a temp dir we own and wipe at the end.
	workDir, err := os.MkdirTemp("", "whatskept-contacts-")
	if err != nil {
		return nil, fmt.Errorf("contacts: mkdtemp: %w", err)
	}
	defer func() {
		// Privacy: blow away the decrypted address book regardless
		// of how we exit. The user's full contact list (including
		// people they've never messaged) lives in those files.
		_ = os.RemoveAll(workDir)
	}()
	abPath := filepath.Join(workDir, "AddressBook.sqlitedb")
	if _, err := decryptRecord(b, *abRec, abPath); err != nil {
		return nil, fmt.Errorf("contacts: decrypt AddressBook: %w", err)
	}

	var abiPath string
	if abiRec != nil {
		abiPath = filepath.Join(workDir, "AddressBookImages.sqlitedb")
		if _, err := decryptRecord(b, *abiRec, abiPath); err != nil {
			// Images are optional — log and continue without them.
			log(fmt.Sprintf("AddressBookImages decrypt failed (skipping iOS avatars): %v", err))
			abiPath = ""
		}
	}

	// 3. Read contacts.
	contacts, err := readContacts(abPath)
	if err != nil {
		return nil, fmt.Errorf("contacts: read AddressBook: %w", err)
	}

	stats := &ContactSyncStats{ContactsTotal: len(contacts)}
	if len(contacts) == 0 {
		log("AddressBook has no nameable contacts — nothing to sync.")
		return stats, nil
	}

	// 4. Open the live ChatStorage RW (we'll write wa_contact +
	// wa_ios_avatar). Contact-side reads use a ro&immutable=1 conn
	// against the temp file, so this RW conn doesn't fight with
	// SQLite's locking.
	rwDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("contacts: open chat db rw: %w", err)
	}
	defer rwDB.Close()

	jidSet, err := buildJIDIndex(rwDB)
	if err != nil {
		return nil, fmt.Errorf("contacts: build JID index: %w", err)
	}
	defaultCC := detectDefaultCountryCode(rwDB)
	stats.DefaultCountryCode = defaultCC

	// 5. Match.
	mapping, personIDByJID, mstats := matchContacts(contacts, jidSet, defaultCC)
	stats.PhonesTotal = mstats.phonesTotal
	stats.PhonesNormalized = mstats.phonesNormalized
	stats.PhonesMatched = mstats.phonesMatched
	stats.JIDsResolved = len(mapping)

	// 6. Snapshot-rebuild wa_contact.
	now := time.Now().UTC().Format(time.RFC3339)
	if err := ensureContactSchema(rwDB); err != nil {
		return nil, fmt.Errorf("contacts: ensure schema: %w", err)
	}
	if err := writeContactMapping(rwDB, mapping, now); err != nil {
		return nil, fmt.Errorf("contacts: write wa_contact: %w", err)
	}
	if defaultCC != "" {
		log(fmt.Sprintf("Matched %d JIDs to %d iOS contacts (default CC: +%s).",
			stats.JIDsResolved, stats.ContactsTotal, defaultCC))
	} else {
		log(fmt.Sprintf("Matched %d JIDs to %d iOS contacts.",
			stats.JIDsResolved, stats.ContactsTotal))
	}

	// 7. iOS-Contacts avatars (if AddressBookImages decrypted OK).
	if abiPath == "" {
		// Still wipe wa_ios_avatar so a previous-run snapshot
		// doesn't leak into the freshly synced workspace.
		if _, err := rwDB.Exec("DELETE FROM wa_ios_avatar"); err != nil {
			stats.Errors = appendBoundedErr(stats.Errors, fmt.Sprintf("wipe wa_ios_avatar: %v", err))
		}
		return stats, nil
	}
	if err := writeIOSAvatars(rwDB, workspace, abiPath, personIDByJID, now, stats); err != nil {
		// writeIOSAvatars is itself fail-soft — only the catastrophic
		// "couldn't open the images DB at all" case bubbles here.
		stats.Errors = appendBoundedErr(stats.Errors, err.Error())
	}
	if stats.AvatarsWritten > 0 || stats.AvatarsFailed > 0 {
		log(fmt.Sprintf("Wrote %d iOS-Contacts avatars (%d no-thumb, %d failed).",
			stats.AvatarsWritten, stats.AvatarsNoThumb, stats.AvatarsFailed))
	}

	return stats, nil
}

// ---------------------------------------------------------------------------
// Manifest + decryption
// ---------------------------------------------------------------------------

// locateAddressBookRecords returns the manifest entries for
// AddressBook.sqlitedb and (optionally) AddressBookImages.sqlitedb in
// HomeDomain. Either may be nil — fail-soft is the caller's problem.
func locateAddressBookRecords(b *backup.Bundle) (ab, abi *backup.Record) {
	recs := b.Records()
	for i := range recs {
		r := &recs[i]
		if r.Domain != addressBookDomain {
			continue
		}
		switch r.Path {
		case addressBookPath:
			ab = r
		case addressBookImagesPath:
			abi = r
		}
		if ab != nil && abi != nil {
			return ab, abi
		}
	}
	return ab, abi
}

// decryptRecord streams a single manifest record through the
// dunhamsteve decryption pipeline into outPath. Trusted to handle
// the iOS-system-DB filesize quirk (manifest.filesize > actual
// decrypted size) — dunhamsteve doesn't enforce a length assertion,
// unlike its Python counterpart.
func decryptRecord(b *backup.Bundle, rec backup.Record, outPath string) (int64, error) {
	rd, err := b.FileReader(rec)
	if err != nil {
		return 0, fmt.Errorf("file reader: %w", err)
	}
	defer rd.Close()
	w, err := os.Create(outPath)
	if err != nil {
		return 0, fmt.Errorf("create: %w", err)
	}
	n, copyErr := io.Copy(w, rd)
	closeErr := w.Close()
	if copyErr != nil {
		return n, fmt.Errorf("copy: %w", copyErr)
	}
	if closeErr != nil {
		return n, fmt.Errorf("close: %w", closeErr)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// AddressBook reads
// ---------------------------------------------------------------------------

// contact mirrors the Python `Contact` dataclass. Internal-only;
// nothing outside this file should touch it.
type contact struct {
	personID    int
	displayName string
	phones      []string // raw phone strings as stored by iOS
}

// composeDisplayName picks the user's preferred label for a contact,
// matching how iOS Contacts itself prioritises:
//
//  1. Nickname (a deliberate override, e.g. "Mom").
//  2. First (Middle?) Last — the standard form.
//  3. Organization (for business contacts with no person name).
//
// Returns "" when none of those are set; the caller drops contacts
// with no composable name.
func composeDisplayName(first, middle, last, org, nick string) string {
	if s := strings.TrimSpace(nick); s != "" {
		return s
	}
	parts := []string{}
	for _, p := range []string{first, middle, last} {
		if s := strings.TrimSpace(p); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	return strings.TrimSpace(org)
}

// readContacts opens the decrypted AddressBook DB read-only and
// returns every ABPerson with a composable name AND at least one
// non-empty phone entry. Matches Python's read_contacts.
func readContacts(abPath string) ([]contact, error) {
	// `immutable=1` skips journal-creation; required because
	// `mode=ro` would otherwise still try to create a sidecar.
	uri := "file:" + abPath + "?mode=ro&immutable=1"
	db, err := sql.Open("sqlite3", uri)
	if err != nil {
		return nil, fmt.Errorf("open ab db: %w", err)
	}
	defer db.Close()

	// 1) Person rows → name index keyed by ROWID.
	rows, err := db.Query(`
		SELECT ROWID, First, Middle, Last, Organization, Nickname
		FROM   ABPerson
	`)
	if err != nil {
		return nil, fmt.Errorf("query ABPerson: %w", err)
	}
	names := make(map[int]string, 1024)
	for rows.Next() {
		var rowid int
		var first, middle, last, org, nick sql.NullString
		if err := rows.Scan(&rowid, &first, &middle, &last, &org, &nick); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan ABPerson: %w", err)
		}
		if name := composeDisplayName(first.String, middle.String, last.String, org.String, nick.String); name != "" {
			names[rowid] = name
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows ABPerson: %w", err)
	}

	// 2) Phone rows → list of phones per person.
	phoneRows, err := db.Query(
		`SELECT record_id, value FROM ABMultiValue
		 WHERE  property = ? AND value IS NOT NULL`,
		abPropertyPhone,
	)
	if err != nil {
		return nil, fmt.Errorf("query ABMultiValue: %w", err)
	}
	phonesByPerson := make(map[int][]string, len(names))
	for phoneRows.Next() {
		var recordID int
		var value sql.NullString
		if err := phoneRows.Scan(&recordID, &value); err != nil {
			phoneRows.Close()
			return nil, fmt.Errorf("scan ABMultiValue: %w", err)
		}
		v := strings.TrimSpace(value.String)
		if v == "" {
			continue
		}
		phonesByPerson[recordID] = append(phonesByPerson[recordID], v)
	}
	phoneRows.Close()
	if err := phoneRows.Err(); err != nil {
		return nil, fmt.Errorf("rows ABMultiValue: %w", err)
	}

	// 3) Join: keep only ABPerson rows that have BOTH a name and at
	// least one phone. Iterating over phonesByPerson preserves
	// Python's "phone-keyed" iteration order in spirit; on a JID
	// collision the first-seen contact wins (see matchContacts).
	out := make([]contact, 0, len(phonesByPerson))
	for pid, phones := range phonesByPerson {
		name, ok := names[pid]
		if !ok {
			continue
		}
		out = append(out, contact{personID: pid, displayName: name, phones: phones})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Phone normalization + country-code heuristics
// ---------------------------------------------------------------------------

// normalizePhone strips a user-typed phone string down to digits.
// Returns "" when the input doesn't look like a real phone number
// (service codes, extensions, malformed entries, etc.).
//
//   - The leading `+` is dropped (E.164 minus the plus).
//   - `00` international prefix is normalised to nothing.
//   - Embedded letters / pause markers (',', ';', 'p', 'w') are
//     treated as separators and dropped — we don't model DTMF tails.
func normalizePhone(raw string) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	digits := b.String()
	digits = strings.TrimPrefix(digits, "00")
	if len(digits) < phoneMinDigits || len(digits) > phoneMaxDigits {
		return ""
	}
	return digits
}

// detectDefaultCountryCode returns the plurality country-code prefix
// across all 1:1 message JIDs. Walks ZWAMESSAGE.ZFROMJID for
// `*@s.whatsapp.net` rows, peels off the suffix, then takes the
// leading 1-, 2-, and 3-digit prefixes and counts. The most common
// 1–3 digit prefix is returned.
//
// Returns "" if the data is too sparse (< defaultCCMinHits) — better
// to skip default-CC fixup than to mis-attribute hundreds of phones
// to the wrong country.
func detectDefaultCountryCode(db *sql.DB) string {
	rows, err := db.Query(
		`SELECT REPLACE(ZFROMJID, '@s.whatsapp.net', '')
		 FROM   ZWAMESSAGE
		 WHERE  ZFROMJID LIKE '%@s.whatsapp.net'`,
	)
	if err != nil {
		return ""
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var digits sql.NullString
		if err := rows.Scan(&digits); err != nil {
			return ""
		}
		if digits.String == "" {
			continue
		}
		// Try 3-digit then 2-digit then 1-digit prefix; first valid wins.
		for _, k := range []int{3, 2, 1} {
			if k > len(digits.String) {
				continue
			}
			cc := digits.String[:k]
			if _, ok := validCountryCodes[cc]; ok {
				counts[cc]++
				break
			}
		}
	}

	var bestCC string
	var bestN int
	for cc, n := range counts {
		if n > bestN {
			bestCC, bestN = cc, n
		}
	}
	if bestN < defaultCCMinHits {
		return ""
	}
	return bestCC
}

// applyDefaultCountryCode prepends defaultCC to a local-format phone.
//
//   - If the first 1-3 digits already match a valid country code,
//     return as-is (caller can dial it directly).
//   - Otherwise, if defaultCC is set, strip a single leading 0
//     (common national-format trunk prefix) and prepend defaultCC.
//
// Returns "" if no plausible E.164 form can be produced.
func applyDefaultCountryCode(digits, defaultCC string) string {
	if digits == "" {
		return ""
	}
	if isCCPrefixed(digits) {
		return digits
	}
	if defaultCC == "" {
		return ""
	}
	local := strings.TrimLeft(digits, "0")
	out := defaultCC + local
	if len(out) < phoneMinDigits || len(out) > phoneMaxDigits {
		return ""
	}
	return out
}

// isCCPrefixed reports whether digits' leading 1–3 chars match a
// recognised country code.
func isCCPrefixed(digits string) bool {
	for _, k := range []int{3, 2, 1} {
		if k > len(digits) {
			continue
		}
		if _, ok := validCountryCodes[digits[:k]]; ok {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// JID index
// ---------------------------------------------------------------------------

// buildJIDIndex returns the digit-only set of every 1:1 JID seen
// anywhere in ChatStorage. Group JIDs (`*@g.us`) are excluded. LIDs
// (`*@lid`) are also excluded — wa_contact is keyed by phone, and
// the view-level wa_jid_alias join handles LID lookups at query time.
func buildJIDIndex(db *sql.DB) (map[string]struct{}, error) {
	out := make(map[string]struct{}, 4096)
	for _, q := range []string{
		"SELECT DISTINCT ZFROMJID    FROM ZWAMESSAGE     WHERE ZFROMJID    LIKE '%@s.whatsapp.net'",
		"SELECT DISTINCT ZTOJID      FROM ZWAMESSAGE     WHERE ZTOJID      LIKE '%@s.whatsapp.net'",
		"SELECT DISTINCT ZMEMBERJID  FROM ZWAGROUPMEMBER WHERE ZMEMBERJID  LIKE '%@s.whatsapp.net'",
		"SELECT DISTINCT ZCONTACTJID FROM ZWACHATSESSION WHERE ZCONTACTJID LIKE '%@s.whatsapp.net'",
	} {
		rows, err := db.Query(q)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", q, err)
		}
		for rows.Next() {
			var s sql.NullString
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return nil, err
			}
			if s.Valid && s.String != "" {
				out[strings.TrimSuffix(s.String, "@s.whatsapp.net")] = struct{}{}
			}
		}
		rows.Close()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Match
// ---------------------------------------------------------------------------

// matchInternalStats holds the counters surfaced as ContactSyncStats
// fields. Internal so the public type stays JSON-friendly.
type matchInternalStats struct {
	phonesTotal      int
	phonesNormalized int
	phonesMatched    int
}

// matchContacts produces (mapping, personIDByJID, stats):
//
//   - mapping[jidDigits]      = display_name
//   - personIDByJID[jidDigits] = ABPerson.ROWID (used to look up the
//     contact's iOS-Contacts avatar in AddressBookImages.sqlitedb)
//
// Match strategy per phone:
//
//  1. Normalise to digit-only (drops '+', '00', separators).
//  2. If first 1-3 digits are a recognised country code → use as-is.
//  3. Otherwise, if defaultCC is set, prepend (after stripping a
//     leading 0) and try the result.
//  4. Also try the raw form as a fallback — handles contacts saved
//     with a country code that isn't in our validCountryCodes set.
//
// First-seen contact wins on JID collisions. Iterating contacts in
// the slice's order (which mirrors phonesByPerson map iteration —
// non-deterministic, but stable within a single sync) is good
// enough; in practice collisions are rare.
func matchContacts(
	contacts []contact,
	jids map[string]struct{},
	defaultCC string,
) (map[string]string, map[string]int, matchInternalStats) {
	mapping := make(map[string]string, len(contacts))
	personIDByJID := make(map[string]int, len(contacts))
	stats := matchInternalStats{}

	for _, c := range contacts {
		for _, raw := range c.phones {
			stats.phonesTotal++
			digits := normalizePhone(raw)
			if digits == "" {
				continue
			}
			stats.phonesNormalized++

			// Build candidate list in priority order. Avoid
			// duplicates via tracking.
			var candidates []string
			seen := map[string]struct{}{}
			add := func(s string) {
				if s == "" {
					return
				}
				if _, ok := seen[s]; ok {
					return
				}
				seen[s] = struct{}{}
				candidates = append(candidates, s)
			}

			if isCCPrefixed(digits) {
				add(digits)
			} else {
				add(applyDefaultCountryCode(digits, defaultCC))
				add(digits) // raw fallback for unknown CCs
			}

			for _, cand := range candidates {
				if _, ok := jids[cand]; !ok {
					continue
				}
				stats.phonesMatched++
				if _, already := mapping[cand]; !already {
					mapping[cand] = c.displayName
					personIDByJID[cand] = c.personID
				}
				break // a phone hits at most one JID
			}
		}
	}
	return mapping, personIDByJID, stats
}

// ---------------------------------------------------------------------------
// Write-back into ChatStorage
// ---------------------------------------------------------------------------

// ensureContactSchema mirrors the wa_contact / wa_ios_avatar DDL
// from views.sql so this module also works on a workspace where
// views.sql hasn't been refreshed since an upgrade. Idempotent.
func ensureContactSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS wa_contact (
			jid           TEXT PRIMARY KEY,
			display_name  TEXT NOT NULL,
			source        TEXT NOT NULL,
			synced_at     TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS wa_contact_display_name_idx ON wa_contact(display_name)`,
		`CREATE TABLE IF NOT EXISTS wa_ios_avatar (
			jid        TEXT PRIMARY KEY,
			path       TEXT NOT NULL,
			kind       TEXT NOT NULL,
			synced_at  TEXT NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("ensure schema (%q): %w", s, err)
		}
	}
	return nil
}

// writeContactMapping snapshot-rebuilds wa_contact in one
// transaction. Snapshot semantics: iOS Contacts are the source of
// truth, so deletions / renames on the device propagate by wiping
// and re-writing on every sync.
func writeContactMapping(db *sql.DB, mapping map[string]string, now string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM wa_contact"); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("wipe wa_contact: %w", err)
	}
	stmt, err := tx.Prepare(
		`INSERT OR REPLACE INTO wa_contact(jid, display_name, source, synced_at)
		 VALUES (?, ?, ?, ?)`,
	)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()
	for digits, name := range mapping {
		jid := digits + "@s.whatsapp.net"
		if _, err := stmt.Exec(jid, name, contactsSource, now); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert %s: %w", jid, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// iOS Contact avatars
// ---------------------------------------------------------------------------

// writeIOSAvatars snapshot-rebuilds wa_ios_avatar and the workspace's
// profiles/ios/ directory from AddressBookImages.sqlitedb. We extract
// the small `ABThumbnailImage.data` blob (typically <10 KB JPEG) per
// matched JID — full-res images are deliberately not extracted to
// keep the workspace small.
//
// Updates `stats` in place: AvatarsMatched, AvatarsWritten,
// AvatarsFailed, AvatarsNoThumb, plus any per-blob errors appended
// to stats.Errors.
//
// Returns a non-nil error only on catastrophic failures (couldn't
// open the images DB at all, couldn't create the output dir).
func writeIOSAvatars(
	rwDB *sql.DB,
	workspace, abiPath string,
	personIDByJID map[string]int,
	now string,
	stats *ContactSyncStats,
) error {
	outDir := filepath.Join(workspace, iosAvatarSubdir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}

	// Snapshot rebuild: wipe table + dir before writing.
	if _, err := rwDB.Exec("DELETE FROM wa_ios_avatar"); err != nil {
		return fmt.Errorf("wipe wa_ios_avatar: %w", err)
	}
	if err := wipeDirContents(outDir); err != nil {
		// Logged but non-fatal — at worst we end up with a stale
		// file alongside the new ones; the upsert still wins.
		stats.Errors = appendBoundedErr(stats.Errors,
			fmt.Sprintf("wipe %s: %v", outDir, err))
	}

	if len(personIDByJID) == 0 {
		return nil
	}

	// Pull all the thumbs we care about in one query (chunked).
	personIDs := make([]int, 0, len(personIDByJID))
	for _, pid := range personIDByJID {
		personIDs = append(personIDs, pid)
	}
	thumbs, err := readIOSAvatarThumbnails(abiPath, personIDs)
	if err != nil {
		return fmt.Errorf("read thumbs: %w", err)
	}

	// Insert in one transaction.
	tx, err := rwDB.Begin()
	if err != nil {
		return fmt.Errorf("begin avatar tx: %w", err)
	}
	stmt, err := tx.Prepare(
		`INSERT OR REPLACE INTO wa_ios_avatar(jid, path, kind, synced_at)
		 VALUES (?, ?, ?, ?)`,
	)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare avatar insert: %w", err)
	}
	defer stmt.Close()

	for digits, pid := range personIDByJID {
		data, ok := thumbs[pid]
		if !ok || len(data) == 0 {
			stats.AvatarsNoThumb++
			continue
		}
		stats.AvatarsMatched++
		if !looksLikeImage(data) {
			stats.AvatarsFailed++
			continue
		}
		jid := digits + "@s.whatsapp.net"
		filename := safeJIDFilename(jid) + ".jpg"
		outPath := filepath.Join(outDir, filename)
		if err := os.WriteFile(outPath, data, 0o644); err != nil {
			stats.AvatarsFailed++
			stats.Errors = appendBoundedErr(stats.Errors,
				fmt.Sprintf("write %s: %v", outPath, err))
			continue
		}
		// Workspace-relative, forward-slashed — portable.
		relPath := iosAvatarSubdir + "/" + filename
		if _, err := stmt.Exec(jid, relPath, "thumb", now); err != nil {
			stats.AvatarsFailed++
			stats.Errors = appendBoundedErr(stats.Errors,
				fmt.Sprintf("upsert %s: %v", jid, err))
			continue
		}
		stats.AvatarsWritten++
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit avatars: %w", err)
	}
	return nil
}

// readIOSAvatarThumbnails pulls ABThumbnailImage.data for each
// requested ABPerson ROWID. Chunks IN clauses to stay under SQLite's
// default 999-variable limit. Persons without a thumb are silently
// omitted from the result.
//
// Returns an empty map (not an error) when the images DB is missing
// the ABThumbnailImage table — older iOS versions and devices that
// have never had a contact photo lack it.
func readIOSAvatarThumbnails(abiPath string, personIDs []int) (map[int][]byte, error) {
	if len(personIDs) == 0 {
		return map[int][]byte{}, nil
	}
	uri := "file:" + abiPath + "?mode=ro&immutable=1"
	db, err := sql.Open("sqlite3", uri)
	if err != nil {
		return nil, fmt.Errorf("open abi db: %w", err)
	}
	defer db.Close()

	// Verify the expected table exists.
	var name string
	if err := db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='ABThumbnailImage'",
	).Scan(&name); err != nil {
		// Either the row is genuinely missing (sql.ErrNoRows) or
		// some other error. Treat both as "no thumbs available" —
		// they're optional.
		return map[int][]byte{}, nil
	}

	const chunk = 500
	out := make(map[int][]byte, len(personIDs))
	for i := 0; i < len(personIDs); i += chunk {
		end := i + chunk
		if end > len(personIDs) {
			end = len(personIDs)
		}
		batch := personIDs[i:end]
		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1] // trim trailing comma
		args := make([]any, len(batch))
		for j, v := range batch {
			args[j] = v
		}
		q := fmt.Sprintf(
			"SELECT record_id, data FROM ABThumbnailImage WHERE record_id IN (%s)",
			placeholders,
		)
		rows, err := db.Query(q, args...)
		if err != nil {
			return nil, fmt.Errorf("query thumbs: %w", err)
		}
		for rows.Next() {
			var pid int
			var blob []byte
			if err := rows.Scan(&pid, &blob); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan thumb: %w", err)
			}
			if len(blob) > 0 {
				out[pid] = blob
			}
		}
		rows.Close()
	}
	return out, nil
}

// wipeDirContents removes every regular file directly under dir.
// Subdirectories are preserved (we never create them; if the user
// dropped one in deliberately, blowing it away unannounced would be
// too surprising).
func wipeDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
