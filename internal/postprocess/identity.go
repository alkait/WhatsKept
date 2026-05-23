package postprocess

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"whatskept/internal/backup"
	"whatskept/internal/binding"
)

// DetectOwnerJID returns the WhatsApp JID belonging to the account
// whose backup this database came from, e.g. "971504320432@s.whatsapp.net".
//
// Heuristic: the account owner is necessarily a member of every group
// they're part of, while no other contact is. We count occurrences of
// each `<digits>@s.whatsapp.net` value in ZWAGROUPMEMBER. The top JID
// is accepted as the owner when it dominates the runner-up by ≥ 2×
// AND appears in at least 3 groups — those thresholds rule out the
// degenerate "user is in 0-or-1 groups" case where we have no signal.
//
// Returns ("", nil) when the heuristic doesn't reach a confident
// answer (no groups, or no clear winner). Callers should then fall
// back to a Fingerprint comparison for identity guarding.
//
// Other JID flavours observed in ChatStorage are deliberately filtered
// out: `@lid` is WhatsApp's "linked identity" hash used across linked
// devices (not a real phone number), `@status` rows are status-update
// chats (not group members), `@g.us` are group JIDs (not people),
// and `@broadcast` is WhatsApp's broadcast list pseudo-JID.
func DetectOwnerJID(db *sql.DB) (string, error) {
	const q = `
		SELECT ZMEMBERJID, COUNT(*) AS c
		FROM ZWAGROUPMEMBER
		WHERE ZMEMBERJID LIKE '%@s.whatsapp.net'
		GROUP BY ZMEMBERJID
		ORDER BY c DESC
		LIMIT 2
	`
	rows, err := db.Query(q)
	if err != nil {
		// ZWAGROUPMEMBER table missing in an unfamiliar schema → caller
		// falls back to fingerprint. Don't treat as fatal.
		return "", nil
	}
	defer rows.Close()

	type entry struct {
		jid string
		n   int
	}
	var top, runner entry
	idx := 0
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.jid, &e.n); err != nil {
			return "", fmt.Errorf("scan owner jid: %w", err)
		}
		switch idx {
		case 0:
			top = e
		case 1:
			runner = e
		}
		idx++
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate owner jid: %w", err)
	}
	if top.n < 3 {
		return "", nil // not enough signal
	}
	// Top must clearly dominate. If two contacts happen to share the
	// same group set (rare but possible in a 2-group household), bail
	// rather than pick the wrong one.
	if runner.n*2 > top.n {
		return "", nil
	}
	return top.jid, nil
}

// Fingerprint returns a SHA-256 of the workspace's chat-session set
// (sorted, distinct `@s.whatsapp.net` and `@g.us` JIDs from
// ZWACHATSESSION). This is the schema-agnostic identity guard: two
// dumps from the same WhatsApp account will share the bulk of their
// chat-session JIDs (modulo whatever new contacts/groups have been
// added between syncs), while two dumps from different accounts will
// not.
//
// Returns ("", nil) if ZWACHATSESSION is missing or empty.
//
// The `@status` and `@broadcast` JIDs are intentionally excluded:
// they're either WhatsApp's own pseudo-JIDs (constant across all
// installs) or transient status-update entries (high churn), neither
// of which adds identity signal.
func Fingerprint(db *sql.DB) (string, error) {
	const q = `
		SELECT DISTINCT ZCONTACTJID
		FROM ZWACHATSESSION
		WHERE ZCONTACTJID LIKE '%@s.whatsapp.net'
		   OR ZCONTACTJID LIKE '%@g.us'
		ORDER BY ZCONTACTJID
	`
	rows, err := db.Query(q)
	if err != nil {
		return "", nil
	}
	defer rows.Close()

	h := sha256.New()
	any := false
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			return "", fmt.Errorf("scan jid for fingerprint: %w", err)
		}
		h.Write([]byte(jid))
		h.Write([]byte{0}) // separator so "ab"+"c" ≠ "a"+"bc"
		any = true
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate fingerprint: %w", err)
	}
	if !any {
		return "", nil
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// ReadIdentity opens a freshly-decrypted ChatStorage.sqlite read-only,
// runs DetectOwnerJID + Fingerprint against it, and combines the
// results with the backup's device metadata into an unsaved Binding
// ready to be persisted by binding.Save (first sync) or compared via
// CompareIdentity (subsequent syncs).
//
// The returned Binding is missing BoundAt and LastSyncedAt — those
// are timestamps the caller stamps once the sync as a whole succeeds.
func ReadIdentity(dbPath string, info backup.Info) (*binding.Binding, error) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open db for identity: %w", err)
	}
	defer db.Close()

	jid, err := DetectOwnerJID(db)
	if err != nil {
		return nil, fmt.Errorf("detect owner jid: %w", err)
	}
	fp, err := Fingerprint(db)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: %w", err)
	}
	return &binding.Binding{
		UDID:        filepath.Base(info.Path),
		DeviceName:  info.DeviceName,
		ProductType: info.ProductType,
		OwnerJID:    jid,
		Fingerprint: fp,
	}, nil
}

// ErrIdentityMismatch is returned by CompareIdentity when the freshly
// decrypted backup belongs to a different WhatsApp account than the
// one this workspace is bound to. The sync layer treats this as a
// hard stop: the live ChatStorage.sqlite is never overwritten when
// this error fires.
//
// Both sides of the mismatch are attached so the UI can render a
// "<old phone> → <new phone>" diff and let the user choose to either
// abort (default) or explicitly forget the binding and re-bind.
type ErrIdentityMismatch struct {
	Bound binding.Binding
	Fresh binding.Binding
}

func (e *ErrIdentityMismatch) Error() string {
	bp := e.Bound.Phone()
	fp := e.Fresh.Phone()
	if bp == "" {
		bp = "this workspace's WhatsApp account"
	}
	if fp == "" {
		fp = "another WhatsApp account"
	}
	return fmt.Sprintf(
		"The backup is signed in as %s, but this workspace is bound to %s. "+
			"Overwriting would silently mix two accounts. "+
			"Use a different workspace for the other account, or click "+
			"\"Forget this workspace's identity\" below to re-bind from scratch.",
		fp, bp,
	)
}

// CompareIdentity returns *ErrIdentityMismatch when `fresh` looks like
// a different WhatsApp account than `bound`, or nil if they match (or
// if we lack the signal to tell them apart).
//
// Decision table:
//
//	bound JID  | fresh JID | result
//	-----------+-----------+-----------------------------------------
//	set        | set       | match iff equal; else mismatch
//	set        | empty     | mismatch (suspiciously zero groups)
//	empty      | set       | match (binding gains a JID; up to caller
//	           |           |        to persist it)
//	empty      | empty     | match (no signal — accept)
//
// The "bound set, fresh empty" case is the conservative call: an
// account that previously had detectable groups but now has none is
// implausible enough to warrant the user explicitly confirming via
// the forget-identity escape hatch. We'd rather fail loud than
// silently bind to the wrong data.
func CompareIdentity(bound, fresh *binding.Binding) error {
	switch {
	case bound.OwnerJID != "" && fresh.OwnerJID != "":
		if bound.OwnerJID == fresh.OwnerJID {
			return nil
		}
		return &ErrIdentityMismatch{Bound: *bound, Fresh: *fresh}
	case bound.OwnerJID != "" && fresh.OwnerJID == "":
		return &ErrIdentityMismatch{Bound: *bound, Fresh: *fresh}
	default:
		// bound has no JID (legacy/edge) — we have no prior signal
		// to refuse against. Accept and let the caller refresh the
		// binding with whatever fresh provides.
		return nil
	}
}
