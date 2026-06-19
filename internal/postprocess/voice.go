package postprocess

// Voice notes, in two phases mirroring the image pipeline (media.go):
//
//   1. DownloadVoice — decrypt every WhatsApp .opus voice note from the
//      iOS backup into <workspace>/voice/<rowid>.opus. The only step that
//      needs the backup password; run as part of SyncMessages.
//   2. VoiceIndex — transcribe every 'downloaded' clip via the cloud
//      (an OpenRouter audio model), persisting transcripts in
//      wa_voice_text and flipping the row to 'transcribed'. No password —
//      a pure consumer of voice/. Needs an OpenRouter API key.
//
// Both are resumable per-row. WhatsApp voice notes are Ogg/Opus and are
// sent to the model verbatim (no local transcoding).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"whatskept/internal/backup"
)

const (
	// Status values stored in voice_index.status.
	VoiceStatusTranscribed = "transcribed"
	VoiceStatusMissing     = "missing"
	VoiceStatusError       = "error"

	// VoiceStatusDownloaded means the .opus is on disk at
	// <Workspace>/voice/<rowid>.opus but not yet transcribed — the resting
	// state between the (password-gated) download and the cloud transcribe.
	// A per-clip transcribe failure keeps this status and records the
	// reason in transcribe_error, so the download step never re-downloads
	// a file that's already on disk.
	VoiceStatusDownloaded = "downloaded"

	// Manifest-path prefix for WhatsApp media (same as image side).
	voiceManifestPrefix = "Message/"

	// OPUS magic at the start of an Ogg-encapsulated stream.
	oggMagic = "OggS"
)

// VoiceIndexOptions configures a voice download or transcribe run. Mirrors
// MediaIndexOptions; see media.go for shared field semantics.
type VoiceIndexOptions struct {
	Workspace string

	// Download-phase fields (decrypt from the encrypted backup).
	BackupPath string
	BackupRoot string
	Password   string

	// Transcribe-phase fields (cloud). Engine must be empty or SourceCloud.
	Engine string
	APIKey string
	Model  string

	// Limit caps rows attempted this run (0 = unlimited).
	Limit int

	// RetryMissing / RetryErrors re-attempt terminal rows.
	RetryMissing bool
	RetryErrors  bool

	// Concurrency caps in-flight transcribe calls (0 = sensible default).
	Concurrency int

	// Force re-transcribes every on-disk clip, overwriting existing rows.
	Force bool

	Ctx           context.Context
	Log           func(string)
	Progress      func(VoiceIndexProgress)
	ProgressEvery int
}

// VoiceIndexProgress is what callers see during a run.
type VoiceIndexProgress struct {
	Done         int     `json:"done"`
	Total        int     `json:"total"`
	Pending      int     `json:"pending"`
	Baseline     int     `json:"baseline"`
	Downloaded   int     `json:"downloaded"`
	Transcribed  int     `json:"transcribed"`
	Missing      int     `json:"missing"`
	Errors       int     `json:"errors"`
	WithText     int     `json:"with_text"`
	CurrentLabel string  `json:"current_label,omitempty"`
	RatePerSec   float64 `json:"rate_per_sec"`
	ETASeconds   float64 `json:"eta_seconds"`
	ElapsedSec   float64 `json:"elapsed_sec"`
	CostUSD      float64 `json:"cost_usd"`
}

// VoiceIndexResult is the final stats summary.
type VoiceIndexResult struct {
	BackupPath         string  `json:"backup_path"`
	Workspace          string  `json:"workspace"`
	TotalCandidates    int     `json:"total_candidates"`
	AlreadyTranscribed int     `json:"already_transcribed"`
	AlreadyDownloaded  int     `json:"already_downloaded"`
	Processed          int     `json:"processed"`
	Downloaded         int     `json:"downloaded"`
	Transcribed        int     `json:"transcribed"`
	Missing            int     `json:"missing"`
	Errors             int     `json:"errors"`
	WithText           int     `json:"with_text"`
	DurationSec        float64 `json:"duration_sec"`
	AudioSecondsTotal  float64 `json:"audio_seconds_total"`
	FTSCountAfter      int     `json:"fts_count_after"`
	CostUSD            float64 `json:"cost_usd,omitempty"`
	Cancelled          bool    `json:"cancelled,omitempty"`
}

