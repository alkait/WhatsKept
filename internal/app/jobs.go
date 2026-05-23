package app

import "sync"

// jobManager tracks long-running subprocesses (idevicebackup2, whatskept
// --update). For the Phase A iteration it is a stub that always reports
// "no active job" — the real implementation arrives in Phase D when SSE
// streaming + orphan adoption land.
type jobManager struct {
	mu sync.RWMutex
}

func newJobManager() *jobManager { return &jobManager{} }

// activeJob returns the most recent running job, or nil if none.
//
// Phase A: always nil. Phase D will scan the in-memory store.
func (j *jobManager) activeJob() *activeJobInfo { return nil }

// activeJobInfo mirrors the Python `ActiveJobInfo` model. Kept here so
// the JSON shape stays stable as we evolve the implementation.
type activeJobInfo struct {
	JobID     string `json:"job_id"`
	UDID      string `json:"udid,omitempty"`
	Task      string `json:"task,omitempty"`
	StartedAt string `json:"started_at"`
	Adopted   bool   `json:"adopted"`
	PID       int    `json:"pid,omitempty"`
}
