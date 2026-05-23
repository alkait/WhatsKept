// Package backup discovers iOS backups under
// ~/Library/Application Support/MobileSync/Backup/ and reads their metadata
// (Info.plist + Manifest.plist).
//
// Mirrors the existing Python `whatskept.backup` module so the Go and Python
// implementations stay behaviourally identical during the rewrite.
package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"howett.net/plist"
)

// DefaultBackupRoot is the canonical macOS location.
//
// The actual returned value depends on $HOME at runtime; see DefaultRoot().
const defaultBackupRootRel = "Library/Application Support/MobileSync/Backup"

// DefaultRoot returns the default backup discovery root for the current user.
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return defaultBackupRootRel
	}
	return filepath.Join(home, defaultBackupRootRel)
}

// ErrAccessDenied is returned when the backup root exists but cannot be read,
// typically because the calling process lacks macOS Full Disk Access.
var ErrAccessDenied = errors.New("backup root cannot be read (grant Full Disk Access?)")

// Info is the parsed metadata for one backup directory.
type Info struct {
	Path           string    // absolute path to the backup directory
	DeviceName     string    // e.g. "Ayman's iPhone"
	ProductType    string    // e.g. "iPhone15,4"
	ProductVersion string    // e.g. "26.3.1"
	LastBackup     time.Time // zero value if unknown
	IsEncrypted    bool
}

// DisplayName is "<device> (<product>, iOS <version>)".
func (b Info) DisplayName() string {
	return fmt.Sprintf("%s (%s, iOS %s)", b.DeviceName, b.ProductType, b.ProductVersion)
}

// LastBackupString is "YYYY-MM-DD HH:MM" or "unknown".
func (b Info) LastBackupString() string {
	if b.LastBackup.IsZero() {
		return "unknown"
	}
	return b.LastBackup.Format("2006-01-02 15:04")
}

// LoadInfo reads Info.plist + Manifest.plist for a single backup directory.
// Returns nil, nil if `dir` does not look like a valid iOS backup (missing
// or unparseable plists). Other I/O errors are returned as-is.
func LoadInfo(dir string) (*Info, error) {
	infoPlist, err := readPlist(filepath.Join(dir, "Info.plist"))
	if err != nil || infoPlist == nil {
		return nil, err
	}
	manifestPlist, err := readPlist(filepath.Join(dir, "Manifest.plist"))
	if err != nil || manifestPlist == nil {
		return nil, err
	}

	getString := func(m map[string]any, keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}

	deviceName := getString(infoPlist, "Device Name", "Display Name")
	if deviceName == "" {
		deviceName = "(unnamed)"
	}
	productType := getString(infoPlist, "Product Type")
	if productType == "" {
		productType = "?"
	}
	productVersion := getString(infoPlist, "Product Version")
	if productVersion == "" {
		productVersion = "?"
	}

	var lastBackup time.Time
	if v, ok := infoPlist["Last Backup Date"]; ok {
		if t, ok := v.(time.Time); ok {
			lastBackup = t
		}
	}

	encrypted := false
	if v, ok := manifestPlist["IsEncrypted"]; ok {
		if b, ok := v.(bool); ok {
			encrypted = b
		}
	}

	return &Info{
		Path:           dir,
		DeviceName:     deviceName,
		ProductType:    productType,
		ProductVersion: productVersion,
		LastBackup:     lastBackup,
		IsEncrypted:    encrypted,
	}, nil
}

// Discover returns every valid backup under `root`, newest first.
//
// If `root` does not exist, returns an empty slice (no error). If it exists
// but cannot be read, returns ErrAccessDenied (typically a Full Disk Access
// problem on macOS).
func Discover(root string) ([]Info, error) {
	stat, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", root, err)
	}
	if !stat.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("%w: %s", ErrAccessDenied, root)
		}
		return nil, fmt.Errorf("readdir %q: %w", root, err)
	}

	var out []Info
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := LoadInfo(filepath.Join(root, e.Name()))
		if err != nil || info == nil {
			continue
		}
		out = append(out, *info)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].LastBackup.After(out[j].LastBackup)
	})
	return out, nil
}

// readPlist parses a plist file (binary or XML) into a map.
// Returns (nil, nil) if the file does not exist or is not parseable as a
// top-level dict.
func readPlist(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var v map[string]any
	dec := plist.NewDecoder(f)
	if err := dec.Decode(&v); err != nil {
		return nil, nil // not a valid plist; treated as "not a backup"
	}
	return v, nil
}

// FormatListing renders a numbered listing of backups (1-indexed), one per
// line, padded into a tidy column. Used by `whatskept list`.
func FormatListing(backups []Info) string {
	if len(backups) == 0 {
		return "(no backups found)"
	}
	widthIdx := len(fmt.Sprintf("%d", len(backups)))
	widthDev := 0
	for _, b := range backups {
		if w := len(b.DisplayName()); w > widthDev {
			widthDev = w
		}
	}
	var sb []byte
	for i, b := range backups {
		lock := "encrypted"
		if !b.IsEncrypted {
			lock = "unencrypted"
		}
		line := fmt.Sprintf(
			"  [%*d] %-*s  %s  %s\n",
			widthIdx, i+1,
			widthDev, b.DisplayName(),
			b.LastBackupString(),
			lock,
		)
		sb = append(sb, line...)
	}
	if len(sb) > 0 {
		sb = sb[:len(sb)-1] // drop trailing newline
	}
	return string(sb)
}