// voiceCandidate is one voice-note row.
type voiceCandidate struct {
	rowid        int64
	manifestPath string
	durationSec  float64
}

func (c voiceCandidate) label() string {
	if c.rowid == 0 {
		return ""
	}
	if c.durationSec > 0 {
		return fmt.Sprintf("rowid=%d dur=%.0fs", c.rowid, c.durationSec)
	}
	return fmt.Sprintf("rowid=%d", c.rowid)
}

// voiceOneResult is downloadOneVoice/transcribeOneVoice's outcome.
type voiceOneResult struct {
	status   string
	withText bool
	audioSec float64
	fatal    error
}

// pickVoiceIndexBackup mirrors pickMediaIndexBackup.
func pickVoiceIndexBackup(opts VoiceIndexOptions) (backup.Info, error) {
	return pickMediaIndexBackup(MediaIndexOptions{BackupPath: opts.BackupPath, BackupRoot: opts.BackupRoot})
}

// countVoiceCandidates returns (total_in_db, already_transcribed).
func countVoiceCandidates(db *sql.DB) (total, already int, err error) {
	if err = db.QueryRow(
		`SELECT COUNT(*) FROM ZWAMEDIAITEM m
		 JOIN ZWAMESSAGE wm ON wm.Z_PK = m.ZMESSAGE
		 WHERE m.ZMEDIALOCALPATH LIKE '%.opus'`,
	).Scan(&total); err != nil {
		return 0, 0, fmt.Errorf("count total: %w", err)
	}
	if err = db.QueryRow(
		`SELECT COUNT(*) FROM voice_index WHERE status = ?`, VoiceStatusTranscribed,
	).Scan(&already); err != nil {
		return 0, 0, fmt.Errorf("count transcribed: %w", err)
	}
	return total, already, nil
}

// countVoiceDownloaded returns how many .opus files are on disk — rows
// 'downloaded' (awaiting transcribe) or 'transcribed' (file still present).
func countVoiceDownloaded(db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM voice_index WHERE status IN (?, ?)`,
		VoiceStatusDownloaded, VoiceStatusTranscribed,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count downloaded: %w", err)
	}
	return n, nil
}

// CountVoiceTranscribePending returns how many on-disk clips a normal
// transcribe run (no force, no retry) would actually queue: fresh
// 'downloaded' rows with no prior transcribe_error. It mirrors
// selectVoiceTranscribeCandidates' non-force predicate EXACTLY so the UI
// can gate "Resume" on real work — unlike downloaded−transcribed, which
// also counts permanently-failed rows the queue skips.
func CountVoiceTranscribePending(db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM voice_index WHERE status = ? AND transcribe_error IS NULL`,
		VoiceStatusDownloaded,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count voice transcribe pending: %w", err)
	}
	return n, nil
}

