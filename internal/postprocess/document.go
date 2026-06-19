package postprocess

// WhatsApp PDF documents, in two phases mirroring the voice pipeline
// (voice.go):
//
//   1. DownloadDocument — decrypt every WhatsApp PDF from the iOS backup
//      into <workspace>/documents/<rowid>.pdf. The only step that needs
//      the backup password; run as part of SyncMessages. Non-PDF
//      documents (xlsx/docx/…) are parked as 'unsupported' without any
//      decrypt — there's no extractor for them yet; their filenames stay
//      searchable via wa_document (rebuilt by views.sql every sync).
//   2. DocumentIndex — extract every 'downloaded' PDF's text via the
//      cloud (OpenRouter's file-parser plugin: free pdf-text for the
//      native layer, mistral-ocr for scanned pages), persisting the body
//      in wa_document_text and flipping the row to 'extracted'. No
//      password — a pure consumer of documents/. Needs an OpenRouter key.
//
// Both are resumable per-row. This replaces the previous macOS-only path
// (Apple PDFKit + Vision OCR via a bundled Swift helper); the cloud path
// is a pure in-process HTTP client, so it works on macOS, Windows, and
// Linux. See document_cloud.go for the extractor.

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
	// Status values stored in document_index.status. See
	// createDocumentSidecarsSQL for the closed set and the full notes.
	DocumentStatusExtracted      = "extracted"
	DocumentStatusExtractedEmpty = "extracted_empty"
	DocumentStatusMissing        = "missing"
	DocumentStatusUnsupported    = "unsupported"
	DocumentStatusError          = "error"

	// DocumentStatusDownloaded means the PDF is on disk at
	// <Workspace>/documents/<rowid>.pdf but not yet extracted — the resting
	// state between the (password-gated) download and the cloud extract. A
	// per-doc extract failure keeps this status and records the reason in
	// extract_error, so the download step never re-downloads a file that's
	// already on disk.
	DocumentStatusDownloaded = "downloaded"

	// All WhatsApp ChatStorage references to media files (including
	// document attachments) are relative paths under Message/ in the
	// WhatsApp app-group container.
	documentManifestPrefix = "Message/"
)

// docExtExpr is the SUBSTR-after-last-dot idiom matching wa_document.ext in
// views.sql; keeping the two in lockstep avoids an "extension differs between
// FTS and indexer" drift bug. Yields a lowercase extension ('' if none).
const docExtExpr = `LOWER(CASE
		         WHEN mi.ZMEDIALOCALPATH IS NULL OR INSTR(mi.ZMEDIALOCALPATH, '.') = 0 THEN ''
		         ELSE SUBSTR(
		           mi.ZMEDIALOCALPATH,
		           LENGTH(RTRIM(mi.ZMEDIALOCALPATH, REPLACE(mi.ZMEDIALOCALPATH, '.', ''))) + 1
		         )
		       END)`

// DocumentIndexOptions configures a document download or extract run.
// Mirrors VoiceIndexOptions; see voice.go / media.go for shared semantics.
type DocumentIndexOptions struct {
	Workspace string

	// Download-phase fields (decrypt from the encrypted backup).
	BackupPath string
	BackupRoot string
	Password   string

	// Extract-phase fields (cloud). Engine must be empty or SourceCloud.
	Engine string
	APIKey string
	Model  string

	// Limit caps rows attempted this run (0 = unlimited).
	Limit int

	// RetryMissing / RetryErrors re-attempt terminal rows.
	RetryMissing bool
	RetryErrors  bool

	// Concurrency caps in-flight extract calls (0 = sensible default).
	Concurrency int

	// Force re-extracts every on-disk PDF, overwriting existing rows.
	Force bool

	Ctx           context.Context
	Log           func(string)
	Progress      func(DocumentIndexProgress)
	ProgressEvery int
}

