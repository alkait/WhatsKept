package postprocess

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"whatskept/internal/backup"
	"whatskept/internal/helpers"
)

// This file is the Go port of the Python whatskept.media_indexer
// module. It owns one user-visible operation:
//
//   `whatskept media-index` (CLI) / "Sync images" (GUI):
//   walk every WhatsApp image message in ChatStorage.sqlite,
//   decrypt the JPEG from the iOS backup, run Apple Vision via the
//   bundled Swift helper to get OCR text + classification labels,
//   and persist the results in wa_image_text. Resumable per-row.
//
// Design notes (departures from the Python original):
//
//   - One Swift subprocess for the whole run, talking line-delimited
//     JSON over stdin/stdout. ~30 ms startup cost amortised over
//     thousands of images, ~1 ms protocol overhead per request.
//   - `engine` column dropped (YAGNI — only one engine).
//   - `language` column added to wa_image_text (Apple Vision returns
//     dominant script; useful for "find Arabic-text receipts").
//   - label_min_conf defaults to 0.50 (Python: 0.30); below that
//     Apple Vision labels are mostly "indoor"/"outdoor" noise.
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

	// statusCancelled is an internal, never-persisted sentinel: an
	// in-flight row that processOne abandoned because the context was
	// cancelled. The collector ignores it (no media_index row written,
	// so the image is simply re-queued on the next run).
	statusCancelled = "cancelled"

	// defaultCloudConcurrency is the in-flight cloud request cap when
	// the caller doesn't specify one. Conservative enough to stay well
	// under typical provider rate limits while still ~Nx faster than
	// sequential.
	defaultCloudConcurrency = 8

	// All WhatsApp ChatStorage references to media files are
	// relative paths under the Message/ subtree of WhatsApp's
	// app-group container. Manifest entries carry the full path
	// directly, so we prefix DB-side ZMEDIALOCALPATH with this.
	mediaManifestPrefix = "Message/"

	// Defaults for Vision tunables. Mirror the env-var defaults
	// in build/vision-helper/main.swift so a user running media-
	// index without overrides gets identical behaviour.
	defaultLabelTopN    = 5
	defaultLabelMinConf = 0.50
)

// mediaImageExts lists every on-disk extension processOne is allowed
// to write under <Workspace>/media/. Used by the orphan-prune walk
// to know which suffixes mean "indexer output" vs "user-dropped
// files we don't touch". Keep in sync with detectImageFormat.
var mediaImageExts = []string{".jpg", ".png", ".heic", ".gif"}

// detectImageFormat sniffs the first few bytes of `data` and returns
// a bare extension ("jpg", "png", "heic", "gif") for the four
// formats Apple Vision accepts in WhatsApp content. ok=false means
// nothing matched — the blob is corrupt, encrypted, or some format
// Vision can't handle (TIFF/WebP do exist but we haven't seen them
// in WhatsApp media so far).
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
	// (decrypt failed, Vision failed, etc.).
	RetryErrors bool

	// LabelTopN / LabelMinConf override the Vision defaults.
	// Passed through to the Swift helper via env vars.
	LabelTopN    int
	LabelMinConf float32

	// Engine selects the describer: SourceApple (default, on-device
	// Apple Vision) or SourceCloud (an OpenRouter vision model). Cloud
	// requires APIKey; Model defaults to DefaultCloudModel.
	Engine string
	APIKey string
	Model  string

	// Concurrency caps in-flight Describe calls. 0 = sensible default
	// per engine (cloud parallelises HTTP; Apple is forced to 1 since
	// the Swift helper is a single-stdin subprocess). The decrypt step
	// and DB writes are serialised internally regardless.
	Concurrency int

	// Force re-describes every describable image, overwriting rows
	// already done by this engine (e.g. to apply an improved prompt or
	// switch models). Without it, cloud runs skip already-cloud rows.
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
	Baseline   int     `json:"baseline"`     // already done by this engine at run start (for cumulative coverage)
	Described  int     `json:"described"`    // OCR succeeded count this run
	Missing    int     `json:"missing"`      // file absent in backup this run
	Errors     int     `json:"errors"`       // failures this run
	WithOCR    int     `json:"with_ocr"`     // rows with non-empty ocr_text
	WithLabels int     `json:"with_labels"`  // rows with at least one label (Apple)
	WithDesc   int     `json:"with_desc"`    // rows with a description (cloud)
	RatePerSec float64 `json:"rate_per_sec"` // images/sec over the run so far
	ETASeconds float64 `json:"eta_seconds"`  // estimated remaining seconds
	ElapsedSec float64 `json:"elapsed_sec"`
	CostUSD    float64 `json:"cost_usd"` // running OpenRouter spend (cloud; 0 for Apple)
}