// CountVoiceTranscribeFailed returns on-disk clips that failed a previous
// transcribe attempt (transcribe_error set, row still 'downloaded'). A
// normal run skips these; only "Retry failures" (retryErrors) re-attempts.
func CountVoiceTranscribeFailed(db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM voice_index WHERE status = ? AND transcribe_error IS NOT NULL`,
		VoiceStatusDownloaded,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count voice transcribe failed: %w", err)
	}
	return n, nil
}

// selectVoiceDownloadCandidates: every .opus whose file is NOT already on
// disk. 'downloaded'/'transcribed' skipped; 'missing'/'error' skipped
// unless the matching retry flag is set.
func selectVoiceDownloadCandidates(db *sql.DB, retryMissing, retryErrors bool, limit int) ([]voiceCandidate, error) {
	skip := []any{VoiceStatusDownloaded, VoiceStatusTranscribed}
	if !retryMissing {
		skip = append(skip, VoiceStatusMissing)
	}
	if !retryErrors {
		skip = append(skip, VoiceStatusError)
	}
	ph := placeholders(len(skip))
	where := fmt.Sprintf(
		"m.ZMEDIALOCALPATH LIKE '%%.opus'"+
			"\n\t\t  AND m.ZMESSAGE NOT IN (SELECT rowid FROM voice_index WHERE status IN (%s))", ph)
	return queryVoiceCandidates(db, where, skip, limit)
}

// selectVoiceTranscribeCandidates: 'downloaded' rows not yet transcribed
// (transcribe_error skipped unless retryErrors). With force, every on-disk
// clip ('downloaded' or 'transcribed') is re-transcribed.
func selectVoiceTranscribeCandidates(db *sql.DB, retryErrors, force bool, limit int) ([]voiceCandidate, error) {
	var sub string
	if force {
		sub = "SELECT rowid FROM voice_index WHERE status IN ('" +
			VoiceStatusDownloaded + "','" + VoiceStatusTranscribed + "')"
	} else {
		ready := "status = '" + VoiceStatusDownloaded + "'"
		if !retryErrors {
			ready += " AND transcribe_error IS NULL"
		}
		sub = "SELECT rowid FROM voice_index WHERE " + ready
	}
	where := "m.ZMEDIALOCALPATH LIKE '%.opus'\n\t\t  AND m.ZMESSAGE IN (" + sub + ")"
	return queryVoiceCandidates(db, where, nil, limit)
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, n*2-1)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}

func queryVoiceCandidates(db *sql.DB, whereClause string, args []any, limit int) ([]voiceCandidate, error) {
	q := fmt.Sprintf(`
		SELECT m.ZMESSAGE, '%s' || m.ZMEDIALOCALPATH, COALESCE(m.ZMOVIEDURATION, 0)
		FROM   ZWAMEDIAITEM m
		JOIN   ZWAMESSAGE   wm ON wm.Z_PK = m.ZMESSAGE
		WHERE  %s
		ORDER BY m.ZMESSAGE ASC`, voiceManifestPrefix, whereClause)
	if limit > 0 {
		q += fmt.Sprintf("\n\t\tLIMIT %d", limit)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("select voice candidates: %w", err)
	}
	defer rows.Close()
	var out []voiceCandidate
	for rows.Next() {
		var c voiceCandidate
		if err := rows.Scan(&c.rowid, &c.manifestPath, &c.durationSec); err != nil {
			return nil, fmt.Errorf("scan voice candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- Download phase --------------------------------------------------

// DownloadVoice decrypts every WhatsApp voice note into <ws>/voice/.
func DownloadVoice(opts VoiceIndexOptions) (*VoiceIndexResult, error) {
	if opts.Workspace == "" {
		return nil, errors.New("voice-download: Workspace required")
	}
	if opts.Password == "" {
		return nil, errors.New("voice-download: Password required for encrypted backups")
	}
	defOpts(&opts, 25)

	db, voiceDir, err := openVoiceDB(opts.Workspace)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	info, err := pickVoiceIndexBackup(opts)
	if err != nil {
		return nil, err
	}
	if !info.IsEncrypted {
		return nil, errors.New("voice-download: backup is not encrypted")
	}
	opts.Log(fmt.Sprintf("Backup: %s", info.Path))
	opts.Log("Unlocking iOS backup…")
	bundle, err := backup.Open(info, opts.Password)
	if err != nil {
		return nil, fmt.Errorf("open backup: %w", err)
	}
	res := &VoiceIndexResult{BackupPath: info.Path, Workspace: opts.Workspace}
	if err := downloadVoiceWithBundle(opts, db, bundle, voiceDir, res); err != nil {
		return nil, err
	}
	return res, nil
}

// downloadVoiceDuringSync pulls voice notes as a step of SyncMessages,
// reusing the already-open backup bundle. Fail-soft.
func downloadVoiceDuringSync(bundle *backup.Bundle, workspace, dbPath string, log func(string)) (*VoiceIndexResult, error) {
	voiceDir := filepath.Join(workspace, "voice")
	if err := os.MkdirAll(voiceDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir voice dir: %w", err)
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	if err := ensureVoiceSidecarSchema(db); err != nil {
		return nil, err
	}
	opts := VoiceIndexOptions{
		Workspace:     workspace,
		Ctx:           context.Background(),
		Log:           log,
		ProgressEvery: 500,
		Progress: func(p VoiceIndexProgress) {
			log(fmt.Sprintf("Downloading voice notes… %d / %d", p.Done, p.Pending))
		},
	}
	res := &VoiceIndexResult{Workspace: workspace}
	if err := downloadVoiceWithBundle(opts, db, bundle, voiceDir, res); err != nil {
		return nil, err
	}
	return res, nil
}

func downloadVoiceWithBundle(opts VoiceIndexOptions, db *sql.DB, bundle *backup.Bundle, voiceDir string, res *VoiceIndexResult) error {
	manifestIdx := buildManifestIndex(bundle, backup.WhatsAppDomain)
	total, _, err := countVoiceCandidates(db)
	if err != nil {
		return err
	}
	alreadyDownloaded, err := countVoiceDownloaded(db)
	if err != nil {
		return err
	}
	candidates, err := selectVoiceDownloadCandidates(db, opts.RetryMissing, opts.RetryErrors, opts.Limit)
	if err != nil {
		return err
	}
	res.TotalCandidates = total
	res.AlreadyDownloaded = alreadyDownloaded
	if len(candidates) == 0 {
		opts.Log(fmt.Sprintf("Voice notes: %d / %d already on disk; nothing to download.", alreadyDownloaded, total))
		return nil
	}
	opts.Log(fmt.Sprintf("Voice notes on device: %d · on disk: %d · to download: %d",
		total, alreadyDownloaded, len(candidates)))

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = defaultDownloadConcurrency
	}
	var decryptMu sync.Mutex
	process := func(ctx context.Context, c voiceCandidate) voiceOneResult {
		return downloadOneVoice(ctx, db, bundle, &decryptMu, manifestIdx, voiceDir, c, opts.Log)
	}
	_ = runVoicePipeline(opts, res, db, candidates, total, alreadyDownloaded, concurrency, nil, false, process)
	opts.Log(fmt.Sprintf("Downloaded %d voice notes (%d missing, %d errors) in %.0fs.",
		res.Downloaded, res.Missing, res.Errors, res.DurationSec))
	return nil
}

// downloadOneVoice decrypts one clip, sanity-checks Ogg magic, writes it
// atomically, and records the outcome in voice_index. Safe for the pool:
// decrypt is serialised behind decryptMu.
func downloadOneVoice(
	ctx context.Context,
	db *sql.DB,
	bundle *backup.Bundle,
	decryptMu *sync.Mutex,
	manifestIdx map[string]*backup.Record,
	voiceDir string,
	c voiceCandidate,
	log func(string),
) voiceOneResult {
	now := nowUTC()
	if ctx.Err() != nil {
		return voiceOneResult{status: statusCancelled}
	}

	rec, ok := manifestIdx[c.manifestPath]
	if !ok {
		writeVoiceIndex(db, c, VoiceStatusMissing, 0, "", now)
		return voiceOneResult{status: VoiceStatusMissing}
	}

	decryptMu.Lock()
	rd, err := bundle.FileReader(*rec)
	if err != nil {
		decryptMu.Unlock()
		if errors.Is(err, io.EOF) {
			writeVoiceIndex(db, c, VoiceStatusMissing, 0, "", now)
			return voiceOneResult{status: VoiceStatusMissing}
		}
		writeVoiceIndex(db, c, VoiceStatusError, 0, fmt.Sprintf("decrypt: %v", err), now)
		return voiceOneResult{status: VoiceStatusError}
	}
	data, err := io.ReadAll(rd)
	_ = rd.Close()
	decryptMu.Unlock()
	if err != nil {
		if errors.Is(err, io.EOF) {
			writeVoiceIndex(db, c, VoiceStatusMissing, 0, "", now)
			return voiceOneResult{status: VoiceStatusMissing}
		}
		writeVoiceIndex(db, c, VoiceStatusError, int64(len(data)), fmt.Sprintf("read: %v", err), now)
		return voiceOneResult{status: VoiceStatusError}
	}
	if len(data) < len(oggMagic) || string(data[:len(oggMagic)]) != oggMagic {
		writeVoiceIndex(db, c, VoiceStatusError, int64(len(data)), "not an Ogg/OPUS file", now)
		return voiceOneResult{status: VoiceStatusError}
	}

	opusPath := filepath.Join(voiceDir, fmt.Sprintf("%d.opus", c.rowid))
	if err := writeFileAtomic(opusPath, data); err != nil {
		writeVoiceIndex(db, c, VoiceStatusError, int64(len(data)), fmt.Sprintf("write opus: %v", err), now)
		return voiceOneResult{status: VoiceStatusError}
	}
	writeVoiceIndex(db, c, VoiceStatusDownloaded, int64(len(data)), "", now)
	return voiceOneResult{status: VoiceStatusDownloaded}
}

// ---- Transcribe phase ------------------------------------------------

// VoiceIndex transcribes already-downloaded voice notes via the cloud.
// No backup password — it reads voice/ and calls an OpenRouter audio
// model. Needs an API key. Resumable per-row.
func VoiceIndex(opts VoiceIndexOptions) (*VoiceIndexResult, error) {
	if opts.Workspace == "" {
		return nil, errors.New("voice-index: Workspace required")
	}
	defOpts(&opts, 1)

	db, voiceDir, err := openVoiceDB(opts.Workspace)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// The .opus files live on the filesystem, but the voice_index rows that
	// make them visible to the transcribe queue live inside ChatStorage.sqlite
	// — which a messages re-sync rebuilds from scratch. If the carry-forward
	// drops those rows, the clips are orphaned (on disk, invisible to the
	// queue) and a run no-ops with "Nothing to transcribe". Re-adopt them so
	// the filesystem, not the index, is the source of truth for "downloaded".
	if n, rErr := reconcileVoiceIndexFromDisk(db, voiceDir); rErr != nil {
		return nil, rErr
	} else if n > 0 {
		opts.Log(fmt.Sprintf("Re-adopted %d on-disk voice note(s) missing from the index (recovered after a re-sync).", n))
	}

	total, alreadyTranscribed, err := countVoiceCandidates(db)
	if err != nil {
		return nil, err
	}
	downloaded, err := countVoiceDownloaded(db)
	if err != nil {
		return nil, err
	}
	candidates, err := selectVoiceTranscribeCandidates(db, opts.RetryErrors, opts.Force, opts.Limit)
	if err != nil {
		return nil, err
	}
	res := &VoiceIndexResult{Workspace: opts.Workspace, TotalCandidates: total, AlreadyTranscribed: alreadyTranscribed}
	if len(candidates) == 0 {
		opts.Log(fmt.Sprintf("Nothing to transcribe: %d / %d downloaded voice notes already transcribed.", alreadyTranscribed, downloaded))
		ftsN, _ := rebuildFTS(db)
		res.FTSCountAfter = ftsN
		return res, nil
	}
	opts.Log(fmt.Sprintf("Voice notes downloaded: %d", downloaded))
	opts.Log(fmt.Sprintf("Already transcribed:    %d", alreadyTranscribed))
	opts.Log(fmt.Sprintf("Queued this run:        %d", len(candidates)))

	transcriber, err := voiceTranscribers.build(opts.Engine, opts.APIKey, opts.Model)
	if err != nil {
		return nil, err
	}
	concurrency := resolveConcurrency(opts.Concurrency)
	opts.Log(fmt.Sprintf("Transcriber: cloud (%s) (concurrency %d)", transcriber.Model(), concurrency))

	baseline := alreadyTranscribed
	process := func(ctx context.Context, c voiceCandidate) voiceOneResult {
		return transcribeOneVoice(ctx, db, transcriber, voiceDir, c, opts.Log)
	}
	fatalErr := runVoicePipeline(opts, res, db, candidates, total, baseline, concurrency,
		func() float64 { return transcriber.CostUSD() }, true, process)

	opts.Log(fmt.Sprintf("Done in %.0fs. transcribed=%d errors=%d audio=%.0fs ($%.4f)",
		res.DurationSec, res.Transcribed, res.Errors, res.AudioSecondsTotal, res.CostUSD))
	if fatalErr != nil {
		return res, fmt.Errorf("transcribe run aborted after %d clips: %w", res.Transcribed, fatalErr)
	}
	return res, nil
}

// transcribeOneVoice reads one .opus off disk and transcribes it via the
// cloud, writing wa_voice_text and flipping voice_index to 'transcribed'.
// A per-clip failure keeps status='downloaded' and records the reason in
// transcribe_error so the file isn't re-downloaded.
func transcribeOneVoice(
	ctx context.Context,
	db *sql.DB,
	transcriber *voiceTranscriber,
	voiceDir string,
	c voiceCandidate,
	log func(string),
) voiceOneResult {
	now := nowUTC()
	path := filepath.Join(voiceDir, fmt.Sprintf("%d.opus", c.rowid))
	data, err := os.ReadFile(path)
	if err != nil {
		setVoiceTranscribeError(db, c.rowid, "file missing on disk at transcribe time", now)
		return voiceOneResult{status: VoiceStatusError}
	}

	text, err := transcriber.Transcribe(ctx, data)
	if err != nil {
		var fatal *FatalError
		if errors.As(err, &fatal) {
			return voiceOneResult{status: statusCancelled, fatal: err}
		}
		if ctx.Err() != nil {
			return voiceOneResult{status: statusCancelled}
		}
		setVoiceTranscribeError(db, c.rowid, fmt.Sprintf("transcribe: %v", err), now)
		return voiceOneResult{status: VoiceStatusError}
	}

	tx, err := db.Begin()
	if err != nil {
		log(fmt.Sprintf("[rowid=%d] begin tx: %v", c.rowid, err))
		return voiceOneResult{status: VoiceStatusError}
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO wa_voice_text
		 (rowid, transcript, language, duration_sec, model, generated_at)
		 VALUES (?, ?, '', ?, ?, ?)`,
		c.rowid, text, c.durationSec, transcriber.Model(), now,
	); err != nil {
		_ = tx.Rollback()
		log(fmt.Sprintf("[rowid=%d] insert wa_voice_text: %v", c.rowid, err))
		return voiceOneResult{status: VoiceStatusError}
	}
	if _, err := tx.Exec(
		`UPDATE voice_index SET status = ?, transcribe_error = NULL, attempted_at = ? WHERE rowid = ?`,
		VoiceStatusTranscribed, now, c.rowid,
	); err != nil {
		_ = tx.Rollback()
		log(fmt.Sprintf("[rowid=%d] update voice_index: %v", c.rowid, err))
		return voiceOneResult{status: VoiceStatusError}
	}
	if err := tx.Commit(); err != nil {
		log(fmt.Sprintf("[rowid=%d] commit: %v", c.rowid, err))
		return voiceOneResult{status: VoiceStatusError}
	}
	return voiceOneResult{status: VoiceStatusTranscribed, withText: text != "", audioSec: c.durationSec}
}

