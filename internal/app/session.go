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

// apiKeyStore holds the OpenRouter API key in RAM for the lifetime of
// a single app session, exactly like passwordStore holds the backup
// password. Same lifecycle: set when the user enters it, used by the
// cloud media describer, cleared on workspace switch / "use a different
// key" / process exit. Deliberately not persisted to disk — the key
// never lands in the workspace or any file (a durable Keychain variant
// is separate future work, mirroring the password's option C).
type apiKeyStore struct {
	mu sync.RWMutex
	v  string
}

func newAPIKeyStore() *apiKeyStore { return &apiKeyStore{} }

func (a *apiKeyStore) get() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.v
}

func (a *apiKeyStore) set(v string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v = v
}

func (a *apiKeyStore) clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v = ""
}

func (a *apiKeyStore) has() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.v != ""
}
