// Package binding persists a workspace's "identity" — which iPhone
// backup (by UDID) and which WhatsApp account (by JID) the workspace
// is tied to. The binding is established on the first successful
// sync and verified on every subsequent one, so a different device's
// or account's backup can never silently overwrite the workspace.
//
// The binding lives in <workspace>/.whatskept.json. Workspace-local
// (not in ~/Library/Application Support/) so that copying or moving
// a workspace carries its identity with it.
package binding

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Filename is the workspace-local file we write/read.
const Filename = ".whatskept.json"

// Binding is the on-disk identity record. All fields except BoundAt
// are optional from a JSON-parsing perspective so older or partial
// files don't crash older binaries; semantic requirements (must have
// UDID + OwnerJID) are enforced by the sync layer.
type Binding struct {
	// UDID is the iOS backup directory name, e.g. "00008101-…F9DA".
	// Used to filter the backup pool to "this workspace's device".
	UDID string `json:"udid"`

	// DeviceName is the human-readable name from the backup's
	// Info.plist (e.g. "Aiman's iPhone"). Refreshed on every sync —
	// the user might rename the phone in Settings.
	DeviceName string `json:"device_name,omitempty"`

	// ProductType is e.g. "iPhone15,4". Useful only for display.
	ProductType string `json:"product_type,omitempty"`

	// OwnerJID is the WhatsApp account's own JID, e.g.
	// "971504320432@s.whatsapp.net". Detected by sampling
	// ZWAGROUPMEMBER for the JID that appears in the vast majority
	// of the user's groups; see postprocess.DetectOwnerJID.
	//
	// Empty if the detection heuristic couldn't reach a confident
	// answer (e.g. a brand-new WhatsApp account with no groups);
	// in that case Fingerprint alone guards subsequent syncs.
	OwnerJID string `json:"owner_jid,omitempty"`

	// Fingerprint is a stable content hash of the workspace's chat
	// session set (sorted distinct ZWACHATSESSION.ZCONTACTJID values,
	// SHA-256). Acts as a schema-agnostic identity check when
	// OwnerJID isn't available.
	Fingerprint string `json:"fingerprint,omitempty"`

	// BoundAt is the timestamp of the first successful sync that
	// established this binding.
	BoundAt time.Time `json:"bound_at"`

	// LastSyncedAt is refreshed on every successful sync. Distinct
	// from the ChatStorage.sqlite mtime so we can record a sync
	// even if the file size/content didn't change.
	LastSyncedAt time.Time `json:"last_synced_at,omitempty"`
}

// Phone returns "+<digits>" extracted from OwnerJID, or "" if no
// JID is bound. Strips the "@s.whatsapp.net" suffix and prepends "+".
// No locale-aware formatting — the E.164 number is unambiguous.
func (b Binding) Phone() string {
	if b.OwnerJID == "" {
		return ""
	}
	at := strings.IndexByte(b.OwnerJID, '@')
	if at <= 0 {
		return ""
	}
	digits := b.OwnerJID[:at]
	for _, r := range digits {
		if r < '0' || r > '9' {
			return "" // not a phone-shaped JID
		}
	}
	return "+" + digits
}

// Path returns the absolute path of the binding file for a workspace.
func Path(workspace string) string {
	return filepath.Join(workspace, Filename)
}

// Load reads the binding for a workspace. Returns (nil, nil) when the
// file doesn't exist — that's the "unbound workspace" signal, not an
// error. Other I/O or JSON errors are returned as-is so the caller
// can choose to surface vs. ignore.
func Load(workspace string) (*Binding, error) {
	data, err := os.ReadFile(Path(workspace))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read binding: %w", err)
	}
	var b Binding
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parse binding: %w", err)
	}
	return &b, nil
}

// Save writes the binding atomically (write-temp + rename) so a crash
// mid-write can never leave a half-written .whatskept.json on disk.
// Mode 0644 — nothing secret in here.
func Save(workspace string, b *Binding) error {
	if b == nil {
		return errors.New("nil binding")
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal binding: %w", err)
	}
	dst := Path(workspace)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write binding tmp: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename binding: %w", err)
	}
	return nil
}

// Delete removes the binding file, if any. Used by the "forget this
// workspace's identity" escape hatch in the mismatch modal — lets
// the next sync re-bind from scratch instead of refusing.
func Delete(workspace string) error {
	err := os.Remove(Path(workspace))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