// ---- Shared worker pool ----------------------------------------------

// runVoicePipeline drives the voice download/transcribe phases. Like
// runMediaPipeline, it is a thin per-phase wrapper over the generic
// runWorkerPipeline (pipeline.go), supplying the voice-specific tally,
// progress emission, and FTS rebuild.
func runVoicePipeline(
	opts VoiceIndexOptions,
	res *VoiceIndexResult,
	db *sql.DB,
	candidates []voiceCandidate,
	total, baseline, concurrency int,
	costFn func() float64,
	rebuildFTSAfter bool,
	process func(ctx context.Context, c voiceCandidate) voiceOneResult,
) error {
	progressEvery := opts.ProgressEvery
	if progressEvery <= 0 {
		progressEvery = 1
	}
	tStart := time.Now()
	cost := func() float64 {
		if costFn == nil {
			return 0
		}
		return costFn()
	}

	var rebuild func()
	if rebuildFTSAfter {
		rebuild = func() {
			opts.Log("Rebuilding messages_fts…")
			if ftsN, err := rebuildFTS(db); err != nil {
				opts.Log(fmt.Sprintf("FTS rebuild failed (non-fatal): %v", err))
			} else {
				res.FTSCountAfter = ftsN
			}
		}
	}

	return runWorkerPipeline(opts.Ctx, db, candidates, concurrency, progressEvery,
		process,
		func(r voiceOneResult) (string, error) { return r.status, r.fatal },
		nil, // voice does not log a separate "Aborting" line
		func(r voiceOneResult) {
			switch r.status {
			case VoiceStatusDownloaded:
				res.Downloaded++
			case VoiceStatusTranscribed:
				res.Transcribed++
				res.AudioSecondsTotal += r.audioSec
				if r.withText {
					res.WithText++
				}
			case VoiceStatusMissing:
				res.Missing++
			case VoiceStatusError:
				res.Errors++
			}
			res.Processed++
		},
		func() {
			res.CostUSD = cost()
			emitVoiceProgress(opts.Progress, res, total, baseline, len(candidates), tStart)
		},
		func(stoppedByUser bool) {
			res.DurationSec = time.Since(tStart).Seconds()
			if stoppedByUser {
				res.Cancelled = true
				opts.Log("Stopped on user request. All committed rows are safe; re-run to resume.")
			}
		},
		rebuild,
	)
}

