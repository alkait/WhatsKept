package app

import "sync"

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
