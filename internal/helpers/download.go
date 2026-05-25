package helpers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// DownloadProgress is delivered to the caller's callback at most
// once per (config-default) 250 ms tick of the download. Fields are
// snapshot values, not deltas.
type DownloadProgress struct {
	BytesRead  int64   `json:"bytes_read"`  // total bytes the local file currently has
	BytesTotal int64   `json:"bytes_total"` // expected final size (== ModelSpec.Bytes)
	RateBPS    float64 `json:"rate_bps"`    // moving rate in bytes/sec
	ETASeconds float64 `json:"eta_seconds"` // best-effort guess (0 if rate unknown)
	Stage      string  `json:"stage"`       // "downloading" | "verifying" | "done"
}

// DownloadOptions tunes the downloader. Zero values are sensible.
type DownloadOptions struct {
	// Ctx allows mid-download cancellation. Defaults to
	// context.Background.
	Ctx context.Context

	// Progress callback. Called at most every TickInterval (default
	// 250 ms) plus once at the very end with Stage="done". Safe to
	// pass nil to opt out of streaming progress.
	Progress func(DownloadProgress)

	// TickInterval throttles progress callbacks to this minimum
	// spacing. Default 250 ms.
	TickInterval time.Duration

	// Client overrides the HTTP client. Default is http.DefaultClient
	// with no timeout (we let ctx own cancellation).
	Client *http.Client
}

// DownloadModel fetches a model spec from spec.URL into ModelPath(spec),
// resuming any partial .part file via HTTP Range requests, then
// verifying against spec.SHA256. On verification failure the file
// is deleted so the next call starts clean.
//
// Atomicity: bytes go to ModelPath(spec)+".part" until verification
// passes, then a single os.Rename promotes it to the final path.
// A crashed mid-download leaves only the .part file behind, which
// the next call resumes from byte spec.SHA256.
//
// Concurrency: callers must serialise. We don't take a global lock
// here because the natural call site (the server's download SSE
// handler) already serialises via the jobManager.
func DownloadModel(spec ModelSpec, opts DownloadOptions) error {
	ctx := opts.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	tick := opts.TickInterval
	if tick <= 0 {
		tick = 250 * time.Millisecond
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}

	final, err := ModelPath(spec)
	if err != nil {
		return err
	}
	part := final + ".part"

	// If the final file already exists with the right size, fast-path
	// (callers that wanted a sha-verify could have done CheckModel
	// themselves before getting here; we leave that decision to them).
	if st, err := os.Stat(final); err == nil && st.Size() == spec.Bytes {
		emitProgress(opts.Progress, DownloadProgress{
			BytesRead:  st.Size(),
			BytesTotal: spec.Bytes,
			Stage:      "done",
		})
		return nil
	}

	// Resume from .part if present.
	var startAt int64
	if st, err := os.Stat(part); err == nil {
		if st.Size() > spec.Bytes {
			// .part is bigger than the spec — corrupt or wrong file.
			// Delete and start over.
			_ = os.Remove(part)
		} else {
			startAt = st.Size()
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.URL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if startAt > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startAt))
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", spec.URL, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// Server ignored Range or this is a fresh download.
		// If we asked for a range and got a 200, the server is
		// re-sending the whole file from byte 0, so we must
		// truncate the .part file.
		if startAt > 0 {
			startAt = 0
		}
	case http.StatusPartialContent:
		// Server honoured the Range request — resuming.
	default:
		return fmt.Errorf("unexpected HTTP status %s for %s", resp.Status, spec.URL)
	}

	// Open .part for write at the resume offset.
	flag := os.O_WRONLY | os.O_CREATE
	if startAt == 0 {
		flag |= os.O_TRUNC
	}
	out, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", part, err)
	}
	if startAt > 0 {
		if _, err := out.Seek(startAt, io.SeekStart); err != nil {
			out.Close()
			return fmt.Errorf("seek %s: %w", part, err)
		}
	}

	// Stream the body to disk while incrementally hashing.
	// We hash whatever bytes we receive in this call (resumed or
	// full); the final whole-file hash is computed separately
	// after the rename, since a resumed download didn't see the
	// pre-resume bytes.
	pr := &progressReader{
		r:        resp.Body,
		total:    spec.Bytes,
		read:     startAt,
		tick:     tick,
		callback: opts.Progress,
	}
	pr.lastTick = time.Now()
	pr.startTime = pr.lastTick

	if _, err := io.Copy(out, pr); err != nil {
		out.Close()
		// Don't delete .part on transient errors — keeping it
		// lets the next call resume.
		return fmt.Errorf("download: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", part, err)
	}

	// Now verify the full file's sha256.
	emitProgress(opts.Progress, DownloadProgress{
		BytesRead:  spec.Bytes,
		BytesTotal: spec.Bytes,
		Stage:      "verifying",
	})
	got, err := fileSHA256(part)
	if err != nil {
		return fmt.Errorf("sha256 %s: %w", part, err)
	}
	if got != spec.SHA256 {
		_ = os.Remove(part) // bad bytes — start clean next time
		return fmt.Errorf("sha256 mismatch: got %s, want %s (deleted partial)", got, spec.SHA256)
	}

	if err := os.Rename(part, final); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", part, final, err)
	}

	emitProgress(opts.Progress, DownloadProgress{
		BytesRead:  spec.Bytes,
		BytesTotal: spec.Bytes,
		Stage:      "done",
	})
	return nil
}

// progressReader wraps an io.Reader, tracking bytes-read and
// throttling the user's progress callback to one call per tick.
type progressReader struct {
	r         io.Reader
	total     int64
	read      int64 // includes any pre-resume offset (== current file size)
	tick      time.Duration
	callback  func(DownloadProgress)
	lastTick  time.Time
	startTime time.Time
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	pr.read += int64(n)
	if pr.callback != nil && time.Since(pr.lastTick) >= pr.tick {
		pr.lastTick = time.Now()
		elapsed := pr.lastTick.Sub(pr.startTime).Seconds()
		var rate, eta float64
		if elapsed > 0 {
			// Rate is total-bytes / elapsed; close enough for
			// ETA display once the download has been running a
			// couple of seconds.
			rate = float64(pr.read) / elapsed
			if rate > 0 && pr.total > pr.read {
				eta = float64(pr.total-pr.read) / rate
			}
		}
		pr.callback(DownloadProgress{
			BytesRead:  pr.read,
			BytesTotal: pr.total,
			RateBPS:    rate,
			ETASeconds: eta,
			Stage:      "downloading",
		})
	}
	return n, err
}

// emitProgress is a nil-safe wrapper.
func emitProgress(cb func(DownloadProgress), p DownloadProgress) {
	if cb != nil {
		cb(p)
	}
}

// VerifyModel computes the file's sha256 and compares against the
// spec. Equivalent to CheckModel(spec, true) but returns a plain
// error so callers don't need to interpret the status enum.
func VerifyModel(spec ModelSpec) error {
	path, err := ModelPath(spec)
	if err != nil {
		return err
	}
	got, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if got != spec.SHA256 {
		return fmt.Errorf("sha256 mismatch on %s: got %s, want %s", path, got, spec.SHA256)
	}
	return nil
}
