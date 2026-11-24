package live

// App-state recovery. WhatsApp syncs settings (archive, pins, mutes,
// lists…) in five collections, each verified against a running hash. A
// collection whose incremental sync breaks — a patch that no longer
// verifies, or patches the server has dropped since live last synced —
// stays stuck until it is downloaded whole again. whatsmeow leaves that
// to the caller, so live does it: after connecting it brings every
// collection up to date, and any incremental sync that fails is retried
// once as a full sync, at most once an hour per collection.
//
// The vendored whatsmeow accepts a full sync whose hash cannot add up
// (the phone folds in records it never uploads) as long as every record
// verifies on its own, the way WhatsApp's own linked devices carry on.

import (
	"context"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/appstate"
)

const fullSyncEvery = time.Hour

// appStateHealer retries broken collections with a full sync.
type appStateHealer struct {
	// fetch syncs one collection: incrementally, or whole when full.
	fetch func(ctx context.Context, name appstate.WAPatchName, full bool) error
	now   func() time.Time
	// afterFull, when set, runs after a successful full sync: full syncs
	// emit no per-record events, so anything kept from those has to be
	// re-read.
	afterFull func(ctx context.Context, name appstate.WAPatchName)

	mu   sync.Mutex
	last map[appstate.WAPatchName]time.Time // last full sync started
}

func newAppStateHealer(fetch func(context.Context, appstate.WAPatchName, bool) error) *appStateHealer {
	return &appStateHealer{fetch: fetch, now: time.Now, last: map[appstate.WAPatchName]time.Time{}}
}

// syncAll brings every collection up to date, healing any that fail.
func (h *appStateHealer) syncAll(ctx context.Context) {
	for _, name := range appstate.AllPatchNames {
		if err := h.fetch(ctx, name, false); err != nil {
			h.heal(ctx, name, err)
		}
	}
}

// heal answers a failed incremental sync of name with a full sync,
// unless one ran within the last hour. Reports whether it ran one.
func (h *appStateHealer) heal(ctx context.Context, name appstate.WAPatchName, cause error) bool {
	h.mu.Lock()
	if last, ok := h.last[name]; ok && h.now().Sub(last) < fullSyncEvery {
		h.mu.Unlock()
		if h.now().Sub(last) < 2*time.Minute {
			return false // the same failure, reported twice: already healing
		}
		logf("app state %s: sync failed (%v); last full sync %s ago — next retry within the hour",
			name, cause, h.now().Sub(last).Round(time.Second))
		return false
	}
	h.last[name] = h.now()
	h.mu.Unlock()

	logf("app state %s: sync failed (%v) — downloading it whole", name, cause)
	if err := h.fetch(ctx, name, true); err != nil {
		logf("app state %s: full sync FAILED: %v", name, err)
	} else {
		logf("app state %s: full sync done", name)
		if h.afterFull != nil {
			h.afterFull(ctx, name)
		}
	}
	return true
}
