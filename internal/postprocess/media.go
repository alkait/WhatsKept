package postprocess

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"whatskept/internal/backup"
)

// This file is the Go port of the Python whatskept.media_indexer
// module. It owns two user-visible operations, split so that only the
// first needs the backup password:
//
//   1. DownloadMedia — `whatskept media-download` (CLI) / "Download
//      images" (GUI): walk every WhatsApp image message in
//      ChatStorage.sqlite, decrypt the JPEG from the iOS backup, and
//      write it to <workspace>/media/<rowid>.<ext>, marking the row
//      'downloaded'. The ONLY image step that touches the backup.
//
//   2. MediaIndex — `whatskept media-index` (CLI) / "AI image
//      descriptions" (GUI): for every 'downloaded' row, read the file
//      back off disk, run the cloud vision model to get an OCR text +
//      description, persist them in wa_image_text, and flip the row to
//      'described'. A pure consumer of media/ — no password.
//
// Both are resumable per-row.
//
// Design notes (departures from the Python original):
//
//   - `language` column added to wa_image_text (the describer returns
//     a best-effort dominant script; useful for "find Arabic-text
//     receipts").
//   - Decrypted JPEGs are kept on disk by default at
//     <workspace>/media/<rowid>.<ext> (jpg / png / heic / gif —
//     whatever the bytes actually are) so the agent can `open`
//     an image without re-decrypting (see AGENTS.md). Orphan
//     files get pruned by SyncMessages on the next sync; we do
//     not have to clean up here.

const (
	// Status values stored in media_index.status. The set is
	// closed; any other value means an older / corrupted client
	// wrote the row and we ignore it on resume.
	MediaStatusDescribed = "described"
	MediaStatusMissing   = "missing"
	MediaStatusError     = "error"

	// MediaStatusDownloaded means the decrypted image is on disk at
	// <Workspace>/media/<rowid>.<ext> but has not yet been described.
	// It is the resting state between the (password-gated) download
	// step and the cloud-describe step, which is a pure consumer of the
	// media/ folder. A per-image describe failure keeps this status and
	// records the reason in describe_error, so the download step never
	// re-downloads a file that's already on disk.
	MediaStatusDownloaded = "downloaded"

	// statusCancelled is an internal, never-persisted sentinel: an
	// in-flight row that downloadOne/describeOne abandoned because the
	// context was cancelled. The collector ignores it (the row is left
	// in its prior state, so the image is simply re-queued next run).
	statusCancelled = "cancelled"

	// defaultCloudConcurrency is the in-flight cloud request cap when
	// the caller doesn't specify one. Conservative enough to stay well
	// under typical provider rate limits while still ~Nx faster than
	// sequential.
	defaultCloudConcurrency = 8

	// defaultDownloadConcurrency is the worker count for the download
	// step. The decrypt itself is serialised (the backup reader isn't
	// concurrency-safe), so this just overlaps disk writes and DB
	// commits with the next decrypt — a modest fan-out is plenty.
	defaultDownloadConcurrency = 4

	// All WhatsApp ChatStorage references to media files are
	// relative paths under the Message/ subtree of WhatsApp's
	// app-group container. Manifest entries carry the full path
	// directly, so we prefix DB-side ZMEDIALOCALPATH with this.
	mediaManifestPrefix = "Message/"
)

// mediaImageExts lists every on-disk extension downloadOne is allowed
// to write under <Workspace>/media/. Used by the orphan-prune walk
// to know which suffixes mean "indexer output" vs "user-dropped
// files we don't touch". Keep in sync with detectImageFormat.
var mediaImageExts = []string{".jpg", ".png", ".heic", ".gif"}

// detectImageFormat sniffs the first few bytes of `data` and returns
// a bare extension ("jpg", "png", "heic", "gif") for the four
// formats we handle in WhatsApp content. ok=false means nothing
// matched — the blob is corrupt, encrypted, or some format we don't
// handle (TIFF/WebP do exist but we haven't seen them in WhatsApp
// media so far).
//
// Magic-byte references:
//   - JPEG:  FF D8 FF
//   - PNG:   89 50 4E 47 0D 0A 1A 0A
//   - HEIC:  bytes [4:8] == "ftyp" (ISO BMFF box header). Brand at
//     [8:12] varies (heic / heix / mif1 / msf1 / heim / heis);
//     we accept any "ftyp" since misclassifying e.g. an MP4
//     here is harmless — Vision rejects non-images cleanly
//     and we surface that as a normal vision error.
//   - GIF:   47 49 46 38 ('GIF8')
func detectImageFormat(data []byte) (string, bool) {
	if len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return "jpg", true
	}
	if len(data) >= 8 &&
		data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 &&
		data[4] == 0x0D && data[5] == 0x0A && data[6] == 0x1A && data[7] == 0x0A {
		return "png", true
	}
	if len(data) >= 12 &&
		data[4] == 'f' && data[5] == 't' && data[6] == 'y' && data[7] == 'p' {
		return "heic", true
	}
	if len(data) >= 4 &&
		data[0] == 'G' && data[1] == 'I' && data[2] == 'F' && data[3] == '8' {
		return "gif", true
	}
	return "", false
}