// MediaIndexResult is the final stats summary.
type MediaIndexResult struct {
	BackupPath       string  `json:"backup_path"`
	Workspace        string  `json:"workspace"`
	TotalCandidates  int     `json:"total_candidates"`
	AlreadyDescribed int     `json:"already_described"`
	Processed        int     `json:"processed"`
	Described        int     `json:"described"`
	Missing          int     `json:"missing"`
	Errors           int     `json:"errors"`
	WithOCR          int     `json:"with_ocr"`
	WithLabels       int     `json:"with_labels"`
	WithDesc         int     `json:"with_desc"`
	DurationSec      float64 `json:"duration_sec"`
	FTSCountAfter    int     `json:"fts_count_after"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
	Cancelled        bool    `json:"cancelled,omitempty"`
}

// MediaIndex runs the full image-OCR pipeline against the workspace.
// See package doc-comment in media.go for the design notes.
func MediaIndex(opts MediaIndexOptions) (*MediaIndexResult, error) {
	if opts.Workspace == "" {
		return nil, errors.New("media-index: Workspace required")
	}
	if opts.Password == "" {
		return nil, errors.New("media-index: Password required for encrypted backups")
	}
	if opts.Ctx == nil {
		opts.Ctx = context.Background()
	}
	if opts.Log == nil {
		opts.Log = func(string) {}
	}
	if opts.LabelTopN <= 0 {
		opts.LabelTopN = defaultLabelTopN
	}
	if opts.LabelMinConf < 0 {
		opts.LabelMinConf = defaultLabelMinConf
	} else if opts.LabelMinConf == 0 {
		opts.LabelMinConf = defaultLabelMinConf
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = 25
	}

	dbPath := filepath.Join(opts.Workspace, "ChatStorage.sqlite")
	mediaDir := filepath.Join(opts.Workspace, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir media dir: %w", err)
	}

	// --- Open DB + ensure schema --------------------------------
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
		return nil, errors.New("media-index: backup is not encrypted (whatskept media-index requires an encrypted backup)")
	}
	opts.Log(fmt.Sprintf("Backup: %s", info.Path))
	opts.Log("Unlocking iOS backup…")
	bundle, err := backup.Open(info, opts.Password)
	if err != nil {
		return nil, fmt.Errorf("open backup: %w", err)
	}

	// Build manifest lookup: (domain, path) -> *Record. The whole
	// scan is O(N) once; per-row lookup is O(1). Allocating ~50K
	// pointers costs ~400 KB which is fine.
	opts.Log("Indexing backup manifest…")
	manifestIdx := buildManifestIndex(bundle, backup.WhatsAppDomain)

	// --- Pick candidates ---------------------------------------
	total, alreadyDescribed, err := countMediaCandidates(db)
	if err != nil {
		return nil, err
	}
	// The cloud describer always upgrades: it re-queues every row not
	// already described by cloud (Apple rows, never-described, and
	// transient 'error' rows), and never downgrades a cloud row (those
	// are excluded). Apple Vision is the base layer and never upgrades.
	upgradeSource := ""
	if opts.Engine == SourceCloud && !opts.Force {
		upgradeSource = SourceCloud
	}
	candidates, err := selectMediaCandidates(db, opts.RetryMissing, opts.RetryErrors, upgradeSource, opts.Force, opts.Limit)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		opts.Log(fmt.Sprintf("Nothing to do: %d / %d already described.", alreadyDescribed, total))
		ftsN, _ := rebuildFTS(db)
		return &MediaIndexResult{
			BackupPath:       info.Path,
			Workspace:        opts.Workspace,
			TotalCandidates:  total,
			AlreadyDescribed: alreadyDescribed,
			FTSCountAfter:    ftsN,
		}, nil
	}
	opts.Log(fmt.Sprintf("Image messages on device: %d", total))
	opts.Log(fmt.Sprintf("Already described:        %d", alreadyDescribed))
	opts.Log(fmt.Sprintf("Queued this run:          %d", len(candidates)))

	// --- Build the describer (Apple Vision or cloud) -----------
	describer, err := buildDescriber(opts)
	if err != nil {
		return nil, err
	}
	defer describer.Close()
	concurrency := resolveConcurrency(opts.Engine, opts.Concurrency)
	opts.Log(fmt.Sprintf("Describer: %s%s (concurrency %d)",
		describer.Source(), modelSuffix(describer.Model()), concurrency))

	// --- Worker pool -------------------------------------------
	// Each worker runs the full decrypt → describe → commit pipeline.
	// The describe call (HTTP, for cloud) is the parallel part; the
	// decrypt is serialised behind decryptMu (the backup reader isn't
	// known-concurrency-safe) and DB writes are serialised by capping
	// the SQLite pool to a single connection. Both are sub-millisecond
	// next to a multi-second model call, so we still get ~Nx speedup.
	db.SetMaxOpenConns(1)
	var decryptMu sync.Mutex

	// runCtx is cancelled either by the caller (user Stop) or internally
	// when a worker hits a FatalError (bad key / no credits) — both make
	// the feeder stop and the workers drain.
	runCtx, cancelRun := context.WithCancel(opts.Ctx)
	defer cancelRun()
	var fatalErr error
	var fatalMu sync.Mutex

	res := &MediaIndexResult{
		BackupPath:       info.Path,
		Workspace:        opts.Workspace,
		TotalCandidates:  total,
		AlreadyDescribed: alreadyDescribed,
	}

	// baseline = images already done by THIS engine at run start, so the
	// UI can show cumulative coverage (baseline + described-this-run)
	// rather than a per-run count that looks tiny next to prior progress.
	baseline := alreadyDescribed
	if opts.Engine == SourceCloud {
		_ = db.QueryRow(`SELECT COUNT(*) FROM wa_image_text WHERE source = ?`, SourceCloud).Scan(&baseline)
	}
	tStart := time.Now()

	jobsCh := make(chan candidate)
	resCh := make(chan oneResult, concurrency)

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobsCh {
				resCh <- processOne(runCtx, db, bundle, &decryptMu, manifestIdx, describer, mediaDir, c, opts.Log)
			}
		}()
	}
	// Feeder: stops early (closing jobsCh) when the run context is
	// cancelled, so workers drain and exit.
	go func() {
		defer close(jobsCh)
		for _, c := range candidates {
			select {
			case <-runCtx.Done():
				return
			case jobsCh <- c:
			}
		}
	}()
	go func() { wg.Wait(); close(resCh) }()

	// Collector (this goroutine) tallies results and emits progress.
	processed := 0
	for r := range resCh {
		if r.fatal != nil {
			// First fatal wins; cancel the run so the feeder/workers
			// wind down without marking the rest as errored.
			fatalMu.Lock()
			if fatalErr == nil {
				fatalErr = r.fatal
				opts.Log("Aborting: " + r.fatal.Error())
			}
			fatalMu.Unlock()
			cancelRun()
			continue
		}
		if r.status == statusCancelled {
			continue // in-flight row aborted by cancel; not a real outcome
		}
		res.Processed++
		switch r.status {
		case MediaStatusDescribed:
			res.Described++
			if r.withOCR {
				res.WithOCR++
			}
			if r.withLabels {
				res.WithLabels++
			}
			if r.withDescription {
				res.WithDesc++
			}
		case MediaStatusMissing:
			res.Missing++
		case MediaStatusError:
			res.Errors++
		}
		processed++
		if opts.Progress != nil && processed%opts.ProgressEvery == 0 {
			res.CostUSD = describerCost(describer)
			emitProgress(opts.Progress, res, total, baseline, len(candidates), tStart)
		}
	}

	res.CostUSD = describerCost(describer)
	res.DurationSec = time.Since(tStart).Seconds()
	// A user Stop cancels opts.Ctx; a fatal abort cancels only runCtx.
	if fatalErr == nil && opts.Ctx.Err() != nil {
		res.Cancelled = true
		opts.Log("Stopped on user request. All committed rows are safe; re-run to resume.")
	}
	if opts.Progress != nil {
		emitProgress(opts.Progress, res, total, baseline, len(candidates), tStart)
	}

	opts.Log(fmt.Sprintf(
		"Done in %.0fs. described=%d missing=%d errors=%d (rate %.1f/s)",
		res.DurationSec, res.Described, res.Missing, res.Errors,
		float64(res.Processed)/maxF(res.DurationSec, 0.001),
	))

	// Rebuild FTS so the new ocr_text + labels are searchable.
	opts.Log("Rebuilding messages_fts…")
	ftsN, err := rebuildFTS(db)
	if err != nil {
		opts.Log(fmt.Sprintf("FTS rebuild failed (non-fatal): %v", err))
	} else {
		res.FTSCountAfter = ftsN
		opts.Log(fmt.Sprintf("messages_fts: %d rows indexed.", ftsN))
	}

	// A fatal failure is surfaced as an error (the committed rows above
	// are kept and searchable) so the caller/job reports WHY it stopped
	// instead of a misleading "success".
	if fatalErr != nil {
		return res, fmt.Errorf("describe run aborted after %d images: %w", res.Described, fatalErr)
	}
	return res, nil
}

// buildDescriber constructs the Describer selected by opts.Engine.
// Apple is the default; cloud requires an API key. The caller owns
// Close().
func buildDescriber(opts MediaIndexOptions) (Describer, error) {
	switch opts.Engine {
	case SourceCloud:
		return newCloudDescriber(opts.APIKey, opts.Model)
	case "", SourceApple:
		helperPath, err := resolveVisionHelper()
		if err != nil {
			return nil, fmt.Errorf("locate vision helper: %w", err)
		}
		worker, err := startVisionWorker(opts.Ctx, helperPath, opts.LabelTopN, opts.LabelMinConf)
		if err != nil {
			return nil, fmt.Errorf("start vision helper: %w", err)
		}
		return &appleDescriber{w: worker}, nil
	default:
		return nil, fmt.Errorf("media-index: unknown engine %q (want %q or %q)", opts.Engine, SourceApple, SourceCloud)
	}
}

func modelSuffix(m string) string {
	if m == "" {
		return ""
	}
	return " (" + m + ")"
}

// resolveConcurrency picks the in-flight Describe cap. Apple is pinned
// to 1 (the Swift helper is a single-stdin subprocess); cloud honours
// an explicit value or falls back to defaultCloudConcurrency.
func resolveConcurrency(engine string, requested int) int {
	if engine != SourceCloud {
		return 1
	}
	if requested > 0 {
		return requested
	}
	return defaultCloudConcurrency
}

// costReporter is optionally implemented by a Describer that can
// report cumulative spend (cloudDescriber does; Apple doesn't).
type costReporter interface{ CostUSD() float64 }

func describerCost(d Describer) float64 {
	if c, ok := d.(costReporter); ok {
		return c.CostUSD()
	}
	return 0
}

// oneResult is processOne's outcome, carrying the per-row tallies so
// the collector needn't re-query the DB for progress counts.
type oneResult struct {
	status          string
	withOCR         bool
	withLabels      bool
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

// selectMediaCandidates is the resume query. Rows already in
// media_index with a "skip" status are excluded.
//
// upgradeSource changes the meaning of "done". When empty (the normal
// case) any 'described' row is skipped. When set (e.g. SourceCloud
// during a re-describe), 'described' is NOT in the skip set — instead
// only rows already described by THAT source are excluded, so rows
// produced by a different describer get re-queued (an upgrade) while
// same-source rows stay idempotently skipped.
func selectMediaCandidates(db *sql.DB, retryMissing, retryErrors bool, upgradeSource string, force bool, limit int) ([]candidate, error) {
	// force = re-describe everything describable (overwrite even cloud
	// rows), e.g. to apply an improved prompt or a different model.
	var skip []any
	if upgradeSource == "" && !force {
		skip = append(skip, MediaStatusDescribed)
	}
	if !retryMissing {
		skip = append(skip, MediaStatusMissing) // no file → undescribable
	}
	// In upgrade mode (cloud) and force mode we always re-attempt
	// 'error' rows: these failures are virtually always transient (no
	// credits, network, rate limit), not the model refusing an image.
	// Outside those, honour the explicit retryErrors flag.
	if !retryErrors && upgradeSource == "" && !force {
		skip = append(skip, MediaStatusError)
	}

	var where strings.Builder
	where.WriteString("m.ZMEDIALOCALPATH LIKE '%.jpg'")
	args := make([]any, 0, len(skip)+2)

	if len(skip) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(skip)), ",")
		fmt.Fprintf(&where,
			"\n\t\t  AND m.ZMESSAGE NOT IN (SELECT rowid FROM media_index WHERE status IN (%s))", ph)
		args = append(args, skip...)
	}
	if upgradeSource != "" {
		where.WriteString(
			"\n\t\t  AND m.ZMESSAGE NOT IN (SELECT mi.rowid FROM media_index mi" +
				" JOIN wa_image_text t ON t.rowid = mi.rowid" +
				" WHERE mi.status = ? AND t.source = ?)")
		args = append(args, MediaStatusDescribed, upgradeSource)
	}

	q := fmt.Sprintf(`
		SELECT m.ZMESSAGE,
		       '%s' || m.ZMEDIALOCALPATH,
		       wm.ZMESSAGETYPE
		FROM   ZWAMEDIAITEM m
		JOIN   ZWAMESSAGE   wm ON wm.Z_PK = m.ZMESSAGE
		WHERE  %s
		ORDER BY m.ZMESSAGE ASC`,
		mediaManifestPrefix, where.String())
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
	return out, nil
}

// processOne runs the full decrypt → describe → commit pipeline for
// one candidate and returns the outcome (status + per-row tallies).
// Safe to run from multiple goroutines: the decrypt is serialised
// behind decryptMu and DB writes go through a single-connection pool
// (see MediaIndex). Per-row commit means a cancel mid-run is safe.
func processOne(
	ctx context.Context,
	db *sql.DB,
	bundle *backup.Bundle,
	decryptMu *sync.Mutex,
	manifestIdx map[string]*backup.Record,
	describer Describer,
	mediaDir string,
	c candidate,
	log func(string),
) oneResult {
	now := nowUTC()

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
	// decrypt+read. It's milliseconds next to the describe call, so
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
	//    is recorded as STATUS_ERROR rather than handed to Vision,
	//    which would otherwise emit a generic "unsupported format".
	ext, ok := detectImageFormat(data)
	if !ok {
		writeMediaIndex(db, c, MediaStatusError, int64(len(data)),
			"unrecognized image format", now)
		return oneResult{status: MediaStatusError}
	}

	// 4. Write to disk. Atomic via tmp+rename so an interrupted
	//    write doesn't leave a half-image that Vision would barf
	//    on a future retry. Use the detected extension so `file`
	//    and `open` behave honestly — agents glob `<rowid>.*` per
	//    AGENTS.md.
	out := filepath.Join(mediaDir, fmt.Sprintf("%d.%s", c.rowid, ext))
	if err := writeFileAtomic(out, data); err != nil {
		writeMediaIndex(db, c, MediaStatusError, int64(len(data)),
			fmt.Sprintf("write: %v", err), now)
		return oneResult{status: MediaStatusError}
	}

	// 5. Describe (Apple Vision subprocess or cloud HTTP).
	dres, err := describer.Describe(ctx, c.rowid, out, data)
	if err != nil {
		// A global failure (bad key, no credits) must abort the whole
		// run — don't write an error row (so the image re-queues next
		// time) and signal fatal to the collector.
		var fatal *FatalError
		if errors.As(err, &fatal) {
			return oneResult{status: statusCancelled, fatal: err}
		}
		// Cancellation isn't a per-row error — report it as the
		// internal sentinel so the row is left un-described and simply
		// re-queued next run (no error row written).
		if ctx.Err() != nil {
			return oneResult{status: statusCancelled}
		}
		writeMediaIndex(db, c, MediaStatusError, int64(len(data)),
			fmt.Sprintf("describe: %v", err), now)
		return oneResult{status: MediaStatusError}
	}

	// 6. Persist results. One transaction so wa_image_text and
	//    media_index are atomic. INSERT OR REPLACE so a re-run
	//    (--retry-errors, or a cloud re-describe upgrade) overwrites
	//    cleanly, including the source/model provenance.
	tx, err := db.Begin()
	if err != nil {
		log(fmt.Sprintf("[rowid=%d] begin tx: %v", c.rowid, err))
		return oneResult{status: MediaStatusError}
	}
	labelsCSV, labelsJSON := encodeLabels(dres.Labels)
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO wa_image_text
		 (rowid, ocr_text, language, labels, label_scores, description, source, model, generated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.rowid, dres.OCRText, dres.Language, labelsCSV, labelsJSON,
		dres.Description, describer.Source(), describer.Model(), now,
	); err != nil {
		_ = tx.Rollback()
		log(fmt.Sprintf("[rowid=%d] insert wa_image_text: %v", c.rowid, err))
		return oneResult{status: MediaStatusError}
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO media_index
		 (rowid, manifest_path, msg_type, status, bytes, error, attempted_at)
		 VALUES (?, ?, ?, ?, ?, NULL, ?)`,
		c.rowid, c.manifestPath, c.msgType, MediaStatusDescribed, int64(len(data)), now,
	); err != nil {
		_ = tx.Rollback()
		log(fmt.Sprintf("[rowid=%d] insert media_index: %v", c.rowid, err))
		return oneResult{status: MediaStatusError}
	}
	if err := tx.Commit(); err != nil {
		log(fmt.Sprintf("[rowid=%d] commit: %v", c.rowid, err))
		return oneResult{status: MediaStatusError}
	}
	return oneResult{
		status:          MediaStatusDescribed,
		withOCR:         dres.OCRText != "",
		withLabels:      len(dres.Labels) > 0,
		withDescription: dres.Description != "",
	}
}

