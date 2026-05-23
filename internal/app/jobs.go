package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// jobEvent is the JSON shape pushed to SSE subscribers. Field naming
// matches what the React frontend already expects (carried over from
// the Python implementation).
type jobEvent struct {
	Type   string `json:"type"`             // "line", "adopted", "ping", "done"
	Data   string `json:"data,omitempty"`   // line text (for "line")
	Status string `json:"status,omitempty"` // "ok" | "error" (for "done")
	Code   int    `json:"code,omitempty"`   // exit code (for "done")
	PID    int    `json:"pid,omitempty"`    // child pid (for "adopted","ping")
	Error  string `json:"error,omitempty"`  // optional error string
}

// job is one tracked subprocess (running, adopted, or already finished).
type job struct {
	id        string
	udid      string
	task      string // "" for backups, "update" for db tasks (future)
	startedAt time.Time
	pid       int
	adopted   bool

	mu          sync.Mutex
	history     []jobEvent
	finished    bool
	finalStatus string // "ok" | "error"
	subs        map[chan jobEvent]struct{}
}

func (j *job) snapshot() (history []jobEvent, finished bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]jobEvent, len(j.history))
	copy(out, j.history)
	return out, j.finished
}

// emit appends an event to the history and broadcasts it to all
// subscribers. Non-blocking: if a subscriber's channel is full, the
// event is dropped for that subscriber (better than stalling the
// pipeline that produced it).
func (j *job) emit(ev jobEvent) {
	j.mu.Lock()
	if j.finished {
		j.mu.Unlock()
		return
	}
	j.history = append(j.history, ev)
	if ev.Type == "done" {
		j.finished = true
		j.finalStatus = ev.Status
	}
	subs := make([]chan jobEvent, 0, len(j.subs))
	for c := range j.subs {
		subs = append(subs, c)
	}
	j.mu.Unlock()

	for _, c := range subs {
		select {
		case c <- ev:
		default:
		}
	}

	// On terminal event, close all subs to release SSE handlers.
	if ev.Type == "done" {
		j.mu.Lock()
		for c := range j.subs {
			close(c)
		}
		j.subs = nil
		j.mu.Unlock()
	}
}

// subscribe returns a channel that receives all future events. It is
// pre-loaded with the entire history so a slightly-late subscriber
// doesn't miss the first lines. The caller must drain the channel
// until it closes (the manager closes it on "done").
//
// If the job is already finished, the returned channel is pre-closed
// after delivering the history.
func (j *job) subscribe() chan jobEvent {
	ch := make(chan jobEvent, 256)

	j.mu.Lock()
	defer j.mu.Unlock()

	for _, ev := range j.history {
		// Buffered, won't block.
		ch <- ev
	}
	if j.finished {
		close(ch)
		return ch
	}
	if j.subs == nil {
		j.subs = make(map[chan jobEvent]struct{})
	}
	j.subs[ch] = struct{}{}
	return ch
}

// jobManager tracks long-running subprocesses (idevicebackup2, eventually
// `whatskept --update`). Mirrors the Python implementation closely
// enough that the existing SSE-driven React UI works unchanged.
type jobManager struct {
	mu    sync.RWMutex
	jobs  map[string]*job
	order []string // chronological insertion order for activeJob()
}

func newJobManager() *jobManager {
	return &jobManager{jobs: make(map[string]*job)}
}

func (m *jobManager) get(id string) (*job, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j, ok := m.jobs[id]
	return j, ok
}

func (m *jobManager) put(j *job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[j.id] = j
	m.order = append(m.order, j.id)
}

// activeJob returns the most-recently-registered job that hasn't
// finished yet, or nil if all jobs are done.
func (m *jobManager) activeJob() *activeJobInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := len(m.order) - 1; i >= 0; i-- {
		j := m.jobs[m.order[i]]
		j.mu.Lock()
		fin := j.finished
		j.mu.Unlock()
		if !fin {
			return &activeJobInfo{
				JobID:     j.id,
				UDID:      j.udid,
				Task:      j.task,
				StartedAt: j.startedAt.Format(time.RFC3339),
				Adopted:   j.adopted,
				PID:       j.pid,
			}
		}
	}
	return nil
}