// ---- helpers ---------------------------------------------------------

func defOpts(opts *VoiceIndexOptions, progressEvery int) {
	if opts.Ctx == nil {
		opts.Ctx = context.Background()
	}
	if opts.Log == nil {
		opts.Log = func(string) {}
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = progressEvery
	}
}

func openVoiceDB(workspace string) (*sql.DB, string, error) {
	voiceDir := filepath.Join(workspace, "voice")
	if err := os.MkdirAll(voiceDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("mkdir voice dir: %w", err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(workspace, "ChatStorage.sqlite"))
	if err != nil {
		return nil, "", fmt.Errorf("open db: %w", err)
	}
	if err := ensureVoiceSidecarSchema(db); err != nil {
		_ = db.Close()
		return nil, "", err
	}
	return db, voiceDir, nil
}

// reconcileVoiceIndexFromDisk adopts orphaned voice/<rowid>.opus files into
// voice_index as 'downloaded' rows. A messages re-sync rebuilds
// ChatStorage.sqlite — and with it voice_index — from the backup, but leaves
// the on-disk voice/ directory untouched. If the sidecar carry-forward fails
// to preserve voice_index (e.g. message Z_PKs were re-keyed), every clip is
// left present on disk yet missing from the index, so selectVoiceTranscribe-
// Candidates finds nothing and the run reports "Nothing to transcribe" while
// thousands of clips sit unread. This re-creates a 'downloaded' row for each
// on-disk clip that maps to a real .opus media item but has no live index
// entry. Files with no matching media item are ignored as stale. Returns the
// number of rows adopted.
func reconcileVoiceIndexFromDisk(db *sql.DB, voiceDir string) (int, error) {
	entries, err := os.ReadDir(voiceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read voice dir: %w", err)
	}

	// Rows the index already tracks — adopt only what it doesn't know about.
	known := map[int64]bool{}
	rows, err := db.Query(`SELECT rowid FROM voice_index`)
	if err != nil {
		return 0, fmt.Errorf("load voice_index rowids: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan voice_index rowid: %w", err)
		}
		known[id] = true
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate voice_index rowids: %w", err)
	}

	now := nowUTC()
	adopted := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".opus") {
			continue
		}
		rowid, err := strconv.ParseInt(strings.TrimSuffix(e.Name(), ".opus"), 10, 64)
		if err != nil || known[rowid] {
			continue
		}
		// Confirm the rowid is a real .opus voice message and pull the manifest
		// path + duration so the adopted row matches a fresh download exactly.
		var c voiceCandidate
		err = db.QueryRow(
			`SELECT m.ZMESSAGE, ? || m.ZMEDIALOCALPATH, COALESCE(m.ZMOVIEDURATION, 0)
			   FROM ZWAMEDIAITEM m
			   JOIN ZWAMESSAGE  wm ON wm.Z_PK = m.ZMESSAGE
			  WHERE m.ZMESSAGE = ? AND m.ZMEDIALOCALPATH LIKE '%.opus'`,
			voiceManifestPrefix, rowid,
		).Scan(&c.rowid, &c.manifestPath, &c.durationSec)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return adopted, fmt.Errorf("look up voice media item %d: %w", rowid, err)
		}
		var bytesLen int64
		if info, statErr := e.Info(); statErr == nil {
			bytesLen = info.Size()
		}
		writeVoiceIndex(db, c, VoiceStatusDownloaded, bytesLen, "", now)
		adopted++
	}
	return adopted, nil
}