// DocumentIndexProgress is what callers see during a run.
type DocumentIndexProgress struct {
	Done         int     `json:"done"`        // processed this run (any status)
	Total        int     `json:"total"`       // total candidates ever
	Pending      int     `json:"pending"`     // queued for this run
	Baseline     int     `json:"baseline"`    // already done before this run
	Downloaded   int     `json:"downloaded"`  // decrypted this run (download phase)
	Extracted    int     `json:"extracted"`   // text recovered this run
	Empty        int     `json:"empty"`       // ran cleanly but no text
	Missing      int     `json:"missing"`     // file absent in backup
	Errors       int     `json:"errors"`      // failures this run
	Unsupported  int     `json:"unsupported"` // non-PDF skipped
	PagesText    int     `json:"pages_text"`  // sum of pages_with_text
	PagesOCR     int     `json:"pages_ocr"`   // sum of pages_ocr
	CurrentLabel string  `json:"current_label,omitempty"`
	RatePerSec   float64 `json:"rate_per_sec"`
	ETASeconds   float64 `json:"eta_seconds"`
	ElapsedSec   float64 `json:"elapsed_sec"`
	CostUSD      float64 `json:"cost_usd"`
}

// DocumentIndexResult is the final stats summary.
type DocumentIndexResult struct {
	BackupPath        string  `json:"backup_path"`
	Workspace         string  `json:"workspace"`
	TotalCandidates   int     `json:"total_candidates"`
	AlreadyExtracted  int     `json:"already_extracted"`
	AlreadyDownloaded int     `json:"already_downloaded"`
	Processed         int     `json:"processed"`
	Downloaded        int     `json:"downloaded"`
	Extracted         int     `json:"extracted"`
	Empty             int     `json:"empty"`
	Missing           int     `json:"missing"`
	Errors            int     `json:"errors"`
	Unsupported       int     `json:"unsupported"`
	PagesText         int     `json:"pages_text"`
	PagesOCR          int     `json:"pages_ocr"`
	DurationSec       float64 `json:"duration_sec"`
	FTSCountAfter     int     `json:"fts_count_after"`
	CostUSD           float64 `json:"cost_usd,omitempty"`
	Cancelled         bool    `json:"cancelled,omitempty"`
}

// documentCandidate is one document row.
type documentCandidate struct {
	rowid        int64
	manifestPath string // 'Message/Media/.../foo.pdf'
	filename     string // ZWAMEDIAITEM.ZAUTHORNAME (sender-supplied name)
	ext          string // lowercase extension parsed from the manifest path
}

func (c documentCandidate) label() string {
	if c.rowid == 0 {
		return ""
	}
	if c.filename != "" {
		return fmt.Sprintf("rowid=%d %s", c.rowid, c.filename)
	}
	return fmt.Sprintf("rowid=%d", c.rowid)
}

// documentOneResult is downloadOneDocument/extractOneDocument's outcome.
type documentOneResult struct {
	status    string
	pagesText int
	pagesOCR  int
	withText  bool
	fatal     error
}

// pickDocumentIndexBackup honours opts.BackupPath if set, otherwise picks
// the most-recent encrypted backup under opts.BackupRoot.
func pickDocumentIndexBackup(opts DocumentIndexOptions) (backup.Info, error) {
	return pickMediaIndexBackup(MediaIndexOptions{BackupPath: opts.BackupPath, BackupRoot: opts.BackupRoot})
}

// countDocumentCandidates returns (total_document_messages, already_extracted).
// "Total" counts all ZMESSAGETYPE=8 rows with a manifest path regardless of
// extension — the GUI gauge uses this as the denominator.
func countDocumentCandidates(db *sql.DB) (total, already int, err error) {
	if err = db.QueryRow(
		`SELECT COUNT(*)
		 FROM ZWAMESSAGE m
		 JOIN ZWAMEDIAITEM mi ON mi.ZMESSAGE = m.Z_PK
		 WHERE m.ZMESSAGETYPE = 8
		   AND mi.ZMEDIALOCALPATH IS NOT NULL`,
	).Scan(&total); err != nil {
		return 0, 0, fmt.Errorf("count total: %w", err)
	}
	if err = db.QueryRow(
		`SELECT COUNT(*) FROM document_index WHERE status = ?`, DocumentStatusExtracted,
	).Scan(&already); err != nil {
		return 0, 0, fmt.Errorf("count extracted: %w", err)
	}
	return total, already, nil
}