// startInProcess registers a new job whose work runs as a Go function
// (no subprocess), streaming each `log` line as a "line" jobEvent.
// `work` returns nil on success or an error whose .Error() string
// is suitable for direct UI display.
//
// Used by the Database tab's Sync button — postprocess.SyncMessages
// runs entirely in-process so we don't need to re-exec ourselves
// the way the Python implementation did.
//
// The returned job ID can be subscribed to via /api/stream/{id}
// immediately; the work goroutine is launched before this returns.
func (m *jobManager) startInProcess(task string, work func(log func(string)) error) string {
	j := &job{
		id:        uuid.NewString(),
		task:      task,
		startedAt: time.Now(),
	}
	m.put(j)

	go func() {
		log := func(s string) {
			j.emit(jobEvent{Type: "line", Data: s})
		}
		err := work(log)
		if err != nil {
			j.emit(jobEvent{Type: "done", Status: "error", Error: err.Error()})
			return
		}
		j.emit(jobEvent{Type: "done", Status: "ok", Code: 0})
	}()

	return j.id
}

// startBackup spawns a backup subprocess and registers it. The caller
// gets back the new job's ID; subscribers (SSE) can connect via
// /api/stream/{id} immediately afterwards.
// onDone is invoked from pumpProcess once the subprocess has exited
// and the final status is known. It runs *before* the "done" event
// is emitted to subscribers, so any state mutation it performs (e.g.
// caching a verified backup password) is visible to anything that
// reacts to the "done" event. Pass nil if no post-exit hook is needed.
func (m *jobManager) startBackup(udid string, network bool, password, backupRoot string, helperCmd helperCommandFunc, onDone func(ok bool)) (string, error) {
	args := []string{}
	if udid != "" {
		args = append(args, "-u", udid)
	}
	if network {
		args = append(args, "-n")
	}
	args = append(args, "backup", backupRoot)

	ctx, cancel := context.WithCancel(context.Background())
	cmd, err := helperCmd(ctx, "idevicebackup2", args...)
	if err != nil {
		cancel()
		return "", fmt.Errorf("build idevicebackup2 command: %w", err)
	}

	if password != "" {
		// Inherit existing env (helpers.Command already prepended PATH)
		// and override BACKUP_PASSWORD.
		cmd.Env = append(cmd.Env, "BACKUP_PASSWORD="+password)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout // merge into one stream
	// Reassign because StderrPipe() and assigning Stderr at the same
	// time conflicts; we want the merged stream so set both to the
	// same os.File-equivalent. Redo carefully.

	if err := cmd.Start(); err != nil {
		cancel()
		return "", fmt.Errorf("start idevicebackup2: %w", err)
	}

	j := &job{
		id:        uuid.NewString(),
		udid:      udid,
		startedAt: time.Now(),
		pid:       cmd.Process.Pid,
	}
	m.put(j)

	go m.pumpProcess(j, cmd, stdout, cancel, onDone)
	return j.id, nil
}

// helperCommandFunc abstracts helpers.Command so the job manager can
// be tested without dragging in the helpers package's filesystem
// side-effects.
type helperCommandFunc func(ctx context.Context, tool string, args ...string) (*exec.Cmd, error)

// pumpProcess scans the merged stdout/stderr pipe line by line,
// emitting "line" events, then waits for the process and emits "done".
// onDone, if non-nil, is called with ok=true iff the process exited
// cleanly (exit code 0). It runs synchronously before the "done"
// event so subscribers observe a consistent post-exit world.
func (m *jobManager) pumpProcess(j *job, cmd *exec.Cmd, output io.ReadCloser, cancel context.CancelFunc, onDone func(ok bool)) {
	defer cancel()
	defer output.Close()

	sc := bufio.NewScanner(output)
	sc.Buffer(make([]byte, 0, 1024), 1024*1024) // tolerate long lines
	for sc.Scan() {
		j.emit(jobEvent{Type: "line", Data: sc.Text()})
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		j.emit(jobEvent{Type: "line", Data: "[scan error] " + err.Error()})
	}

	werr := cmd.Wait()
	code := 0
	status := "ok"
	if werr != nil {
		var exitErr *exec.ExitError
		if errors.As(werr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
		status = "error"
	}
	if onDone != nil {
		onDone(status == "ok")
	}
	j.emit(jobEvent{Type: "done", Status: status, Code: code})
}

// adoptOrphans scans ps for any idevicebackup2 process not started by
// us and registers a watcher job for each. This lets the UI re-attach
// to a backup that was running when the app last crashed/quit.
//
// Best-effort: any failure here is silent so the GUI can still launch.
func (m *jobManager) adoptOrphans() {
	out, err := exec.Command("ps", "-eo", "pid,command").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "idevicebackup2") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == os.Getpid() {
			continue
		}
		// Skip our own embedded helper if it's a child of us — we already
		// track those properly. Heuristic: skip if executable path is in
		// our cache directory. We can't tell from `ps` output cheaply;
		// instead, skip if it's a *child* of our PID.
		if isChildOfCurrentProcess(pid) {
			continue
		}

		udid := ""
		for i, f := range fields {
			if f == "-u" && i+1 < len(fields) {
				udid = fields[i+1]
				break
			}
		}

		j := &job{
			id:        uuid.NewString(),
			udid:      udid,
			startedAt: processStartTime(pid),
			pid:       pid,
			adopted:   true,
		}
		m.put(j)
		j.emit(jobEvent{Type: "adopted", PID: pid})
		go m.watchAdopted(j)
	}
}

