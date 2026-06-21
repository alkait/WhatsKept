package app

import (
	"sync"

	"whatskept/internal/secrets"
)

// passwordStore holds the iOS backup password for the active workspace's
// device. One slot for the whole process: a fresh backup (idevicebackup2)
// and a messages sync (postprocess.SyncMessages) both authenticate against
// the same iPhone-side encryption password, so caching once unblocks both
// flows.
//
// The password can optionally be persisted across restarts via the
// cross-platform credentials file (internal/secrets), keyed by device
// UDID — each iOS backup has its own encryption password, so unlike the
// global OpenRouter key this is per-device. On opening a workspace the
// store loads any persisted password for that device; setVerified(persist)
// writes it (only ever called once the backup/sync has actually verified
// the password); forget() removes it from both RAM and disk.
//
// The in-RAM copy is reset (without touching disk) whenever the active
// workspace changes — loadForWorkspace repopulates it for the new device.
type passwordStore struct {
	mu        sync.RWMutex
	v         string
	udid      string // device the cached password belongs to ("" = unknown)
	persisted bool
}

func newPasswordStore() *passwordStore { return &passwordStore{} }

func (p *passwordStore) get() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.v
}

// set refreshes the in-RAM password without touching persistence. Used on
// the reuse-cached-password path (where the value didn't change, so any
// persisted copy must be left intact).
func (p *passwordStore) set(v string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.v = v
}

// setVerified caches a password the caller has just verified (a backup or
// sync succeeded with it) for device udid. An empty udid falls back to the
// active device. When persist is true the password is written to disk;
// when false any previously persisted copy for that device is removed
// (the user explicitly chose not to remember it).
func (p *passwordStore) setVerified(udid, v string, persist bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if udid != "" {
		p.udid = udid
	}
	p.v = v
	if persist && p.udid != "" {
		if err := secrets.SaveBackupPassword(p.udid, v); err != nil {
			return err
		}
		p.persisted = true
		return nil
	}
	if !persist && p.persisted && p.udid != "" {
		if err := secrets.DeleteBackupPassword(p.udid); err != nil {
			return err
		}
		p.persisted = false
	}
	return nil
}

// loadForWorkspace points the store at a new active device and loads any
// persisted password for it into RAM (skipping the prompt after a
// restart). Resets the in-RAM state first; never deletes from disk. Called
// when a workspace is opened or created (udid "" for a brand-new, unbound
// workspace).
func (p *passwordStore) loadForWorkspace(udid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.v = ""
	p.udid = udid
	p.persisted = false
	if udid == "" {
		return
	}
	if pw, ok := secrets.LoadBackupPassword(udid); ok {
		p.v = pw
		p.persisted = true
	}
}

// forget removes the cached password from RAM and any persisted copy for
// the active device. Backs the "use a different password" and Settings
// "forget" affordances.
func (p *passwordStore) forget() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.v = ""
	p.persisted = false
	if p.udid == "" {
		return nil
	}
	return secrets.DeleteBackupPassword(p.udid)
}

func (p *passwordStore) has() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.v != ""
}

func (p *passwordStore) isPersisted() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.persisted
}

// device returns the UDID the active workspace's backup belongs to, or "" if
// the workspace isn't bound to a device yet.
func (p *passwordStore) device() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.udid
}

// apiKeyStore holds the OpenRouter API key for the active workspace's cloud
// media describer. Like the backup password (and unlike older versions,
// which kept one global key), the key is per workspace: each workspace has
// its own credential, persisted keyed by workspace path in the cross-platform
// credentials file (internal/secrets).
//
// On opening a workspace the store loads that workspace's persisted key (if
// any) via loadForWorkspace; set(persist=true) writes it; set(persist=false)
// keeps it session-only and removes any persisted copy; clear() (the "forget
// key" affordance, also used when a workspace is deleted) removes it from
// both RAM and disk. The in-RAM copy is reset whenever the active workspace
// changes — loadForWorkspace repopulates it for the new workspace.
type apiKeyStore struct {
	mu        sync.RWMutex
	v         string
	path      string // workspace the cached key belongs to ("" = none active)
	persisted bool
}

func newAPIKeyStore() *apiKeyStore { return &apiKeyStore{} }

func (a *apiKeyStore) get() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.v
}

// loadForWorkspace points the store at a workspace and loads that
// workspace's persisted key into RAM (skipping the prompt after a restart).
// Resets the in-RAM state first; never deletes from disk. When migrateLegacy
// is true and the workspace has no key of its own, the pre-per-workspace
// global key (if any) is adopted for it — a one-time upgrade path used when
// opening an existing workspace, not when creating a fresh one.
func (a *apiKeyStore) loadForWorkspace(path string, migrateLegacy bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v = ""
	a.path = path
	a.persisted = false
	if path == "" {
		return
	}
	if k, ok := secrets.LoadOpenRouterKey(path); ok {
		a.v = k
		a.persisted = true
		return
	}
	if migrateLegacy {
		if k, ok := secrets.MigrateLegacyOpenRouterKey(path); ok {
			a.v = k
			a.persisted = true
		}
	}
}

// set stores the key in RAM and, when persist is true and a workspace is
// active, on disk for that workspace. When persist is false any previously
// persisted copy for the active workspace is removed (session-only).
func (a *apiKeyStore) set(v string, persist bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v = v
	if persist && a.path != "" {
		if err := secrets.SaveOpenRouterKey(a.path, v); err != nil {
			return err
		}
		a.persisted = true
		return nil
	}
	if a.persisted && a.path != "" {
		if err := secrets.DeleteOpenRouterKey(a.path); err != nil {
			return err
		}
	}
	a.persisted = false
	return nil
}

// clear forgets the key from RAM and deletes any persisted copy for the
// active workspace.
func (a *apiKeyStore) clear() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v = ""
	a.persisted = false
	if a.path == "" {
		return nil
	}
	return secrets.DeleteOpenRouterKey(a.path)
}

func (a *apiKeyStore) has() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.v != ""
}

func (a *apiKeyStore) isPersisted() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.persisted
}