// MediaIndexOptions configures one MediaIndex run. The zero value is
// not useful — at minimum Workspace, BackupRoot and Password (or an
// explicit BackupPath) must be set.
type MediaIndexOptions struct {
	// Workspace is the directory containing ChatStorage.sqlite.
	// Decrypted images land in <Workspace>/media/<rowid>.<ext>
	// where <ext> reflects the actual format (jpg/png/heic/gif).
	// WhatsApp's ZMEDIALOCALPATH is .jpg regardless of content.
	Workspace string

	// BackupPath, when non-empty, pins the backup to a specific
	// directory. Otherwise the most-recent encrypted backup under
	// BackupRoot is used (respecting any workspace UDID binding
	// applied by the caller — we don't re-check identity here).
	BackupPath string
	BackupRoot string

	// Password to unlock the encrypted backup. Required.
	Password string

	// Limit caps the number of rows attempted in this run. 0 =
	// no cap. Useful for smoke tests; in production the user just
	// re-runs to resume.
	Limit int

	// RetryMissing re-attempts rows previously marked 'missing'
	// (file referenced in DB but absent from this backup). Useful
	// when the user has just pulled a fresh backup that has the
	// missing files.
	RetryMissing bool

	// RetryErrors re-attempts rows previously marked 'error'
	// (decrypt failed, describe failed, etc.).
	RetryErrors bool

	// Engine selects the describer. Only SourceCloud (an OpenRouter
	// vision model) is supported; an empty value defaults to it. Cloud
	// requires APIKey; Model defaults to DefaultCloudModel.
	Engine string
	APIKey string
	Model  string

	// Concurrency caps in-flight Describe calls. 0 = sensible default
	// (defaultCloudConcurrency). The decrypt step and DB writes are
	// serialised internally regardless.
	Concurrency int

	// Force re-describes every describable image, overwriting rows
	// already done (e.g. to apply an improved prompt or switch models).
	// Without it, runs skip rows already described by the cloud.
	Force bool

	// Ctx is checked between rows for graceful cancellation.
	// Cancelling closes the Swift subprocess's stdin (it exits
	// cleanly) and the current row's commit either finishes or
	// gets rolled back by the deferred Close. Either way the DB
	// is consistent.
	Ctx context.Context

	// Log receives one-line human-readable progress lines.
	Log func(string)

	// Progress receives a structured update every ProgressEvery
	// rows. ProgressEvery=0 → default of 25 rows. Pass nil to
	// disable structured progress (Log will still fire).
	Progress      func(MediaIndexProgress)
	ProgressEvery int
}

// MediaIndexProgress is what callers see during a run.
type MediaIndexProgress struct {
	Done       int     `json:"done"`         // processed this run (any status)
	Total      int     `json:"total"`        // total images on device
	Pending    int     `json:"pending"`      // queued for this run
	Baseline   int     `json:"baseline"`     // already done by this phase at run start (for cumulative coverage)
	Downloaded int     `json:"downloaded"`   // images written to media/ this run (download phase)
	Described  int     `json:"described"`    // OCR/describe succeeded count this run
	Missing    int     `json:"missing"`      // file absent in backup this run
	Errors     int     `json:"errors"`       // failures this run
	WithOCR    int     `json:"with_ocr"`     // rows with non-empty ocr_text
	WithDesc   int     `json:"with_desc"`    // rows with a description
	RatePerSec float64 `json:"rate_per_sec"` // images/sec over the run so far
	ETASeconds float64 `json:"eta_seconds"`  // estimated remaining seconds
	ElapsedSec float64 `json:"elapsed_sec"`
	CostUSD    float64 `json:"cost_usd"` // running OpenRouter spend
}

