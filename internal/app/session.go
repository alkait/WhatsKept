package app

import (
	"sync"

	"whatskept/internal/secrets"
)

// passwordStore holds the iOS backup password in RAM for the lifetime
// of a single app session. One slot for the whole process: a fresh
// backup (idevicebackup2) and a messages sync (postprocess.SyncMessages)
// both authenticate against the same iPhone-side encryption password,
// so caching once unblocks both flows.
//
// Cleared when:
//   - the user opens or creates a different workspace,
//   - DELETE /api/session/password is invoked (the "use a different
//     password" affordance in the UI), or
//   - the process exits.
//
// Deliberately not persisted to disk in this iteration. The durable
// variant lives behind option C (macOS Keychain) and is a separate
// piece of work.
type passwordStore struct {
	mu sync.RWMutex
	v  string
}

func newPasswordStore() *passwordStore { return &passwordStore{} }

func (p *passwordStore) get() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.v
}

func (p *passwordStore) set(v string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.v = v
}

func (p *passwordStore) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.v = ""
}

func (p *passwordStore) has() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.v != ""
}

// apiKeyStore holds the OpenRouter API key for the cloud media describer.
// Unlike the backup password, the key is a GLOBAL account credential (not
// workspace-specific), so it is NOT cleared on workspace switches.
//
// It can optionally be persisted across restarts via the cross-platform
// credentials file (internal/secrets): on construction the store loads any
// persisted key into RAM; set(persist=true) writes it; clear() (the "forget
// key" affordance) removes it from both RAM and disk. set(persist=false)
// keeps the key session-only and removes any previously persisted copy.
type apiKeyStore struct {
	mu        sync.RWMutex
	v         string
	persisted bool
}

// newAPIKeyStore loads any persisted key so the cloud describer works
// immediately after a restart.
func newAPIKeyStore() *apiKeyStore {
	a := &apiKeyStore{}
	if k, ok := secrets.LoadOpenRouterKey(); ok {
		a.v = k
		a.persisted = true
	}
	return a
}

func (a *apiKeyStore) get() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.v
}

// set stores the key in RAM and, when persist is true, on disk. When
// persist is false any previously persisted copy is removed (session-only).
func (a *apiKeyStore) set(v string, persist bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v = v
	if persist {
		if err := secrets.SaveOpenRouterKey(v); err != nil {
			return err
		}
		a.persisted = true
		return nil
	}
	if a.persisted {
		if err := secrets.DeleteOpenRouterKey(); err != nil {
			return err
		}
	}
	a.persisted = false
	return nil
}

// clear forgets the key from RAM and deletes any persisted copy.
func (a *apiKeyStore) clear() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v = ""
	a.persisted = false
	return secrets.DeleteOpenRouterKey()
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