// countDocumentDownloaded returns how many PDFs are on disk — rows
// 'downloaded' (awaiting extract) or terminal-but-present ('extracted' /
// 'extracted_empty').
func countDocumentDownloaded(db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM document_index WHERE status IN (?, ?, ?)`,
		DocumentStatusDownloaded, DocumentStatusExtracted, DocumentStatusExtractedEmpty,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count downloaded: %w", err)
	}
	return n, nil
}

// CountDocumentExtractPending returns on-disk PDFs a normal extract run (no
// force, no retry) would queue: fresh 'downloaded' rows with no prior
// extract_error. Mirrors selectDocumentExtractCandidates' non-force predicate
// so the UI can gate "Resume" on real work.
func CountDocumentExtractPending(db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM document_index WHERE status = ? AND extract_error IS NULL`,
		DocumentStatusDownloaded,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count document extract pending: %w", err)
	}
	return n, nil
}

// CountDocumentExtractFailed returns on-disk PDFs that failed a previous
// extract attempt (extract_error set, row still 'downloaded'). A normal run
// skips these; only "Retry failures" (retryErrors) re-attempts.
func CountDocumentExtractFailed(db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM document_index WHERE status = ? AND extract_error IS NOT NULL`,
		DocumentStatusDownloaded,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count document extract failed: %w", err)
	}
	return n, nil
}

// selectDocumentDownloadCandidates: every document message whose row isn't
// already in a skip status. 'downloaded'/'extracted'/'extracted_empty'/
// 'unsupported' skipped; 'missing'/'error' skipped unless the retry flag is
// set. Non-PDFs reach downloadOneDocument and are parked 'unsupported'.
func selectDocumentDownloadCandidates(db *sql.DB, retryMissing, retryErrors bool, limit int) ([]documentCandidate, error) {
	skip := []any{
		DocumentStatusDownloaded, DocumentStatusExtracted,
		DocumentStatusExtractedEmpty, DocumentStatusUnsupported,
	}
	if !retryMissing {
		skip = append(skip, DocumentStatusMissing)
	}
	if !retryErrors {
		skip = append(skip, DocumentStatusError)
	}
	where := fmt.Sprintf(
		"m.ZMESSAGETYPE = 8 AND mi.ZMEDIALOCALPATH IS NOT NULL"+
			"\n\t\t  AND m.Z_PK NOT IN (SELECT rowid FROM document_index WHERE status IN (%s))",
		placeholders(len(skip)))
	return queryDocumentCandidates(db, where, skip, limit)
}

// selectDocumentExtractCandidates: 'downloaded' rows not yet extracted
// (extract_error skipped unless retryErrors). With force, every on-disk PDF
// ('downloaded'/'extracted'/'extracted_empty') is re-extracted.
func selectDocumentExtractCandidates(db *sql.DB, retryErrors, force bool, limit int) ([]documentCandidate, error) {
	var sub string
	if force {
		sub = "SELECT rowid FROM document_index WHERE status IN ('" +
			DocumentStatusDownloaded + "','" + DocumentStatusExtracted + "','" + DocumentStatusExtractedEmpty + "')"
	} else {
		ready := "status = '" + DocumentStatusDownloaded + "'"
		if !retryErrors {
			ready += " AND extract_error IS NULL"
		}
		sub = "SELECT rowid FROM document_index WHERE " + ready
	}
	where := "m.ZMESSAGETYPE = 8 AND mi.ZMEDIALOCALPATH IS NOT NULL\n\t\t  AND m.Z_PK IN (" + sub + ")"
	return queryDocumentCandidates(db, where, nil, limit)
}

func queryDocumentCandidates(db *sql.DB, whereClause string, args []any, limit int) ([]documentCandidate, error) {
	q := fmt.Sprintf(`
		SELECT m.Z_PK,
		       '%s' || mi.ZMEDIALOCALPATH,
		       COALESCE(mi.ZAUTHORNAME, ''),
		       %s
		FROM   ZWAMESSAGE   m
		JOIN   ZWAMEDIAITEM mi ON mi.ZMESSAGE = m.Z_PK
		WHERE  %s
		ORDER BY m.Z_PK ASC`, documentManifestPrefix, docExtExpr, whereClause)
	if limit > 0 {
		q += fmt.Sprintf("\n\t\tLIMIT %d", limit)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("select document candidates: %w", err)
	}
	defer rows.Close()
	var out []documentCandidate
	for rows.Next() {
		var c documentCandidate
		if err := rows.Scan(&c.rowid, &c.manifestPath, &c.filename, &c.ext); err != nil {
			return nil, fmt.Errorf("scan document candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- Download phase --------------------------------------------------

// DownloadDocument decrypts every WhatsApp PDF into <ws>/documents/.
func DownloadDocument(opts DocumentIndexOptions) (*DocumentIndexResult, error) {
	if opts.Workspace == "" {
		return nil, errors.New("document-download: Workspace required")
	}
	if opts.Password == "" {
		return nil, errors.New("document-download: Password required for encrypted backups")
	}
	defDocOpts(&opts, 25)

	db, docDir, err := openDocumentDB(opts.Workspace)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	info, err := pickDocumentIndexBackup(opts)
	if err != nil {
		return nil, err
	}
	if !info.IsEncrypted {
		return nil, errors.New("document-download: backup is not encrypted")
	}
	opts.Log(fmt.Sprintf("Backup: %s", info.Path))
	opts.Log("Unlocking iOS backup…")
	bundle, err := backup.Open(info, opts.Password)
	if err != nil {
		return nil, fmt.Errorf("open backup: %w", err)
	}
	res := &DocumentIndexResult{BackupPath: info.Path, Workspace: opts.Workspace}
	if err := downloadDocumentWithBundle(opts, db, bundle, docDir, res); err != nil {
		return nil, err
	}
	return res, nil
}

// downloadDocumentDuringSync pulls PDFs as a step of SyncMessages, reusing the
// already-open backup bundle. Fail-soft.
func downloadDocumentDuringSync(bundle *backup.Bundle, workspace, dbPath string, log func(string)) (*DocumentIndexResult, error) {
	docDir := filepath.Join(workspace, "documents")
	if err := os.MkdirAll(docDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir documents dir: %w", err)
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	if err := ensureDocumentSidecarSchema(db); err != nil {
		return nil, err
	}
	opts := DocumentIndexOptions{
		Workspace:     workspace,
		Ctx:           context.Background(),
		Log:           log,
		ProgressEvery: 200,
		Progress: func(p DocumentIndexProgress) {
			log(fmt.Sprintf("Downloading documents… %d / %d", p.Done, p.Pending))
		},
	}
	res := &DocumentIndexResult{Workspace: workspace}
	if err := downloadDocumentWithBundle(opts, db, bundle, docDir, res); err != nil {
		return nil, err
	}
	return res, nil
}

func downloadDocumentWithBundle(opts DocumentIndexOptions, db *sql.DB, bundle *backup.Bundle, docDir string, res *DocumentIndexResult) error {
	manifestIdx := buildManifestIndex(bundle, backup.WhatsAppDomain)
	total, _, err := countDocumentCandidates(db)
	if err != nil {
		return err
	}
	alreadyDownloaded, err := countDocumentDownloaded(db)
	if err != nil {
		return err
	}
	candidates, err := selectDocumentDownloadCandidates(db, opts.RetryMissing, opts.RetryErrors, opts.Limit)
	if err != nil {
		return err
	}
	res.TotalCandidates = total
	res.AlreadyDownloaded = alreadyDownloaded
	if len(candidates) == 0 {
		opts.Log(fmt.Sprintf("Documents: %d / %d already on disk; nothing to download.", alreadyDownloaded, total))
		return nil
	}
	opts.Log(fmt.Sprintf("Documents on device: %d · on disk: %d · to download: %d",
		total, alreadyDownloaded, len(candidates)))

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = defaultDownloadConcurrency
	}
	var decryptMu sync.Mutex
	process := func(ctx context.Context, c documentCandidate) documentOneResult {
		return downloadOneDocument(ctx, db, bundle, &decryptMu, manifestIdx, docDir, c)
	}
	_ = runDocumentPipeline(opts, res, db, candidates, total, alreadyDownloaded, concurrency, nil, false, process)
	opts.Log(fmt.Sprintf("Downloaded %d documents (%d missing, %d errors, %d non-PDF) in %.0fs.",
		res.Downloaded, res.Missing, res.Errors, res.Unsupported, res.DurationSec))
	return nil
}

// downloadOneDocument decrypts one PDF, sanity-checks the %PDF- magic, writes
// it atomically, and records the outcome in document_index. Non-PDFs are
// parked 'unsupported' without any decrypt. Safe for the pool: decrypt is
// serialised behind decryptMu.
func downloadOneDocument(
	ctx context.Context,
	db *sql.DB,
	bundle *backup.Bundle,
	decryptMu *sync.Mutex,
	manifestIdx map[string]*backup.Record,
	docDir string,
	c documentCandidate,
) documentOneResult {
	now := nowUTC()
	if ctx.Err() != nil {
		return documentOneResult{status: statusCancelled}
	}

	// Long-tail formats (xlsx, docx, …) — no extractor; park without decrypt.
	if c.ext != "pdf" {
		writeDocumentIndex(db, c, DocumentStatusUnsupported, 0, 0, "non-PDF format", now)
		return documentOneResult{status: DocumentStatusUnsupported}
	}

	rec, ok := manifestIdx[c.manifestPath]
	if !ok {
		writeDocumentIndex(db, c, DocumentStatusMissing, 0, 0, "", now)
		return documentOneResult{status: DocumentStatusMissing}
	}

	decryptMu.Lock()
	rd, err := bundle.FileReader(*rec)
	if err != nil {
		decryptMu.Unlock()
		if errors.Is(err, io.EOF) {
			writeDocumentIndex(db, c, DocumentStatusMissing, 0, 0, "", now)
			return documentOneResult{status: DocumentStatusMissing}
		}
		writeDocumentIndex(db, c, DocumentStatusError, 0, 0, fmt.Sprintf("decrypt: %v", err), now)
		return documentOneResult{status: DocumentStatusError}
	}
	data, err := io.ReadAll(rd)
	_ = rd.Close()
	decryptMu.Unlock()
	if err != nil {
		if errors.Is(err, io.EOF) {
			writeDocumentIndex(db, c, DocumentStatusMissing, 0, 0, "", now)
			return documentOneResult{status: DocumentStatusMissing}
		}
		writeDocumentIndex(db, c, DocumentStatusError, int64(len(data)), 0, fmt.Sprintf("read: %v", err), now)
		return documentOneResult{status: DocumentStatusError}
	}
	if len(data) < 5 || string(data[:5]) != "%PDF-" {
		writeDocumentIndex(db, c, DocumentStatusError, int64(len(data)), 0, "not a PDF (bad magic)", now)
		return documentOneResult{status: DocumentStatusError}
	}

	out := filepath.Join(docDir, fmt.Sprintf("%d.pdf", c.rowid))
	if err := writeFileAtomic(out, data); err != nil {
		writeDocumentIndex(db, c, DocumentStatusError, int64(len(data)), 0, fmt.Sprintf("write: %v", err), now)
		return documentOneResult{status: DocumentStatusError}
	}
	writeDocumentIndex(db, c, DocumentStatusDownloaded, int64(len(data)), 0, "", now)
	return documentOneResult{status: DocumentStatusDownloaded}
}

// ---- Extract phase ---------------------------------------------------

// DocumentIndex extracts text from already-downloaded PDFs via the cloud.
// No backup password — it reads documents/ and calls OpenRouter's file-parser
// plugin. Needs an API key. Resumable per-row.
func DocumentIndex(opts DocumentIndexOptions) (*DocumentIndexResult, error) {
	if opts.Workspace == "" {
		return nil, errors.New("document-index: Workspace required")
	}
	defDocOpts(&opts, 5)

	db, docDir, err := openDocumentDB(opts.Workspace)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// Re-adopt on-disk PDFs missing from the index (recovered after a
	// re-sync rebuilt ChatStorage.sqlite). Same rationale as voice.
	if n, rErr := reconcileDocumentIndexFromDisk(db, docDir); rErr != nil {
		return nil, rErr
	} else if n > 0 {
		opts.Log(fmt.Sprintf("Re-adopted %d on-disk document(s) missing from the index (recovered after a re-sync).", n))
	}

	total, alreadyExtracted, err := countDocumentCandidates(db)
	if err != nil {
		return nil, err
	}
	downloaded, err := countDocumentDownloaded(db)
	if err != nil {
		return nil, err
	}
	candidates, err := selectDocumentExtractCandidates(db, opts.RetryErrors, opts.Force, opts.Limit)
	if err != nil {
		return nil, err
	}
	res := &DocumentIndexResult{Workspace: opts.Workspace, TotalCandidates: total, AlreadyExtracted: alreadyExtracted}
	if len(candidates) == 0 {
		opts.Log(fmt.Sprintf("Nothing to extract: %d / %d downloaded PDFs already extracted.", alreadyExtracted, downloaded))
		ftsN, _ := rebuildFTS(db)
		res.FTSCountAfter = ftsN
		return res, nil
	}
	opts.Log(fmt.Sprintf("Documents downloaded: %d", downloaded))
	opts.Log(fmt.Sprintf("Already extracted:    %d", alreadyExtracted))
	opts.Log(fmt.Sprintf("Queued this run:      %d", len(candidates)))

	extractor, err := documentExtractors.build(opts.Engine, opts.APIKey, opts.Model)
	if err != nil {
		return nil, err
	}
	concurrency := resolveConcurrency(opts.Concurrency)
	opts.Log(fmt.Sprintf("Extractor: cloud (%s) (concurrency %d)", extractor.Model(), concurrency))

	baseline := alreadyExtracted
	process := func(ctx context.Context, c documentCandidate) documentOneResult {
		return extractOneDocument(ctx, db, extractor, docDir, c, opts.Log)
	}
	fatalErr := runDocumentPipeline(opts, res, db, candidates, total, baseline, concurrency,
		func() float64 { return extractor.CostUSD() }, true, process)

	opts.Log(fmt.Sprintf("Done in %.0fs. extracted=%d empty=%d errors=%d ($%.4f)",
		res.DurationSec, res.Extracted, res.Empty, res.Errors, res.CostUSD))
	if fatalErr != nil {
		return res, fmt.Errorf("extract run aborted after %d documents: %w", res.Extracted, fatalErr)
	}
	return res, nil
}

// extractOneDocument reads one PDF off disk and extracts its text via the
// cloud, writing wa_document_text and flipping document_index to 'extracted'
// (or 'extracted_empty' for a clean-but-empty result). A per-doc failure
// keeps status='downloaded' and records the reason in extract_error so the
// file isn't re-downloaded.
func extractOneDocument(
	ctx context.Context,
	db *sql.DB,
	extractor *cloudDocumentExtractor,
	docDir string,
	c documentCandidate,
	log func(string),
) documentOneResult {
	now := nowUTC()
	path := filepath.Join(docDir, fmt.Sprintf("%d.pdf", c.rowid))
	data, err := os.ReadFile(path)
	if err != nil {
		setDocumentExtractError(db, c.rowid, "file missing on disk at extract time", now)
		return documentOneResult{status: DocumentStatusError}
	}

	ext, err := extractor.Extract(ctx, data, documentFilename(c))
	if err != nil {
		var fatal *FatalError
		if errors.As(err, &fatal) {
			return documentOneResult{status: statusCancelled, fatal: err}
		}
		if ctx.Err() != nil {
			return documentOneResult{status: statusCancelled}
		}
		setDocumentExtractError(db, c.rowid, fmt.Sprintf("extract: %v", err), now)
		return documentOneResult{status: DocumentStatusError}
	}

	text := strings.TrimSpace(ext.Text)
	if text == "" {
		// Ran cleanly but no recoverable text. Flip to 'extracted_empty',
		// clearing any prior extract_error; no wa_document_text row.
		if _, e := db.Exec(
			`UPDATE document_index SET status = ?, page_count = ?, extract_error = NULL, attempted_at = ? WHERE rowid = ?`,
			DocumentStatusExtractedEmpty, nullableInt(ext.PageCount), now, c.rowid,
		); e != nil {
			log(fmt.Sprintf("[rowid=%d] update document_index (empty): %v", c.rowid, e))
			return documentOneResult{status: DocumentStatusError}
		}
		return documentOneResult{status: DocumentStatusExtractedEmpty}
	}

	tx, err := db.Begin()
	if err != nil {
		log(fmt.Sprintf("[rowid=%d] begin tx: %v", c.rowid, err))
		return documentOneResult{status: DocumentStatusError}
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO wa_document_text
		 (rowid, text, page_count, pages_with_text, pages_ocr, method, model, generated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.rowid, text, ext.PageCount, ext.PagesText, ext.PagesOCR, ext.Method, extractor.Model(), now,
	); err != nil {
		_ = tx.Rollback()
		log(fmt.Sprintf("[rowid=%d] insert wa_document_text: %v", c.rowid, err))
		return documentOneResult{status: DocumentStatusError}
	}
	if _, err := tx.Exec(
		`UPDATE document_index SET status = ?, page_count = ?, extract_error = NULL, attempted_at = ? WHERE rowid = ?`,
		DocumentStatusExtracted, nullableInt(ext.PageCount), now, c.rowid,
	); err != nil {
		_ = tx.Rollback()
		log(fmt.Sprintf("[rowid=%d] update document_index: %v", c.rowid, err))
		return documentOneResult{status: DocumentStatusError}
	}
	if err := tx.Commit(); err != nil {
		log(fmt.Sprintf("[rowid=%d] commit: %v", c.rowid, err))
		return documentOneResult{status: DocumentStatusError}
	}
	return documentOneResult{status: DocumentStatusExtracted, pagesText: ext.PagesText, pagesOCR: ext.PagesOCR, withText: true}
}

// documentFilename is the sender-supplied name when present (so the file-parser
// sees a real ".pdf" name), else a synthetic one from the rowid.
func documentFilename(c documentCandidate) string {
	if c.filename != "" {
		return c.filename
	}
	return fmt.Sprintf("%d.pdf", c.rowid)
}

// ---- Shared worker pool ----------------------------------------------

// runDocumentPipeline drives the document download/extract phases — a thin
// per-phase wrapper over the generic runWorkerPipeline (pipeline.go), supplying
// the document-specific tally, progress emission, and FTS rebuild. Mirrors
// runVoicePipeline.
func runDocumentPipeline(
	opts DocumentIndexOptions,
	res *DocumentIndexResult,
	db *sql.DB,
	candidates []documentCandidate,
	total, baseline, concurrency int,
	costFn func() float64,
	rebuildFTSAfter bool,
	process func(ctx context.Context, c documentCandidate) documentOneResult,
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
		func(r documentOneResult) (string, error) { return r.status, r.fatal },
		nil,
		func(r documentOneResult) {
			switch r.status {
			case DocumentStatusDownloaded:
				res.Downloaded++
			case DocumentStatusExtracted:
				res.Extracted++
				res.PagesText += r.pagesText
				res.PagesOCR += r.pagesOCR
			case DocumentStatusExtractedEmpty:
				res.Empty++
			case DocumentStatusMissing:
				res.Missing++
			case DocumentStatusUnsupported:
				res.Unsupported++
			case DocumentStatusError:
				res.Errors++
			}
			res.Processed++
		},
		func() {
			res.CostUSD = cost()
			emitDocumentProgress(opts.Progress, res, total, baseline, len(candidates), tStart)
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

func defDocOpts(opts *DocumentIndexOptions, progressEvery int) {
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

func openDocumentDB(workspace string) (*sql.DB, string, error) {
	docDir := filepath.Join(workspace, "documents")
	if err := os.MkdirAll(docDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("mkdir documents dir: %w", err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(workspace, "ChatStorage.sqlite"))
	if err != nil {
		return nil, "", fmt.Errorf("open db: %w", err)
	}
	if err := ensureDocumentSidecarSchema(db); err != nil {
		_ = db.Close()
		return nil, "", err
	}
	return db, docDir, nil
}

// reconcileDocumentIndexFromDisk adopts orphaned documents/<rowid>.pdf files
// into document_index as 'downloaded' rows. A messages re-sync rebuilds
// ChatStorage.sqlite — and with it document_index — from the backup, leaving
// the on-disk documents/ untouched. If the carry-forward dropped those rows,
// every PDF is present on disk yet invisible to the extract queue. Re-create a
// 'downloaded' row for each on-disk PDF mapping to a real document message.
// Mirrors reconcileVoiceIndexFromDisk. Returns the number of rows adopted.
func reconcileDocumentIndexFromDisk(db *sql.DB, docDir string) (int, error) {
	entries, err := os.ReadDir(docDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read documents dir: %w", err)
	}

	known := map[int64]bool{}
	rows, err := db.Query(`SELECT rowid FROM document_index`)
	if err != nil {
		return 0, fmt.Errorf("load document_index rowids: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan document_index rowid: %w", err)
		}
		known[id] = true
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate document_index rowids: %w", err)
	}

	now := nowUTC()
	adopted := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pdf") {
			continue
		}
		rowid, err := strconv.ParseInt(strings.TrimSuffix(e.Name(), ".pdf"), 10, 64)
		if err != nil || known[rowid] {
			continue
		}
		var c documentCandidate
		err = db.QueryRow(fmt.Sprintf(
			`SELECT m.Z_PK, ? || mi.ZMEDIALOCALPATH, COALESCE(mi.ZAUTHORNAME, ''), %s
			   FROM ZWAMESSAGE   m
			   JOIN ZWAMEDIAITEM mi ON mi.ZMESSAGE = m.Z_PK
			  WHERE m.Z_PK = ? AND m.ZMESSAGETYPE = 8 AND mi.ZMEDIALOCALPATH IS NOT NULL`, docExtExpr),
			documentManifestPrefix, rowid,
		).Scan(&c.rowid, &c.manifestPath, &c.filename, &c.ext)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return adopted, fmt.Errorf("look up document message %d: %w", rowid, err)
		}
		var bytesLen int64
		if info, statErr := e.Info(); statErr == nil {
			bytesLen = info.Size()
		}
		writeDocumentIndex(db, c, DocumentStatusDownloaded, bytesLen, 0, "", now)
		adopted++
	}
	return adopted, nil
}

// writeDocumentIndex commits a single document_index row (terminal download
// outcomes). INSERT OR REPLACE resets extract_error to NULL.
func writeDocumentIndex(db *sql.DB, c documentCandidate, status string, bytesLen int64, pageCount int, errMsg, now string) {
	var errVal any
	if errMsg != "" {
		errVal = errMsg
	}
	_, _ = db.Exec(
		`INSERT OR REPLACE INTO document_index
		 (rowid, manifest_path, ext, status, bytes, page_count, error, extract_error, attempted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?)`,
		c.rowid, c.manifestPath, c.ext, status, bytesLen, nullableInt(pageCount), errVal, now,
	)
}

// setDocumentExtractError records a per-doc extract failure without changing
// status (the file stays on disk, not re-downloaded).
func setDocumentExtractError(db *sql.DB, rowid int64, msg, now string) {
	_, _ = db.Exec(
		`UPDATE document_index SET extract_error = ?, attempted_at = ? WHERE rowid = ?`,
		msg, now, rowid,
	)
}

// nullableInt stores a positive count, NULL for zero/unknown (keeps the
// page_count column meaningfully sparse).
func nullableInt(n int) any {
	if n > 0 {
		return n
	}
	return nil
}

func emitDocumentProgress(cb func(DocumentIndexProgress), res *DocumentIndexResult, total, baseline, pending int, started time.Time) {
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
	cb(DocumentIndexProgress{
		Done:        res.Processed,
		Total:       total,
		Pending:     pending,
		Baseline:    baseline,
		Downloaded:  res.Downloaded,
		Extracted:   res.Extracted,
		Empty:       res.Empty,
		Missing:     res.Missing,
		Errors:      res.Errors,
		Unsupported: res.Unsupported,
		PagesText:   res.PagesText,
		PagesOCR:    res.PagesOCR,
		RatePerSec:  rate,
		ETASeconds:  eta,
		ElapsedSec:  elapsed,
		CostUSD:     res.CostUSD,
	})
}