// MediaIndexResult is the final stats summary.
type MediaIndexResult struct {
	BackupPath       string  `json:"backup_path"`
	Workspace        string  `json:"workspace"`
	TotalCandidates  int     `json:"total_candidates"`
	AlreadyDescribed int     `json:"already_described"`
	Processed        int     `json:"processed"`
	Downloaded       int     `json:"downloaded"`
	Described        int     `json:"described"`
	Missing          int     `json:"missing"`
	Errors           int     `json:"errors"`
	WithOCR          int     `json:"with_ocr"`
	WithDesc         int     `json:"with_desc"`
	DurationSec      float64 `json:"duration_sec"`
	FTSCountAfter    int     `json:"fts_count_after"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
	Cancelled        bool    `json:"cancelled,omitempty"`
}

// DownloadMedia decrypts every WhatsApp image referenced in
// ChatStorage.sqlite from the encrypted iOS backup and writes it to
// <Workspace>/media/<rowid>.<ext>. This is the only image step that
// touches the backup, so it is the only one that needs Password. The
// enrichment step (MediaIndex) is a pure consumer of the media/ folder.
// Resumable per-row: a row is marked 'downloaded'
// only after its atomic write + DB commit succeed.
func DownloadMedia(opts MediaIndexOptions) (*MediaIndexResult, error) {
	if opts.Workspace == "" {
		return nil, errors.New("media-download: Workspace required")
	}
	if opts.Password == "" {
		return nil, errors.New("media-download: Password required for encrypted backups")
	}
	if opts.Ctx == nil {
		opts.Ctx = context.Background()
	}
	if opts.Log == nil {
		opts.Log = func(string) {}
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = 25
	}

	dbPath := filepath.Join(opts.Workspace, "ChatStorage.sqlite")
	mediaDir := filepath.Join(opts.Workspace, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir media dir: %w", err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	if err := ensureImageSidecarSchema(db); err != nil {
		return nil, err
	}

	// --- Locate and unlock the backup ---------------------------
	info, err := pickMediaIndexBackup(opts)
	if err != nil {
		return nil, err
	}
	if !info.IsEncrypted {
		return nil, errors.New("media-download: backup is not encrypted (whatskept requires an encrypted backup)")
	}
	opts.Log(fmt.Sprintf("Backup: %s", info.Path))
	opts.Log("Unlocking iOS backup…")
	bundle, err := backup.Open(info, opts.Password)
	if err != nil {
		return nil, fmt.Errorf("open backup: %w", err)
	}
	res := &MediaIndexResult{BackupPath: info.Path, Workspace: opts.Workspace}
	if err := downloadMediaWithBundle(opts, db, bundle, mediaDir, res); err != nil {
		return nil, err
	}
	return res, nil
}

// downloadMediaDuringSync decrypts WhatsApp images into <workspace>/media/
// as a step of the core SyncMessages pipeline, reusing the backup bundle
// the sync already opened. Image files are cheap (decryption is
// milliseconds each), so — like SyncContacts/SyncProfiles — they're pulled
// as part of the sync; only the enrichment (Apple Vision / cloud) is
// optional and deferred. Fail-soft: the caller logs an error and continues.
func downloadMediaDuringSync(bundle *backup.Bundle, workspace, dbPath string, log func(string)) (*MediaIndexResult, error) {
	mediaDir := filepath.Join(workspace, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir media dir: %w", err)
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	if err := ensureImageSidecarSchema(db); err != nil {
		return nil, err
	}

	opts := MediaIndexOptions{
		Workspace:     workspace,
		Ctx:           context.Background(),
		Log:           log,
		ProgressEvery: 500, // periodic "Downloading images…" log lines
		// The sync's SSE stream is log-only, so render progress as lines.
		Progress: func(p MediaIndexProgress) {
			log(fmt.Sprintf("Downloading images… %d / %d", p.Done, p.Pending))
		},
	}
	res := &MediaIndexResult{Workspace: workspace}
	if err := downloadMediaWithBundle(opts, db, bundle, mediaDir, res); err != nil {
		return nil, err
	}
	return res, nil
}

// downloadMediaWithBundle runs the decrypt→write pipeline against an
// already-open backup bundle and DB. Shared by DownloadMedia (which opens
// its own bundle) and downloadMediaDuringSync (which reuses the sync's).
// mediaDir must already exist. Fills res with the run tallies.
func downloadMediaWithBundle(opts MediaIndexOptions, db *sql.DB, bundle *backup.Bundle, mediaDir string, res *MediaIndexResult) error {
	manifestIdx := buildManifestIndex(bundle, backup.WhatsAppDomain)

	total, _, err := countMediaCandidates(db)
	if err != nil {
		return err
	}
	alreadyDownloaded, err := countDownloaded(db)
	if err != nil {
		return err
	}
	candidates, err := selectDownloadCandidates(db, opts.RetryMissing, opts.RetryErrors, opts.Limit)
	if err != nil {
		return err
	}
	res.TotalCandidates = total
	res.AlreadyDescribed = alreadyDownloaded // here: images already on disk
	if len(candidates) == 0 {
		opts.Log(fmt.Sprintf("Images: %d / %d already on disk; nothing to download.", alreadyDownloaded, total))
		return nil
	}
	opts.Log(fmt.Sprintf("Images on device: %d · already on disk: %d · to download: %d",
		total, alreadyDownloaded, len(candidates)))

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = defaultDownloadConcurrency
	}
	var decryptMu sync.Mutex
	process := func(ctx context.Context, c candidate) oneResult {
		return downloadOne(ctx, db, bundle, &decryptMu, manifestIdx, mediaDir, c, opts.Log)
	}
	// Download never changes ocr_text/description, so no FTS rebuild.
	_ = runMediaPipeline(opts, res, db, candidates, total, alreadyDownloaded, concurrency, nil, false, process)

	opts.Log(fmt.Sprintf(
		"Downloaded %d images (%d missing, %d errors) in %.0fs.",
		res.Downloaded, res.Missing, res.Errors, res.DurationSec,
	))
	return nil
}

// MediaIndex describes images that are already on disk in
// <Workspace>/media/ (written by DownloadMedia). It runs the cloud
// describer over every 'downloaded' row, writes the OCR + description
// into wa_image_text, and transitions the row to 'described'. It needs
// NO backup password —
// it never touches the encrypted backup. Resumable per-row.
func MediaIndex(opts MediaIndexOptions) (*MediaIndexResult, error) {
	if opts.Workspace == "" {
		return nil, errors.New("media-index: Workspace required")
	}
	if opts.Ctx == nil {
		opts.Ctx = context.Background()
	}
	if opts.Log == nil {
		opts.Log = func(string) {}
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = 25
	}

	dbPath := filepath.Join(opts.Workspace, "ChatStorage.sqlite")
	mediaDir := filepath.Join(opts.Workspace, "media")

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	if err := ensureImageSidecarSchema(db); err != nil {
		return nil, err
	}

	total, alreadyDescribed, err := countMediaCandidates(db)
	if err != nil {
		return nil, err
	}
	downloaded, err := countDownloaded(db)
	if err != nil {
		return nil, err
	}
	candidates, err := selectDescribeCandidates(db, opts.RetryErrors, opts.Force, opts.Limit)
	if err != nil {
		return nil, err
	}

	res := &MediaIndexResult{
		Workspace:        opts.Workspace,
		TotalCandidates:  total,
		AlreadyDescribed: alreadyDescribed,
	}
	if len(candidates) == 0 {
		opts.Log(fmt.Sprintf("Nothing to describe: %d / %d downloaded images already described.", alreadyDescribed, downloaded))
		ftsN, _ := rebuildFTS(db)
		res.FTSCountAfter = ftsN
		return res, nil
	}
	opts.Log(fmt.Sprintf("Images downloaded:  %d", downloaded))
	opts.Log(fmt.Sprintf("Already described:  %d", alreadyDescribed))
	opts.Log(fmt.Sprintf("Queued this run:    %d", len(candidates)))

	// --- Build the cloud describer -----------------------------
	describer, err := buildDescriber(opts)
	if err != nil {
		return nil, err
	}
	defer describer.Close()
	concurrency := resolveConcurrency(opts.Concurrency)
	opts.Log(fmt.Sprintf("Describer: %s%s (concurrency %d)",
		describer.Source(), modelSuffix(describer.Model()), concurrency))

	// baseline = images already described by the cloud at run start, so
	// the UI can show cumulative coverage (baseline + described-this-run)
	// rather than a per-run count that looks tiny next to prior progress.
	baseline := alreadyDescribed
	_ = db.QueryRow(`SELECT COUNT(*) FROM wa_image_text WHERE source = ?`, SourceCloud).Scan(&baseline)

	process := func(ctx context.Context, c candidate) oneResult {
		return describeOne(ctx, db, describer, mediaDir, c, opts.Log)
	}
	fatalErr := runMediaPipeline(opts, res, db, candidates, total, baseline, concurrency,
		func() float64 { return describerCost(describer) }, true, process)

	opts.Log(fmt.Sprintf(
		"Done in %.0fs. described=%d errors=%d (rate %.1f/s)",
		res.DurationSec, res.Described, res.Errors,
		float64(res.Processed)/maxF(res.DurationSec, 0.001),
	))

	// A fatal failure is surfaced as an error (the committed rows above
	// are kept and searchable) so the caller/job reports WHY it stopped
	// instead of a misleading "success".
	if fatalErr != nil {
		return res, fmt.Errorf("describe run aborted after %d images: %w", res.Described, fatalErr)
	}
	return res, nil
}

// runMediaPipeline drives the image download/describe phases. It is a thin
// per-phase wrapper over the generic runWorkerPipeline (pipeline.go),
// supplying the media-specific tally, progress emission, and FTS rebuild.
// It fills res and returns the first FatalError seen (download never
// produces one).
func runMediaPipeline(
	opts MediaIndexOptions,
	res *MediaIndexResult,
	db *sql.DB,
	candidates []candidate,
	total, baseline, concurrency int,
	costFn func() float64,
	rebuildFTSAfter bool,
	process func(ctx context.Context, c candidate) oneResult,
) error {
	progressEvery := opts.ProgressEvery
	if progressEvery <= 0 {
		progressEvery = 25 // guard the progress-emit modulo
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
			// Rebuild FTS so the new ocr_text + description are searchable.
			opts.Log("Rebuilding messages_fts…")
			if ftsN, err := rebuildFTS(db); err != nil {
				opts.Log(fmt.Sprintf("FTS rebuild failed (non-fatal): %v", err))
			} else {
				res.FTSCountAfter = ftsN
				opts.Log(fmt.Sprintf("messages_fts: %d rows indexed.", ftsN))
			}
		}
	}

	return runWorkerPipeline(opts.Ctx, db, candidates, concurrency, progressEvery,
		process,
		func(r oneResult) (string, error) { return r.status, r.fatal },
		func(err error) { opts.Log("Aborting: " + err.Error()) },
		func(r oneResult) {
			switch r.status {
			case MediaStatusDownloaded:
				res.Downloaded++
			case MediaStatusDescribed:
				res.Described++
				if r.withOCR {
					res.WithOCR++
				}
				if r.withDescription {
					res.WithDesc++
				}
			case MediaStatusMissing:
				res.Missing++
			case MediaStatusError:
				res.Errors++
			}
			res.Processed++
		},
		func() {
			res.CostUSD = cost()
			emitProgress(opts.Progress, res, total, baseline, len(candidates), tStart)
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

// buildDescriber constructs the Describer for opts.Engine via the
// imageDescribers registry (empty defaults to cloud). Cloud requires an
// API key. The caller owns Close().
func buildDescriber(opts MediaIndexOptions) (Describer, error) {
	return imageDescribers.build(opts.Engine, opts.APIKey, opts.Model)
}

func modelSuffix(m string) string {
	if m == "" {
		return ""
	}
	return " (" + m + ")"
}

// resolveConcurrency picks the in-flight Describe cap: the caller's
// explicit value, or defaultCloudConcurrency.
func resolveConcurrency(requested int) int {
	if requested > 0 {
		return requested
	}
	return defaultCloudConcurrency
}

// costReporter is optionally implemented by a Describer that can
// report cumulative spend (cloudDescriber does).
type costReporter interface{ CostUSD() float64 }

func describerCost(d Describer) float64 {
	if c, ok := d.(costReporter); ok {
		return c.CostUSD()
	}
	return 0
}

// oneResult is downloadOne/describeOne's outcome, carrying the per-row
// tallies so the collector needn't re-query the DB for progress counts.
type oneResult struct {
	status          string
	withOCR         bool
	withDescription bool
	fatal           error // non-nil → a global failure; abort the whole run
}

// pickMediaIndexBackup honours opts.BackupPath if set, otherwise
// picks the most-recent encrypted backup under opts.BackupRoot. The
// caller is expected to have already verified identity if there's a
// workspace binding — we don't double-check here because media-index
// runs against the SAME backup the last SyncMessages used, by
// definition (livePath is downstream of that sync).
func pickMediaIndexBackup(opts MediaIndexOptions) (backup.Info, error) {
	if opts.BackupPath != "" {
		info, err := backup.LoadInfo(opts.BackupPath)
		if err != nil {
			return backup.Info{}, fmt.Errorf("load backup %s: %w", opts.BackupPath, err)
		}
		return *info, nil
	}
	root := opts.BackupRoot
	if root == "" {
		root = filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "MobileSync", "Backup")
	}
	infos, err := backup.Discover(root)
	if err != nil {
		return backup.Info{}, fmt.Errorf("discover backups: %w", err)
	}
	var encrypted []backup.Info
	for _, b := range infos {
		if b.IsEncrypted {
			encrypted = append(encrypted, b)
		}
	}
	if len(encrypted) == 0 {
		return backup.Info{}, fmt.Errorf("no encrypted backups under %s", root)
	}
	sort.SliceStable(encrypted, func(i, j int) bool {
		return encrypted[i].LastBackup.After(encrypted[j].LastBackup)
	})
	return encrypted[0], nil
}

// buildManifestIndex returns a map from manifest path → Record
// pointer, scoped to a single domain. We only need WhatsApp media,
// and indexing the whole manifest is ~5 ms.
func buildManifestIndex(b *backup.Bundle, domain string) map[string]*backup.Record {
	recs := b.Records()
	out := make(map[string]*backup.Record, 4096)
	for i := range recs {
		r := &recs[i]
		if r.Domain == domain {
			out[r.Path] = r
		}
	}
	return out
}

// candidate is one row we'll try to OCR.
type candidate struct {
	rowid        int64
	manifestPath string // 'Message/Media/.../foo.jpg'
	msgType      int
}

// countMediaCandidates returns (total_in_db, already_described).
func countMediaCandidates(db *sql.DB) (total, already int, err error) {
	if err = db.QueryRow(
		`SELECT COUNT(*)
		 FROM ZWAMEDIAITEM m
		 JOIN ZWAMESSAGE wm ON wm.Z_PK = m.ZMESSAGE
		 WHERE m.ZMEDIALOCALPATH LIKE '%.jpg'`,
	).Scan(&total); err != nil {
		return 0, 0, fmt.Errorf("count total: %w", err)
	}
	if err = db.QueryRow(
		`SELECT COUNT(*) FROM media_index WHERE status = ?`,
		MediaStatusDescribed,
	).Scan(&already); err != nil {
		return 0, 0, fmt.Errorf("count described: %w", err)
	}
	return total, already, nil
}

// countDownloaded returns how many images are on disk in media/ — rows
// that are either 'downloaded' (awaiting describe) or 'described' (a
// described row's file is still on disk). This is the denominator the
// describe phase and the UI gate measure against.
func countDownloaded(db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM media_index WHERE status IN (?, ?)`,
		MediaStatusDownloaded, MediaStatusDescribed,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count downloaded: %w", err)
	}
	return n, nil
}