// writeMediaIndex commits a single media_index row outside any
// caller-managed transaction. Used for terminal-status writes
// (missing / error) where there's no corresponding wa_image_text
// row to keep atomic with.
func writeMediaIndex(db *sql.DB, c candidate, status string, bytesLen int64, errMsg string, now string) {
	var errVal any
	if errMsg != "" {
		errVal = errMsg
	}
	_, _ = db.Exec(
		`INSERT OR REPLACE INTO media_index
		 (rowid, manifest_path, msg_type, status, bytes, error, attempted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.rowid, c.manifestPath, c.msgType, status, bytesLen, errVal, now,
	)
}

// encodeLabels converts the worker's [[name, score], …] response
// into the two columns wa_image_text expects:
//   - labels       : "dog,cat,pet"          (CSV, FTS-friendly)
//   - label_scores : {"dog":0.94,...}       (JSON, exact scores)
func encodeLabels(ls []visionLabel) (csv, scores string) {
	if len(ls) == 0 {
		return "", "{}"
	}
	names := make([]string, len(ls))
	scoreMap := make(map[string]float32, len(ls))
	for i, l := range ls {
		names[i] = l.Name
		scoreMap[l.Name] = l.Score
	}
	csv = joinCSV(names)
	b, _ := json.Marshal(scoreMap)
	scores = string(b)
	return
}

// joinCSV joins string labels with ',' separator. Labels contain
// only ASCII letters/digits/hyphens (Apple's classifier identifiers),
// so we don't need to escape.
func joinCSV(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	n := 0
	for _, p := range parts {
		n += len(p) + 1
	}
	out := make([]byte, 0, n)
	for i, p := range parts {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, p...)
	}
	return string(out)
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
		Described:  res.Described,
		Missing:    res.Missing,
		Errors:     res.Errors,
		WithOCR:    res.WithOCR,
		WithLabels: res.WithLabels,
		WithDesc:   res.WithDesc,
		RatePerSec: rate,
		ETASeconds: eta,
		ElapsedSec: elapsed,
		CostUSD:    res.CostUSD,
	})
}

// resolveVisionHelper returns the absolute path to the extracted
// whatskept-vision binary. helpers.Path() handles the embed →
// cache-dir extraction; we just append the filename.
func resolveVisionHelper() (string, error) {
	dir, err := helpers.Path()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, helpers.WhatskeptVision)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("helper not found at %s: %w", p, err)
	}
	return p, nil
}

// -------------------------------------------------------------------
// Vision worker subprocess client
// -------------------------------------------------------------------

// visionWorker is a request-response client for the Swift Vision
// helper. Synchronous from the caller's perspective: each describe()
// call writes one JSON line and blocks until the matching response
// arrives. We don't pipeline requests because Vision is the
// dominant cost (~100-300 ms each) and overlapping two Swift calls
// against one process buys little — the Swift binary uses one
// VNImageRequestHandler at a time. If we ever want more
// concurrency, spawning a second worker is the simplest path.
type visionWorker struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Reader
	stderr   io.ReadCloser
	closeErr error
}

type visionRequest struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
}

type visionLabel struct {
	Name  string
	Score float32
}

// visionResponse is the parsed Swift-side reply. Labels arrive as
// [["name", score], ...] which JSON-decodes into a tuple-shaped
// slice via custom UnmarshalJSON below.
type visionResponse struct {
	ID       int64
	OK       bool
	OCRText  string
	Language string
	Labels   []visionLabel
	Error    string
}

func (vr *visionResponse) UnmarshalJSON(data []byte) error {
	// Decode into a flexible shape first so we can pick at the
	// labels array which is [[name, score], ...] not [{name,score}].
	var raw struct {
		ID       int64               `json:"id"`
		OK       bool                `json:"ok"`
		OCRText  string              `json:"ocr_text"`
		Language string              `json:"language"`
		Labels   [][]json.RawMessage `json:"labels"`
		Error    string              `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	vr.ID = raw.ID
	vr.OK = raw.OK
	vr.OCRText = raw.OCRText
	vr.Language = raw.Language
	vr.Error = raw.Error
	vr.Labels = make([]visionLabel, 0, len(raw.Labels))
	for _, pair := range raw.Labels {
		if len(pair) != 2 {
			continue
		}
		var name string
		var score float32
		if err := json.Unmarshal(pair[0], &name); err != nil {
			continue
		}
		if err := json.Unmarshal(pair[1], &score); err != nil {
			continue
		}
		vr.Labels = append(vr.Labels, visionLabel{Name: name, Score: score})
	}
	return nil
}

func startVisionWorker(ctx context.Context, path string, labelTopN int, labelMinConf float32) (*visionWorker, error) {
	cmd := exec.CommandContext(ctx, path)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("WHATSKEPT_VISION_LABEL_TOP_N=%d", labelTopN),
		fmt.Sprintf("WHATSKEPT_VISION_LABEL_MIN_CONF=%.2f", labelMinConf),
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start helper: %w", err)
	}
	return &visionWorker{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdout, 64*1024),
		stderr: stderr,
	}, nil
}

func (w *visionWorker) describe(rowid int64, path string) (*visionResponse, error) {
	req := visionRequest{ID: rowid, Path: path}
	enc, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	enc = append(enc, '\n')
	if _, err := w.stdin.Write(enc); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	line, err := w.stdout.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var resp visionResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w (line=%q)", err, string(line))
	}
	if resp.ID != rowid {
		// Out-of-order shouldn't happen with our synchronous
		// protocol, but if it does the safest thing is to fail
		// the call so the caller marks the row as error and
		// can retry.
		return nil, fmt.Errorf("response id mismatch: want=%d got=%d", rowid, resp.ID)
	}
	return &resp, nil
}

// Close shuts the worker down cleanly. Closing stdin → Swift
// loop exits with status 0 → cmd.Wait returns. Called via defer
// from MediaIndex; safe to call twice.
func (w *visionWorker) Close() error {
	if w.cmd == nil {
		return w.closeErr
	}
	if w.stdin != nil {
		_ = w.stdin.Close()
		w.stdin = nil
	}
	w.closeErr = w.cmd.Wait()
	w.cmd = nil
	return w.closeErr
}