// processStartTime returns the wall-clock time at which the given pid
// was launched, by parsing `ps -p PID -o lstart=` (e.g.
// "Sat May 23 12:30:45 2026" — matches Go's time.ANSIC layout).
//
// This is what makes the elapsed-time counter survive an app restart:
// if whatskept was killed at T+1m15s and reopens 15 s later, the GUI
// still shows T+1m30s for the adopted backup, not a fresh 0:00.
//
// Falls back to time.Now() on any failure (lookup, parse, etc.) so a
// missing column or format change doesn't break adoption.
func processStartTime(pid int) time.Time {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return time.Now()
	}
	s := strings.TrimSpace(string(out))
	t, err := time.ParseInLocation(time.ANSIC, s, time.Local)
	if err != nil {
		return time.Now()
	}
	return t
}

// watchAdopted polls an adopted PID until it exits, emitting "ping"
// events every 5 s and a final "done".
func (m *jobManager) watchAdopted(j *job) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		<-tick.C
		if !pidAlive(j.pid) {
			j.emit(jobEvent{Type: "done", Status: "ok", Code: 0})
			return
		}
		j.emit(jobEvent{Type: "ping", PID: j.pid})
	}
}

// pidAlive returns true iff the given pid maps to a live process.
func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// signal 0 doesn't deliver, just probes existence/permission.
	err = p.Signal(syscall.Signal(0))
	return err == nil
}

// isChildOfCurrentProcess returns true if pid's parent (PPID) is the
// current process. Used by the orphan-adopter so we don't double-
// register a child we just spawned ourselves.
func isChildOfCurrentProcess(pid int) bool {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return false
	}
	return ppid == os.Getpid()
}

// activeJobInfo is the JSON shape returned by /api/jobs/active.
// Mirrors the Python `ActiveJobInfo` model so the React code is
// unchanged.
type activeJobInfo struct {
	JobID     string `json:"job_id"`
	UDID      string `json:"udid,omitempty"`
	Task      string `json:"task,omitempty"`
	StartedAt string `json:"started_at"`
	Adopted   bool   `json:"adopted"`
	PID       int    `json:"pid,omitempty"`
}