// CountDescribePending returns how many on-disk images a normal cloud
// describe run (no force, no retry) would actually queue: fresh
// 'downloaded' rows with no prior describe_error, plus legacy non-cloud
// 'described' rows to upgrade. It mirrors selectDescribeCandidates'
// non-force predicate EXACTLY so the UI can gate "Resume" on real work —
// unlike downloaded−described, which also counts permanently-failed rows
// the queue skips (the cause of the phantom "Resume" → "Nothing to
// describe" loop).
func CountDescribePending(db *sql.DB) (int, error) {
	var n int
	q := `SELECT COUNT(*) FROM (
		SELECT rowid FROM media_index
		  WHERE status = ? AND describe_error IS NULL
		UNION
		SELECT mi.rowid FROM media_index mi
		  JOIN wa_image_text t ON t.rowid = mi.rowid
		  WHERE mi.status = ? AND t.source <> ?
	)`
	if err := db.QueryRow(q, MediaStatusDownloaded, MediaStatusDescribed, SourceCloud).Scan(&n); err != nil {
		return 0, fmt.Errorf("count describe pending: %w", err)
	}
	return n, nil
}

// CountDescribeFailed returns on-disk images that failed a previous
// describe attempt (describe_error set, row still 'downloaded'). A normal
// run skips these; only "Retry failures" (retryErrors) re-attempts them.
func CountDescribeFailed(db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM media_index WHERE status = ? AND describe_error IS NOT NULL`,
		MediaStatusDownloaded,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count describe failed: %w", err)
	}
	return n, nil
}

// selectDownloadCandidates is the resume query for the download phase:
// every WhatsApp image whose file is NOT already on disk. Rows already
// 'downloaded' or 'described' are skipped (file present); 'missing' and
// download-'error' rows are skipped unless the matching retry flag asks
// to re-attempt them.
func selectDownloadCandidates(db *sql.DB, retryMissing, retryErrors bool, limit int) ([]candidate, error) {
	skip := []any{MediaStatusDownloaded, MediaStatusDescribed}
	if !retryMissing {
		skip = append(skip, MediaStatusMissing)
	}
	if !retryErrors {
		skip = append(skip, MediaStatusError)
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(skip)), ",")
	where := fmt.Sprintf(
		"m.ZMEDIALOCALPATH LIKE '%%.jpg'"+
			"\n\t\t  AND m.ZMESSAGE NOT IN (SELECT rowid FROM media_index WHERE status IN (%s))", ph)
	return queryCandidates(db, where, skip, limit)
}

// selectDescribeCandidates is the resume query for the cloud-describe
// phase. It only ever queues rows whose file is already on disk:
// every 'downloaded' row, plus any 'described' row NOT produced by the
// cloud (legacy on-device descriptions, upgraded to cloud), never
// re-doing a cloud row. A prior per-image describe failure
// (describe_error set) is skipped unless retryErrors. With force,
// re-describes every on-disk row regardless of source.
func selectDescribeCandidates(db *sql.DB, retryErrors, force bool, limit int) ([]candidate, error) {
	downloadedReady := "status = '" + MediaStatusDownloaded + "'"
	if !retryErrors {
		downloadedReady += " AND describe_error IS NULL"
	}

	var sub string
	if force {
		sub = "SELECT rowid FROM media_index WHERE status IN ('" +
			MediaStatusDownloaded + "','" + MediaStatusDescribed + "')"
	} else {
		sub = "SELECT rowid FROM media_index WHERE " + downloadedReady +
			" UNION SELECT mi.rowid FROM media_index mi" +
			" JOIN wa_image_text t ON t.rowid = mi.rowid" +
			" WHERE mi.status = '" + MediaStatusDescribed + "' AND t.source <> '" + SourceCloud + "'"
	}
	where := "m.ZMEDIALOCALPATH LIKE '%.jpg'\n\t\t  AND m.ZMESSAGE IN (" + sub + ")"
	return queryCandidates(db, where, nil, limit)
}

// queryCandidates runs the shared candidate SELECT against the given
// WHERE clause (which both selectors build) and scans the rows into
// candidate structs.
func queryCandidates(db *sql.DB, whereClause string, args []any, limit int) ([]candidate, error) {
	q := fmt.Sprintf(`
		SELECT m.ZMESSAGE,
		       '%s' || m.ZMEDIALOCALPATH,
		       wm.ZMESSAGETYPE
		FROM   ZWAMEDIAITEM m
		JOIN   ZWAMESSAGE   wm ON wm.Z_PK = m.ZMESSAGE
		WHERE  %s
		ORDER BY m.ZMESSAGE ASC`,
		mediaManifestPrefix, whereClause)
	if limit > 0 {
		q += fmt.Sprintf("\n\t\tLIMIT %d", limit)
	}

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("select candidates: %w", err)
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.rowid, &c.manifestPath, &c.msgType); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// downloadOne runs the decrypt → format-sniff → atomic-write pipeline
// for one candidate and records the outcome in media_index
// (downloaded / missing / error). It never describes. Safe to run from
// multiple goroutines: the decrypt is serialised behind decryptMu and
// DB writes go through a single-connection pool (see runMediaPipeline).
// A row is marked 'downloaded' only after a clean write + commit, so a
// crash mid-run simply re-queues it.
func downloadOne(
	ctx context.Context,
	db *sql.DB,
	bundle *backup.Bundle,
	decryptMu *sync.Mutex,
	manifestIdx map[string]*backup.Record,
	mediaDir string,
	c candidate,
	log func(string),
) oneResult {
	now := nowUTC()

	if ctx.Err() != nil {
		return oneResult{status: statusCancelled}
	}

	// 1. Find the file in the manifest.
	rec, ok := manifestIdx[c.manifestPath]
	if !ok {
		writeMediaIndex(db, c, MediaStatusMissing, 0, "", now)
		return oneResult{status: MediaStatusMissing}
	}

	// 2. Decrypt to memory. WhatsApp images are small (~50-500 KB);
	//    no streaming needed and we want a single bytes blob for
	//    the magic-byte sanity check and the on-disk write.
	//
	//    EOF is its own bucket: the manifest references the blob
	//    but iOS didn't actually persist its bytes (selective
	//    backup, or a media item that's still uploading). User-side
	//    that's identical to "file not in manifest", so reclassify
	//    as missing instead of error.
	// The backup reader isn't known-concurrency-safe, so serialise the
	// decrypt+read. It's milliseconds next to the disk write, so
	// holding the lock here costs the worker pool almost nothing.
	decryptMu.Lock()
	rd, err := bundle.FileReader(*rec)
	if err != nil {
		decryptMu.Unlock()
		if errors.Is(err, io.EOF) {
			writeMediaIndex(db, c, MediaStatusMissing, 0, "", now)
			return oneResult{status: MediaStatusMissing}
		}
		writeMediaIndex(db, c, MediaStatusError, 0,
			fmt.Sprintf("decrypt: %v", err), now)
		return oneResult{status: MediaStatusError}
	}
	data, err := io.ReadAll(rd)
	_ = rd.Close()
	decryptMu.Unlock()
	if err != nil {
		if errors.Is(err, io.EOF) {
			writeMediaIndex(db, c, MediaStatusMissing, 0, "", now)
			return oneResult{status: MediaStatusMissing}
		}
		writeMediaIndex(db, c, MediaStatusError, int64(len(data)),
			fmt.Sprintf("read: %v", err), now)
		return oneResult{status: MediaStatusError}
	}

	// 3. Format check. WhatsApp's ZMEDIALOCALPATH always ends in
	//    `.jpg` regardless of the actual blob — in practice we see
	//    JPEG, PNG (esp. stickers / canvas-rendered group content),
	//    HEIC (Live Photos and modern iPhone cameras), and GIF.
	//    Apple Vision handles all four natively. Anything else
	//    (zero-byte files, truncated downloads, AES-padded garbage)
	//    is recorded as STATUS_ERROR rather than written, since no
	//    describer could read it later.
	ext, ok := detectImageFormat(data)
	if !ok {
		writeMediaIndex(db, c, MediaStatusError, int64(len(data)),
			"unrecognized image format", now)
		return oneResult{status: MediaStatusError}
	}

	// 4. Write to disk. Atomic via tmp+rename so an interrupted
	//    write doesn't leave a half-image a describer would barf on.
	//    Use the detected extension so `file` and `open` behave
	//    honestly — agents glob `<rowid>.*` per AGENTS.md.
	out := filepath.Join(mediaDir, fmt.Sprintf("%d.%s", c.rowid, ext))
	if err := writeFileAtomic(out, data); err != nil {
		writeMediaIndex(db, c, MediaStatusError, int64(len(data)),
			fmt.Sprintf("write: %v", err), now)
		return oneResult{status: MediaStatusError}
	}

	// 5. Mark downloaded (file on disk, awaiting describe). INSERT OR
	//    REPLACE clears any prior error/describe_error for this row.
	writeMediaIndex(db, c, MediaStatusDownloaded, int64(len(data)), "", now)
	return oneResult{status: MediaStatusDownloaded}
}

// describeOne reads one already-downloaded image off disk and runs the
// describer over it (Apple Vision subprocess or cloud HTTP), writing the
// result into wa_image_text and transitioning the media_index row
// downloaded→described. It never touches the backup. A per-image
// describe failure keeps status='downloaded' and records the reason in
// describe_error so the row is NOT re-downloaded and the retry path can
// find it. The returned oneResult.status is for the in-memory tally only.
func describeOne(
	ctx context.Context,
	db *sql.DB,
	describer Describer,
	mediaDir string,
	c candidate,
	log func(string),
) oneResult {
	now := nowUTC()

	// Locate the on-disk image written by the download step.
	path, ok := findMediaFile(mediaDir, c.rowid)
	if !ok {
		// File vanished (user cleared media/). Leave the row as
		// 'downloaded' so a re-download re-queues it, but record why
		// this describe attempt failed.
		setDescribeError(db, c.rowid, "file missing on disk at describe time", now)
		return oneResult{status: MediaStatusError}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		setDescribeError(db, c.rowid, fmt.Sprintf("read: %v", err), now)
		return oneResult{status: MediaStatusError}
	}

	dres, err := describer.Describe(ctx, c.rowid, path, data)
	if err != nil {
		// A global failure (bad key, no credits) must abort the whole
		// run — leave the row 'downloaded' (no describe_error, so it
		// re-queues cleanly) and signal fatal to the collector.
		var fatal *FatalError
		if errors.As(err, &fatal) {
			return oneResult{status: statusCancelled, fatal: err}
		}
		// Cancellation isn't a per-row error — the row stays 'downloaded'
		// and is simply re-queued next run.
		if ctx.Err() != nil {
			return oneResult{status: statusCancelled}
		}
		setDescribeError(db, c.rowid, fmt.Sprintf("describe: %v", err), now)
		return oneResult{status: MediaStatusError}
	}

	// Persist results. One transaction so wa_image_text and the
	// media_index status flip stay atomic. INSERT OR REPLACE on
	// wa_image_text so a re-run (force, or a cloud upgrade) overwrites
	// cleanly, including the source/model provenance.
	tx, err := db.Begin()
	if err != nil {
		log(fmt.Sprintf("[rowid=%d] begin tx: %v", c.rowid, err))
		return oneResult{status: MediaStatusError}
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO wa_image_text
		 (rowid, ocr_text, language, description, source, model, generated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.rowid, dres.OCRText, dres.Language,
		dres.Description, describer.Source(), describer.Model(), now,
	); err != nil {
		_ = tx.Rollback()
		log(fmt.Sprintf("[rowid=%d] insert wa_image_text: %v", c.rowid, err))
		return oneResult{status: MediaStatusError}
	}
	if _, err := tx.Exec(
		`UPDATE media_index SET status = ?, describe_error = NULL, attempted_at = ? WHERE rowid = ?`,
		MediaStatusDescribed, now, c.rowid,
	); err != nil {
		_ = tx.Rollback()
		log(fmt.Sprintf("[rowid=%d] update media_index: %v", c.rowid, err))
		return oneResult{status: MediaStatusError}
	}
	if err := tx.Commit(); err != nil {
		log(fmt.Sprintf("[rowid=%d] commit: %v", c.rowid, err))
		return oneResult{status: MediaStatusError}
	}
	return oneResult{
		status:          MediaStatusDescribed,
		withOCR:         dres.OCRText != "",
		withDescription: dres.Description != "",
	}
}

// findMediaFile returns the on-disk path of the downloaded image for a
// rowid, probing each extension the download step may have written
// (jpg / png / heic / gif). ok=false means no file is present.
func findMediaFile(mediaDir string, rowid int64) (string, bool) {
	for _, ext := range mediaImageExts {
		p := filepath.Join(mediaDir, fmt.Sprintf("%d%s", rowid, ext))
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	return "", false
}

// setDescribeError records a per-image describe failure on an existing
// (downloaded) media_index row without changing its status, so the file
// stays on disk and is not re-downloaded.
func setDescribeError(db *sql.DB, rowid int64, msg, now string) {
	_, _ = db.Exec(
		`UPDATE media_index SET describe_error = ?, attempted_at = ? WHERE rowid = ?`,
		msg, now, rowid,
	)
}

// writeMediaIndex commits a single media_index row outside any
// caller-managed transaction. Used by the download phase for every
// terminal outcome (downloaded / missing / error). INSERT OR REPLACE
// resets describe_error to NULL — a freshly (re)downloaded row has no
// describe failure.
func writeMediaIndex(db *sql.DB, c candidate, status string, bytesLen int64, errMsg string, now string) {
	var errVal any
	if errMsg != "" {
		errVal = errMsg
	}
	_, _ = db.Exec(
		`INSERT OR REPLACE INTO media_index
		 (rowid, manifest_path, msg_type, status, bytes, error, describe_error, attempted_at)
		 VALUES (?, ?, ?, ?, ?, ?, NULL, ?)`,
		c.rowid, c.manifestPath, c.msgType, status, bytesLen, errVal, now,
	)
}

// writeFileAtomic writes data to path via a sibling tmp file then
// renames. Same-filesystem rename is atomic on macOS, so a crashed
// process never leaves a torn JPEG behind.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func emitProgress(cb func(MediaIndexProgress), res *MediaIndexResult, total, already, pending int, started time.Time) {
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
	cb(MediaIndexProgress{
		Done:       res.Processed,
		Total:      total,
		Pending:    pending,
		Baseline:   already,
		Downloaded: res.Downloaded,
		Described:  res.Described,
		Missing:    res.Missing,
		Errors:     res.Errors,
		WithOCR:    res.WithOCR,
		WithDesc:   res.WithDesc,
		RatePerSec: rate,
		ETASeconds: eta,
		ElapsedSec: elapsed,
		CostUSD:    res.CostUSD,
	})
}
