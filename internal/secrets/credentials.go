package secrets

// Cross-platform persistence for app-managed credentials: the OpenRouter
// API key (per workspace, for the cloud image describer) and iOS backup
// passwords (per device). Both are keyed maps, not single values.
//
// Stored as a 0600 JSON file under the OS per-user config dir:
//   macOS:   ~/Library/Application Support/whatskept/credentials.json
//   Windows: %AppData%\whatskept\credentials.json
//   Linux:   ~/.config/whatskept/credentials.json
//
// Plaintext on disk (no OS keystore is used, so any app-held encryption
// key would itself have to live in a readable file — encryption-at-rest
// would be theater). This mirrors how the backup password already lives
// in plaintext in the workspace .env. Set WHATSKEPT_CONFIG_DIR to relocate
// the directory (tests point it at a temp dir for isolation).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const (
	configDirEnv        = "WHATSKEPT_CONFIG_DIR"
	appConfigDirName    = "whatskept"
	credentialsFileName = "credentials.json"
)

// credentials is the on-disk shape of credentials.json. Add fields here
// as more app-level secrets need to persist.
//
// Both secret kinds are keyed maps: OpenRouterKeys by absolute workspace
// path (each workspace has its own API key) and BackupPasswords by device
// UDID (each iOS backup has its own encryption password).
//
// OpenRouterAPIKey is the pre-per-workspace global key from older versions,
// kept only so it can be migrated into the first workspace opened after an
// upgrade (see MigrateLegacyOpenRouterKey); it is dropped once migrated.
type credentials struct {
	OpenRouterKeys   map[string]string `json:"openrouter_keys,omitempty"`
	OpenRouterAPIKey string            `json:"openrouter_api_key,omitempty"` // legacy global key, migrated on first load
	BackupPasswords  map[string]string `json:"backup_passwords,omitempty"`
}

// isEmpty reports whether the file would carry no secrets (so it can be
// removed rather than left as an empty husk). A struct == comparison can't
// be used now that map fields are present.
func (c credentials) isEmpty() bool {
	return len(c.OpenRouterKeys) == 0 && c.OpenRouterAPIKey == "" && len(c.BackupPasswords) == 0
}

// appConfigDir resolves the per-user config directory, honouring the
// WHATSKEPT_CONFIG_DIR override.
func appConfigDir() (string, error) {
	if base := os.Getenv(configDirEnv); base != "" {
		return base, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, appConfigDirName), nil
}

func credentialsPath() (string, error) {
	dir, err := appConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, credentialsFileName), nil
}

// loadCredentials reads the file; a missing file is not an error (returns
// the zero value).
func loadCredentials() (credentials, error) {
	var c credentials
	p, err := credentialsPath()
	if err != nil {
		return c, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	return c, nil
}

// saveCredentials writes the file atomically with 0600 perms, creating the
// config dir (0700) if needed.
func saveCredentials(c credentials) error {
	p, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	// os.Rename replaces an existing target on all supported platforms
	// (incl. Windows via MoveFileEx), giving an atomic swap.
	return os.Rename(tmp, p)
}

// LoadOpenRouterKey returns the persisted OpenRouter key for a workspace and
// whether one is present. Best-effort: any read/parse error, or an empty
// workspace, reports "not present".
func LoadOpenRouterKey(workspace string) (string, bool) {
	if workspace == "" {
		return "", false
	}
	c, err := loadCredentials()
	if err != nil {
		return "", false
	}
	k, ok := c.OpenRouterKeys[workspace]
	if !ok || k == "" {
		return "", false
	}
	return k, true
}

// SaveOpenRouterKey persists the key for a workspace, preserving any other
// fields (incl. other workspaces' keys and all backup passwords).
func SaveOpenRouterKey(workspace, key string) error {
	if workspace == "" {
		return errors.New("SaveOpenRouterKey: empty workspace")
	}
	c, err := loadCredentials()
	if err != nil {
		// Corrupt/unreadable file: overwrite rather than refuse to save.
		c = credentials{}
	}
	if c.OpenRouterKeys == nil {
		c.OpenRouterKeys = map[string]string{}
	}
	c.OpenRouterKeys[workspace] = key
	return saveCredentials(c)
}

// DeleteOpenRouterKey removes the persisted key for one workspace. If
// nothing else remains in the file it is deleted entirely. A missing file
// or absent entry is a no-op.
func DeleteOpenRouterKey(workspace string) error {
	c, err := loadCredentials()
	if err != nil {
		return err
	}
	if _, ok := c.OpenRouterKeys[workspace]; !ok {
		return nil
	}
	delete(c.OpenRouterKeys, workspace)
	if len(c.OpenRouterKeys) == 0 {
		c.OpenRouterKeys = nil
	}
	return saveOrRemove(c)
}

// MigrateLegacyOpenRouterKey moves the pre-per-workspace global key (written
// by older versions) into the given workspace's slot and removes the global
// copy, returning the adopted key. It runs once, the first time an existing
// workspace is opened after an upgrade, so the user's single configured key
// isn't lost — it's handed to the workspace they open first. Reports
// ("", false) when there is no legacy key (the normal case).
func MigrateLegacyOpenRouterKey(workspace string) (string, bool) {
	if workspace == "" {
		return "", false
	}
	c, err := loadCredentials()
	if err != nil || c.OpenRouterAPIKey == "" {
		return "", false
	}
	if c.OpenRouterKeys == nil {
		c.OpenRouterKeys = map[string]string{}
	}
	if _, ok := c.OpenRouterKeys[workspace]; !ok {
		c.OpenRouterKeys[workspace] = c.OpenRouterAPIKey
	}
	c.OpenRouterAPIKey = ""
	if err := saveCredentials(c); err != nil {
		return "", false
	}
	return c.OpenRouterKeys[workspace], true
}

// saveOrRemove writes the credentials file, or deletes it entirely when no
// secrets remain (so we don't leave an empty {} file behind). A missing
// file on removal is a no-op.
func saveOrRemove(c credentials) error {
	if c.isEmpty() {
		p, err := credentialsPath()
		if err != nil {
			return err
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return saveCredentials(c)
}

// LoadBackupPassword returns the persisted backup password for the given
// device UDID and whether one is present. Best-effort: any read/parse
// error reports "not present".
func LoadBackupPassword(udid string) (string, bool) {
	if udid == "" {
		return "", false
	}
	c, err := loadCredentials()
	if err != nil {
		return "", false
	}
	pw, ok := c.BackupPasswords[udid]
	if !ok || pw == "" {
		return "", false
	}
	return pw, true
}

// SaveBackupPassword persists the backup password for a device UDID,
// preserving any other fields (incl. other devices' passwords).
func SaveBackupPassword(udid, password string) error {
	if udid == "" {
		return errors.New("SaveBackupPassword: empty udid")
	}
	c, err := loadCredentials()
	if err != nil {
		// Corrupt/unreadable file: overwrite rather than refuse to save.
		c = credentials{}
	}
	if c.BackupPasswords == nil {
		c.BackupPasswords = map[string]string{}
	}
	c.BackupPasswords[udid] = password
	return saveCredentials(c)
}

// DeleteBackupPassword removes the persisted password for one device. If
// nothing else remains in the file it is deleted entirely. A missing file
// or absent entry is a no-op.
func DeleteBackupPassword(udid string) error {
	c, err := loadCredentials()
	if err != nil {
		return err
	}
	if _, ok := c.BackupPasswords[udid]; !ok {
		return nil
	}
	delete(c.BackupPasswords, udid)
	if len(c.BackupPasswords) == 0 {
		c.BackupPasswords = nil
	}
	return saveOrRemove(c)
}
