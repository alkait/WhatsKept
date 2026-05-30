package app

import (
	"bufio"
	"context"
	"encoding/json"
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

	"github.com/creack/pty"
	"github.com/google/uuid"
)

// jobEvent is the JSON shape pushed to SSE subscribers. Field naming
// matches what the React frontend already expects (carried over from
// the Python implementation).
//
// Event types:
//
//	"line"     human-readable log line (Data)
//	"progress" structured progress update (Data = JSON payload, shape
//	           defined per-task by the emitter; the UI dispatches on
//	           the job's task field to know how to render it)
//	"adopted"  backup process adopted from a previous app lifetime
//	"ping"     idle heartbeat, ~5 s cadence
//	"done"     terminal — Status + Code + optional Error
type jobEvent struct {
	Type   string `json:"type"`
	Data   string `json:"data,omitempty"`   // "line": text; "progress": JSON payload
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

	cancel context.CancelFunc // non-nil for cancellable in-process jobs

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
	return m.startInProcessProgress(task, func(log func(string), _ func(any)) error {
		return work(log)
	})
}

// startInProcessProgress is the variant used by long-running jobs
// (notably media-index) that have something more than a rolling log
// to show. The work function gets a second callback `progress(any)`
// which JSON-encodes its argument and emits it as a "progress"
// jobEvent. The frontend dispatches on the job's `task` field to
// know how to deserialise the payload.
//
// Existing callers can keep using startInProcess unchanged — it's a
// thin wrapper around this one that discards the progress channel.
func (m *jobManager) startInProcessProgress(
	task string,
	work func(log func(string), progress func(any)) error,
) string {
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
		progress := func(payload any) {
			b, err := json.Marshal(payload)
			if err != nil {
				// Marshal failures are silent — losing one
				// progress tick is fine, the next will come.
				return
			}
			j.emit(jobEvent{Type: "progress", Data: string(b)})
		}
		err := work(log, progress)
		if err != nil {
			j.emit(jobEvent{Type: "done", Status: "error", Error: err.Error()})
			return
		}
		j.emit(jobEvent{Type: "done", Status: "ok", Code: 0})
	}()

	return j.id
}

// startInProcessProgressCtx is the cancellable variant of
// startInProcessProgress. The work function receives a context.Context
// that is cancelled when DELETE /api/jobs/{id} → jobManager.cancelJob
// is called, so a long run (notably the cloud describer) can be stopped
// from the UI without quitting the app. Committed work is preserved.
func (m *jobManager) startInProcessProgressCtx(
	task string,
	work func(ctx context.Context, log func(string), progress func(any)) error,
) string {
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{
		id:        uuid.NewString(),
		task:      task,
		startedAt: time.Now(),
		cancel:    cancel,
	}
	m.put(j)

	go func() {
		defer cancel() // release the context when the goroutine exits
		log := func(s string) { j.emit(jobEvent{Type: "line", Data: s}) }
		progress := func(payload any) {
			b, err := json.Marshal(payload)
			if err != nil {
				return
			}
			j.emit(jobEvent{Type: "progress", Data: string(b)})
		}
		err := work(ctx, log, progress)
		if err != nil {
			j.emit(jobEvent{Type: "done", Status: "error", Error: err.Error()})
			return
		}
		j.emit(jobEvent{Type: "done", Status: "ok", Code: 0})
	}()

	return j.id
}

