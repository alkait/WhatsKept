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
	Total      int     `json:"total"`        // total candidates ever
	Pending    int     `json:"pending"`      // queued for this run
	Described  int     `json:"described"`    // OCR succeeded count this run
	Missing    int     `json:"missing"`      // file absent in backup this run
	Errors     int     `json:"errors"`       // failures this run
	WithOCR    int     `json:"with_ocr"`     // rows with non-empty ocr_text
	WithLabels int     `json:"with_labels"`  // rows with at least one label
	RatePerSec float64 `json:"rate_per_sec"` // images/sec over the run so far
	ETASeconds float64 `json:"eta_seconds"`  // estimated remaining seconds
	ElapsedSec float64 `json:"elapsed_sec"`
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
	DurationSec      float64 `json:"duration_sec"`
	FTSCountAfter    int     `json:"fts_count_after"`
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
	if _, err := db.Exec(createImageSidecarsSQL); err != nil {
		return nil, fmt.Errorf("create sidecar tables: %w", err)
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
	candidates, err := selectMediaCandidates(db, opts.RetryMissing, opts.RetryErrors, opts.Limit)
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

	// --- Start the Swift Vision worker -------------------------
	helperPath, err := resolveVisionHelper()
	if err != nil {
		return nil, fmt.Errorf("locate vision helper: %w", err)
	}
	worker, err := startVisionWorker(opts.Ctx, helperPath, opts.LabelTopN, opts.LabelMinConf)
	if err != nil {
		return nil, fmt.Errorf("start vision helper: %w", err)
	}
	defer worker.Close()

	// --- Main loop ---------------------------------------------
	res := &MediaIndexResult{
		BackupPath:       info.Path,
		Workspace:        opts.Workspace,
		TotalCandidates:  total,
		AlreadyDescribed: alreadyDescribed,
	}
	tStart := time.Now()

	for i, c := range candidates {
		// SIGINT / Cancel between rows.
		select {
		case <-opts.Ctx.Done():
			res.Cancelled = true
			opts.Log("Stopped on user request. All committed rows are safe; re-run to resume.")
			res.DurationSec = time.Since(tStart).Seconds()
			return res, nil
		default:
		}

		status := processOne(db, bundle, manifestIdx, worker, mediaDir, c, opts.Log)
		res.Processed++
		switch status {
		case MediaStatusDescribed:
			res.Described++
			// Quick re-read to count ocr/labels hits for the
			// progress display. Cheap (single PK lookup).
			var hasOCR, hasLabels int
			_ = db.QueryRow(
				`SELECT ocr_text != '' AS o, labels != '' AS l FROM wa_image_text WHERE rowid = ?`,
				c.rowid,
			).Scan(&hasOCR, &hasLabels)
			if hasOCR == 1 {
				res.WithOCR++
			}
			if hasLabels == 1 {
				res.WithLabels++
			}
		case MediaStatusMissing:
			res.Missing++
		case MediaStatusError:
			res.Errors++
		}

		if opts.Progress != nil && (i+1)%opts.ProgressEvery == 0 {
			emitProgress(opts.Progress, res, total, alreadyDescribed, len(candidates), tStart)
		}
	}

	res.DurationSec = time.Since(tStart).Seconds()
	if opts.Progress != nil {
		emitProgress(opts.Progress, res, total, alreadyDescribed, len(candidates), tStart)
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

	return res, nil
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
func selectMediaCandidates(db *sql.DB, retryMissing, retryErrors bool, limit int) ([]candidate, error) {
	skip := []any{MediaStatusDescribed}
	if !retryMissing {
		skip = append(skip, MediaStatusMissing)
	}
	if !retryErrors {
		skip = append(skip, MediaStatusError)
	}
	placeholders := make([]byte, 0, len(skip)*2)
	for i := range skip {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
	}

	q := fmt.Sprintf(`
		SELECT m.ZMESSAGE,
		       '%s' || m.ZMEDIALOCALPATH,
		       wm.ZMESSAGETYPE
		FROM   ZWAMEDIAITEM m
		JOIN   ZWAMESSAGE   wm ON wm.Z_PK = m.ZMESSAGE
		WHERE  m.ZMEDIALOCALPATH LIKE '%%.jpg'
		  AND  m.ZMESSAGE NOT IN (
		         SELECT rowid FROM media_index WHERE status IN (%s)
		       )
		ORDER BY m.ZMESSAGE ASC`,
		mediaManifestPrefix, string(placeholders))
	if limit > 0 {
		q += fmt.Sprintf("\n\t\tLIMIT %d", limit)
	}

	rows, err := db.Query(q, skip...)
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

// processOne runs the full decrypt → vision → commit pipeline for
// one candidate. Returns the terminal status so the caller can
// accumulate counts. Per-row commit means a SIGINT mid-loop is safe.
func processOne(
	db *sql.DB,
	bundle *backup.Bundle,
	manifestIdx map[string]*backup.Record,
	worker *visionWorker,
	mediaDir string,
	c candidate,
	log func(string),
) string {
	now := nowUTC()

	// 1. Find the file in the manifest.
	rec, ok := manifestIdx[c.manifestPath]
	if !ok {
		writeMediaIndex(db, c, MediaStatusMissing, 0, "", now)
		return MediaStatusMissing
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
	rd, err := bundle.FileReader(*rec)
	if err != nil {
		if errors.Is(err, io.EOF) {
			writeMediaIndex(db, c, MediaStatusMissing, 0, "", now)
			return MediaStatusMissing
		}
		writeMediaIndex(db, c, MediaStatusError, 0,
			fmt.Sprintf("decrypt: %v", err), now)
		return MediaStatusError
	}
	data, err := io.ReadAll(rd)
	_ = rd.Close()
	if err != nil {
		if errors.Is(err, io.EOF) {
			writeMediaIndex(db, c, MediaStatusMissing, 0, "", now)
			return MediaStatusMissing
		}
		writeMediaIndex(db, c, MediaStatusError, int64(len(data)),
			fmt.Sprintf("read: %v", err), now)
		return MediaStatusError
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
		return MediaStatusError
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
		return MediaStatusError
	}

	// 5. Vision call.
	vres, err := worker.describe(c.rowid, out)
	if err != nil {
		writeMediaIndex(db, c, MediaStatusError, int64(len(data)),
			fmt.Sprintf("vision: %v", err), now)
		return MediaStatusError
	}
	if !vres.OK {
		writeMediaIndex(db, c, MediaStatusError, int64(len(data)),
			fmt.Sprintf("vision: %s", vres.Error), now)
		return MediaStatusError
	}

	// 6. Persist results. One transaction so wa_image_text and
	//    media_index are atomic. INSERT OR REPLACE so a re-run
	//    (e.g. after --retry-errors) overwrites cleanly.
	tx, err := db.Begin()
	if err != nil {
		log(fmt.Sprintf("[rowid=%d] begin tx: %v", c.rowid, err))
		return MediaStatusError
	}
	labelsCSV, labelsJSON := encodeLabels(vres.Labels)
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO wa_image_text
		 (rowid, ocr_text, language, labels, label_scores, generated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.rowid, vres.OCRText, vres.Language, labelsCSV, labelsJSON, now,
	); err != nil {
		_ = tx.Rollback()
		log(fmt.Sprintf("[rowid=%d] insert wa_image_text: %v", c.rowid, err))
		return MediaStatusError
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO media_index
		 (rowid, manifest_path, msg_type, status, bytes, error, attempted_at)
		 VALUES (?, ?, ?, ?, ?, NULL, ?)`,
		c.rowid, c.manifestPath, c.msgType, MediaStatusDescribed, int64(len(data)), now,
	); err != nil {
		_ = tx.Rollback()
		log(fmt.Sprintf("[rowid=%d] insert media_index: %v", c.rowid, err))
		return MediaStatusError
	}
	if err := tx.Commit(); err != nil {
		log(fmt.Sprintf("[rowid=%d] commit: %v", c.rowid, err))
		return MediaStatusError
	}
	return MediaStatusDescribed
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
		Described:  res.Described,
		Missing:    res.Missing,
		Errors:     res.Errors,
		WithOCR:    res.WithOCR,
		WithLabels: res.WithLabels,
		RatePerSec: rate,
		ETASeconds: eta,
		ElapsedSec: elapsed,
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