// writeVoiceIndex commits a single voice_index row (terminal download
// outcomes). INSERT OR REPLACE resets transcribe_error to NULL.
func writeVoiceIndex(db *sql.DB, c voiceCandidate, status string, bytesLen int64, errMsg string, now string) {
	var errVal any
	if errMsg != "" {
		errVal = errMsg
	}
	_, _ = db.Exec(
		`INSERT OR REPLACE INTO voice_index
		 (rowid, manifest_path, status, bytes, duration_sec, error, transcribe_error, attempted_at)
		 VALUES (?, ?, ?, ?, ?, ?, NULL, ?)`,
		c.rowid, c.manifestPath, status, bytesLen, c.durationSec, errVal, now,
	)
}

// setVoiceTranscribeError records a per-clip transcribe failure without
// changing status (the file stays on disk, not re-downloaded).
func setVoiceTranscribeError(db *sql.DB, rowid int64, msg, now string) {
	_, _ = db.Exec(
		`UPDATE voice_index SET transcribe_error = ?, attempted_at = ? WHERE rowid = ?`,
		msg, now, rowid,
	)
}

func emitVoiceProgress(cb func(VoiceIndexProgress), res *VoiceIndexResult, total, baseline, pending int, started time.Time) {
	if cb == nil {
		return
	}
	elapsed := time.Since(started).Seconds()
	rate := float64(res.Processed) / maxF(elapsed, 0.001)
	remaining := pending - res.Processed
	eta := 0.0
	if rate > 0 && remaining > 0 {
		eta = float64(remaining) / rate
	}
	cb(VoiceIndexProgress{
		Done:        res.Processed,
		Total:       total,
		Pending:     pending,
		Baseline:    baseline,
		Downloaded:  res.Downloaded,
		Transcribed: res.Transcribed,
		Missing:     res.Missing,
		Errors:      res.Errors,
		WithText:    res.WithText,
		RatePerSec:  rate,
		ETASeconds:  eta,
		ElapsedSec:  elapsed,
		CostUSD:     res.CostUSD,
	})
}