// cancelJob cancels a running cancellable in-process job. Returns false
// if the job is unknown or isn't cancellable (e.g. a subprocess job).
func (m *jobManager) cancelJob(id string) bool {
	j, ok := m.get(id)
	if !ok || j.cancel == nil {
		return false
	}
	j.cancel()
	return true
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

	// Run idevicebackup2 under a PTY rather than wiring its stdout to
	// a raw pipe. The reason is a libc stdio buffering trap: when a
	// child's stdout is a non-TTY (a pipe is non-TTY), libc switches
	// stdout to *block*-buffered (4 KiB). For an incremental backup
	// where only a few hundred bytes of newline-terminated status
	// flow in the initial phase before the receive phase falls into
	// pure '\r' progress redraws, those initial lines stay stuck in
	// the child's stdio buffer for the entire run — the UI then sees
	// zero line events and the "show log" toggle never appears until
	// the process exits and fflushes everything in one burst.
	//
	// Giving the child a controlling terminal keeps stdout *line*-
	// buffered, so each '\n' flushes immediately and pumpLines emits
	// one event per line in real time. stderr is automatically merged
	// onto the same PTY by pty.Start, replacing the old manual
	// `cmd.Stderr = cmd.Stdout` plumbing.
	ptmx, err := pty.Start(cmd)
	if err != nil {
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

	go m.pumpProcess(j, cmd, ptmx, cancel, onDone)
	return j.id, nil
}

// helperCommandFunc abstracts helpers.Command so the job manager can
// be tested without dragging in the helpers package's filesystem
// side-effects.
type helperCommandFunc func(ctx context.Context, tool string, args ...string) (*exec.Cmd, error)

// pumpLines reads `output` and emits one "line" jobEvent per newline-
// terminated line (and one final event at EOF for any trailing data).
//
// idevicebackup2 redraws its per-file progress bar in place using
// carriage returns ('\r') rather than newlines. We *must* keep
// reading those bytes (otherwise the child's stdout pipe fills up
// and the backup deadlocks), but we *must not* surface each redraw
// as its own log line — at thousands per second they would flood
// the SSE history and freeze the React UI (the "show log" toggle
// stops responding to clicks). So we consume '\r'-terminated
// segments without emitting, keeping the same observable line
// stream the original ScanLines-based implementation produced
// while also fixing the original "bufio.Scanner: token too long"
// stall caused by an unbounded buffered token.
//
// `maxLine` caps the accumulator so a pathological upstream emitting
// neither '\n' nor '\r' can't OOM us; once full, excess bytes are
// dropped until the next terminator.
func pumpLines(output io.Reader, maxLine int, emit func(string)) error {
	br := bufio.NewReaderSize(output, 64*1024)
	buf := make([]byte, 0, 4096)
	flushLine := func() {
		// Strip a trailing '\r' so a CRLF stream doesn't leave one
		// dangling on each emitted line.
		s := buf
		if len(s) > 0 && s[len(s)-1] == '\r' {
			s = s[:len(s)-1]
		}
		emit(string(s))
		buf = buf[:0]
	}
	for {
		b, err := br.ReadByte()
		if err != nil {
			if len(buf) > 0 {
				flushLine()
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch b {
		case '\n':
			flushLine()
		case '\r':
			// Distinguish CRLF (a line terminator) from a bare
			// '\r' (a progress redraw). Peek one byte ahead: if
			// the next byte is '\n', the pair is one terminator
			// and we emit the accumulated buffer once; otherwise
			// '\r' is a redraw separator and we drop the buffer.
			next, perr := br.Peek(1)
			if perr == nil && len(next) > 0 && next[0] == '\n' {
				_, _ = br.ReadByte() // consume the '\n'
				flushLine()
			} else {
				buf = buf[:0]
			}
		default:
			if len(buf) < maxLine {
				buf = append(buf, b)
			}
			// else: silently truncate. We still consume the byte
			// so the pipe drains.
		}
	}
}

// pumpProcess scans the merged stdout/stderr pipe line by line,
// emitting "line" events, then waits for the process and emits "done".
// onDone, if non-nil, is called with ok=true iff the process exited
// cleanly (exit code 0). It runs synchronously before the "done"
// event so subscribers observe a consistent post-exit world.
func (m *jobManager) pumpProcess(j *job, cmd *exec.Cmd, output io.ReadCloser, cancel context.CancelFunc, onDone func(ok bool)) {
	defer cancel()
	defer output.Close()

	if err := pumpLines(output, 1024*1024, func(line string) {
		j.emit(jobEvent{Type: "line", Data: line})
	}); err != nil && !errors.Is(err, syscall.EIO) {
		// syscall.EIO from a PTY master read is the normal "slave end
		// closed because child exited" signal on macOS — it is not a
		// real error, so we filter it out to avoid emitting a noisy
		// "[read error]" line on every successful backup.
		j.emit(jobEvent{Type: "line", Data: "[read error] " + err.Error()})
		// Defensive: ensure the stream is fully drained so the child
		// can't block on a full stdout buffer while we Wait() below.
		_, _ = io.Copy(io.Discard, output)
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
