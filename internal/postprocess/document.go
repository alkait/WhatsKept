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
	"time"

	"whatskept/internal/backup"
)

// This file owns one user-visible operation:
//
//   `whatskept document-index` (CLI) / "Sync documents" (GUI):
//   walk every WhatsApp PDF document in ChatStorage.sqlite, decrypt
//   the PDF from the iOS backup, extract its body text via Apple
//   PDFKit (with a Vision OCR fallback per scanned page), and
//   persist the results in wa_document_text + document_index.
//
// Pipeline per document (PDF only — see "Why PDF only" below):
//
//   1. Look up the manifest record by ZMEDIALOCALPATH (with the
//      "Message/" prefix all WhatsApp media uses).
//   2. Decrypt to memory. WhatsApp PDFs are small in the median
//      case (716 KB) and bounded at ~45 MB; no streaming needed.
//   3. Sniff "%PDF-" magic. Anything else is recorded as 'error'
//      rather than handed to PDFKit (which would fail with a
//      generic "could not open document" message).
//   4. Write to <Workspace>/documents/<rowid>.pdf atomically.
//   5. Invoke the bundled Swift helper with {kind:"pdf", path:…}.
//      The helper does the actual PDFKit-then-OCR work and returns
//      {text, page_count, pages_with_text, pages_ocr, method}.
//   6. Atomic two-table commit: wa_document_text (the body text)
//      and document_index (terminal state for resume).
//   7. After the loop, rebuild messages_fts so the new text is
//      MATCHable.
//
// Why PDF only? Roughly 84% of WhatsApp documents in a heavy user's
// corpus are PDFs; xlsx/docx/etc. are a long tail and need different
// extractors (textutil / unzip+XPath). We park those as status=
// 'unsupported' so resume logic is cheap and a future expansion
// only has to flip the status check, not redo the indexer's bones.
// See wa_document for the bare filename FTS coverage of the
// long tail (which views.sql rebuilds on every sync).

const (
	// Status values stored in document_index.status. See
	// createDocumentSidecarsSQL for the closed set.
	DocumentStatusExtracted      = "extracted"
	DocumentStatusExtractedEmpty = "extracted_empty"
	DocumentStatusMissing        = "missing"
	DocumentStatusUnsupported    = "unsupported"
	DocumentStatusError          = "error"

	// All WhatsApp ChatStorage references to media files (including
	// document attachments) are relative paths under Message/ in the
	// WhatsApp app-group container.
	documentManifestPrefix = "Message/"
)

// DocumentIndexOptions configures one DocumentIndex run. Mirrors
// MediaIndexOptions / VoiceIndexOptions for cross-call symmetry —
// see media.go for the shared field semantics.
type DocumentIndexOptions struct {
	// Workspace is the directory containing ChatStorage.sqlite.
	// Decrypted PDFs land in <Workspace>/documents/<rowid>.pdf.
	Workspace string

	// BackupPath / BackupRoot — optional override and search root
	// for backup discovery; identical semantics to MediaIndex.
	BackupPath string
	BackupRoot string

	// Password to unlock the encrypted backup. Required.
	Password string

	// Limit caps the number of rows attempted in this run. 0 =
	// unlimited.
	Limit int

	// RetryMissing / RetryErrors — re-attempt rows previously
	// marked terminal. Same semantics as media-index. Note that
	// 'unsupported' rows are always skipped (no retry flag) — we
	// don't have an extractor for them yet, so attempting again
	// would just produce the same result.
	RetryMissing bool
	RetryErrors  bool

	// MaxOCRPages / RenderScale override the Swift helper's PDF
	// tunables (WHATSKEPT_PDF_MAX_OCR_PAGES / _RENDER_SCALE). Zero
	// means "use helper default" (100 pages / 2.0× scale).
	MaxOCRPages int
	RenderScale float32

	// Ctx is checked between rows for graceful cancellation. Same
	// rules as the other indexers: closing the helper's stdin
	// causes it to exit cleanly; the current row's commit either
	// finishes or rolls back.
	Ctx context.Context

	// Log receives one-line human-readable progress lines.
	Log func(string)

	// Progress receives a structured update every ProgressEvery
	// rows. ProgressEvery=0 → default of 5 rows (documents are slow
	// enough that batching makes the UI feel sluggish; not as slow
	// as voice though, so 5 strikes the balance).
	Progress      func(DocumentIndexProgress)
	ProgressEvery int
}

// DocumentIndexProgress is what callers see during a run.
type DocumentIndexProgress struct {
	Done         int     `json:"done"`        // processed this run (any status)
	Total        int     `json:"total"`       // total candidates ever
	Pending      int     `json:"pending"`     // queued for this run
	Extracted    int     `json:"extracted"`   // text recovered (any text >0 chars)
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
}

// DocumentIndexResult is the final stats summary.
type DocumentIndexResult struct {
	BackupPath       string  `json:"backup_path"`
	Workspace        string  `json:"workspace"`
	TotalCandidates  int     `json:"total_candidates"`
	AlreadyExtracted int     `json:"already_extracted"`
	Processed        int     `json:"processed"`
	Extracted        int     `json:"extracted"`
	Empty            int     `json:"empty"`
	Missing          int     `json:"missing"`
	Errors           int     `json:"errors"`
	Unsupported      int     `json:"unsupported"`
	PagesText        int     `json:"pages_text"`
	PagesOCR         int     `json:"pages_ocr"`
	DurationSec      float64 `json:"duration_sec"`
	FTSCountAfter    int     `json:"fts_count_after"`
	Cancelled        bool    `json:"cancelled,omitempty"`
}

// DocumentIndex runs the full PDF-extraction pipeline against the
// workspace. See file-level comment for the full design notes.
func DocumentIndex(opts DocumentIndexOptions) (*DocumentIndexResult, error) {
	if opts.Workspace == "" {
		return nil, errors.New("document-index: Workspace required")
	}
	if opts.Password == "" {
		return nil, errors.New("document-index: Password required for encrypted backups")
	}
	if opts.Ctx == nil {
		opts.Ctx = context.Background()
	}
	if opts.Log == nil {
		opts.Log = func(string) {}
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = 5
	}

	dbPath := filepath.Join(opts.Workspace, "ChatStorage.sqlite")
	docDir := filepath.Join(opts.Workspace, "documents")
	if err := os.MkdirAll(docDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir documents dir: %w", err)
	}

	// --- Open DB + ensure schema --------------------------------
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec(createDocumentSidecarsSQL); err != nil {
		return nil, fmt.Errorf("create document sidecar tables: %w", err)
	}

	// --- Locate and unlock the backup ---------------------------
	info, err := pickDocumentIndexBackup(opts)
	if err != nil {
		return nil, err
	}
	if !info.IsEncrypted {
		return nil, errors.New("document-index: backup is not encrypted (whatskept document-index requires an encrypted backup)")
	}
	opts.Log(fmt.Sprintf("Backup: %s", info.Path))
	opts.Log("Unlocking iOS backup…")
	bundle, err := backup.Open(info, opts.Password)
	if err != nil {
		return nil, fmt.Errorf("open backup: %w", err)
	}

	opts.Log("Indexing backup manifest…")
	manifestIdx := buildManifestIndex(bundle, backup.WhatsAppDomain)

	// --- Pick candidates ---------------------------------------
	total, already, err := countDocumentCandidates(db)
	if err != nil {
		return nil, err
	}
	candidates, err := selectDocumentCandidates(db, opts.RetryMissing, opts.RetryErrors, opts.Limit)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		opts.Log(fmt.Sprintf("Nothing to do: %d / %d already extracted.", already, total))
		ftsN, _ := rebuildFTS(db)
		return &DocumentIndexResult{
			BackupPath:       info.Path,
			Workspace:        opts.Workspace,
			TotalCandidates:  total,
			AlreadyExtracted: already,
			FTSCountAfter:    ftsN,
		}, nil
	}
	opts.Log(fmt.Sprintf("Documents on device:      %d", total))
	opts.Log(fmt.Sprintf("Already extracted:        %d", already))
	opts.Log(fmt.Sprintf("Queued this run:          %d", len(candidates)))

	// --- Start the Swift PDF worker -----------------------------
	helperPath, err := resolveVisionHelper()
	if err != nil {
		return nil, fmt.Errorf("locate vision helper: %w", err)
	}
	worker, err := startPDFWorker(opts.Ctx, helperPath, opts.MaxOCRPages, opts.RenderScale)
	if err != nil {
		return nil, fmt.Errorf("start vision helper: %w", err)
	}
	defer worker.Close()

	// --- Main loop ---------------------------------------------
	res := &DocumentIndexResult{
		BackupPath:       info.Path,
		Workspace:        opts.Workspace,
		TotalCandidates:  total,
		AlreadyExtracted: already,
	}
	tStart := time.Now()

	for i, c := range candidates {
		select {
		case <-opts.Ctx.Done():
			res.Cancelled = true
			opts.Log("Stopped on user request. All committed rows are safe; re-run to resume.")
			res.DurationSec = time.Since(tStart).Seconds()
			return res, nil
		default:
		}

		status, pagesText, pagesOCR := processOneDocument(db, bundle, manifestIdx, worker, docDir, c, opts.Log)
		res.Processed++
		switch status {
		case DocumentStatusExtracted:
			res.Extracted++
			res.PagesText += pagesText
			res.PagesOCR += pagesOCR
		case DocumentStatusExtractedEmpty:
			res.Empty++
		case DocumentStatusMissing:
			res.Missing++
		case DocumentStatusUnsupported:
			res.Unsupported++
		case DocumentStatusError:
			res.Errors++
		}

		if opts.Progress != nil && (i+1)%opts.ProgressEvery == 0 {
			emitDocumentProgress(opts.Progress, res, total, already, len(candidates), tStart, c)
		}
	}

	res.DurationSec = time.Since(tStart).Seconds()
	if opts.Progress != nil {
		emitDocumentProgress(opts.Progress, res, total, already, len(candidates), tStart, documentCandidate{})
	}

	opts.Log(fmt.Sprintf(
		"Done in %.0fs. extracted=%d empty=%d missing=%d errors=%d unsupported=%d (rate %.1f/s)",
		res.DurationSec, res.Extracted, res.Empty, res.Missing, res.Errors, res.Unsupported,
		float64(res.Processed)/maxF(res.DurationSec, 0.001),
	))

	// Rebuild FTS so the new wa_document_text rows become searchable.
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

// pickDocumentIndexBackup honours opts.BackupPath if set, otherwise
// picks the most-recent encrypted backup under opts.BackupRoot.
// Mirrors pickMediaIndexBackup / pickVoiceIndexBackup; kept separate
// so future divergence in any path doesn't force an awkward refactor.
func pickDocumentIndexBackup(opts DocumentIndexOptions) (backup.Info, error) {
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

// documentCandidate is one row we'll attempt to extract.
type documentCandidate struct {
	rowid        int64
	manifestPath string // 'Message/Media/.../foo.pdf'
	filename     string // ZWAMEDIAITEM.ZAUTHORNAME (the sender-supplied original name)
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

// countDocumentCandidates returns (total_document_messages,
// already_extracted). "Total" counts all ZMESSAGETYPE=8 rows
// regardless of extension — the GUI's progress gauge uses this as
// the denominator so the user sees "1,558 / 1,930" rather than
// pretending the long tail doesn't exist.
func countDocumentCandidates(db *sql.DB) (total, already int, err error) {
	// Rows with NULL ZMEDIALOCALPATH are unreachable (no manifest path
	// to decrypt) and excluded from the addressable total. The GUI
	// gauge denominator (document_pdf_total) already matches this
	// filter via the wa_document view.
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
		`SELECT COUNT(*) FROM document_index WHERE status = ?`,
		DocumentStatusExtracted,
	).Scan(&already); err != nil {
		return 0, 0, fmt.Errorf("count extracted: %w", err)
	}
	return total, already, nil
}

// selectDocumentCandidates is the resume query. Rows already in
// document_index with a "skip" status are excluded. 'unsupported'
// is always skipped (re-attempting it with no extractor change
// just produces the same result), but the user can flush it via
// `DELETE FROM document_index WHERE status='unsupported'` if a
// future build adds support.
func selectDocumentCandidates(db *sql.DB, retryMissing, retryErrors bool, limit int) ([]documentCandidate, error) {
	skip := []any{
		DocumentStatusExtracted,
		DocumentStatusExtractedEmpty,
		DocumentStatusUnsupported,
	}
	if !retryMissing {
		skip = append(skip, DocumentStatusMissing)
	}
	if !retryErrors {
		skip = append(skip, DocumentStatusError)
	}
	placeholders := make([]byte, 0, len(skip)*2)
	for i := range skip {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
	}

	// The SUBSTR-after-last-dot idiom matches the one in views.sql
	// for wa_document.ext; keeping the two in lockstep avoids a
	// subtle "extension differs between FTS and indexer" drift bug.
	q := fmt.Sprintf(`
		SELECT m.Z_PK,
		       '%s' || mi.ZMEDIALOCALPATH,
		       COALESCE(mi.ZAUTHORNAME, ''),
		       LOWER(CASE
		         WHEN mi.ZMEDIALOCALPATH IS NULL OR INSTR(mi.ZMEDIALOCALPATH, '.') = 0 THEN ''
		         ELSE SUBSTR(
		           mi.ZMEDIALOCALPATH,
		           LENGTH(RTRIM(mi.ZMEDIALOCALPATH, REPLACE(mi.ZMEDIALOCALPATH, '.', ''))) + 1
		         )
		       END)
		FROM   ZWAMESSAGE   m
		JOIN   ZWAMEDIAITEM mi ON mi.ZMESSAGE = m.Z_PK
		WHERE  m.ZMESSAGETYPE = 8
		  AND  mi.ZMEDIALOCALPATH IS NOT NULL
		  AND  m.Z_PK NOT IN (
		         SELECT rowid FROM document_index WHERE status IN (%s)
		       )
		ORDER BY m.Z_PK ASC`,
		documentManifestPrefix, string(placeholders))
	if limit > 0 {
		q += fmt.Sprintf("\n\t\tLIMIT %d", limit)
	}

	rows, err := db.Query(q, skip...)
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
	return out, nil
}

// processOneDocument runs the full decrypt → write → extract →
// commit pipeline for one candidate. Returns
// (status, pages_with_text, pages_ocr) — non-Extracted statuses
// return (status, 0, 0).
//
// Per-row commit means a SIGINT mid-loop is safe.
func processOneDocument(
	db *sql.DB,
	bundle *backup.Bundle,
	manifestIdx map[string]*backup.Record,
	worker *pdfWorker,
	docDir string,
	c documentCandidate,
	log func(string),
) (string, int, int) {
	now := nowUTC()

	// Long-tail formats (xlsx, docx, html, etc.) are recorded as
	// 'unsupported' without any decrypt/write — there's no
	// extractor to run. The bare filename is still in wa_document
	// (and thus messages_fts), so "did Khalid send me that
	// budget.xlsx" still works.
	if c.ext != "pdf" {
		writeDocumentIndex(db, c, DocumentStatusUnsupported, 0, 0, "non-PDF format", now)
		return DocumentStatusUnsupported, 0, 0
	}

	// 1. Find the file in the manifest.
	rec, ok := manifestIdx[c.manifestPath]
	if !ok {
		writeDocumentIndex(db, c, DocumentStatusMissing, 0, 0, "", now)
		return DocumentStatusMissing, 0, 0
	}

	// 2. Decrypt to memory. EOF gets its own bucket — same logic
	//    as the image and voice indexers: the manifest references
	//    the blob but iOS didn't actually persist its bytes
	//    (selective backup, or the document was never opened on
	//    the phone and got evicted). User-visible that's identical
	//    to "missing from manifest", so reclassify as missing.
	rd, err := bundle.FileReader(*rec)
	if err != nil {
		if errors.Is(err, io.EOF) {
			writeDocumentIndex(db, c, DocumentStatusMissing, 0, 0, "", now)
			return DocumentStatusMissing, 0, 0
		}
		writeDocumentIndex(db, c, DocumentStatusError, 0, 0,
			fmt.Sprintf("decrypt: %v", err), now)
		return DocumentStatusError, 0, 0
	}
	data, err := io.ReadAll(rd)
	_ = rd.Close()
	if err != nil {
		if errors.Is(err, io.EOF) {
			writeDocumentIndex(db, c, DocumentStatusMissing, 0, 0, "", now)
			return DocumentStatusMissing, 0, 0
		}
		writeDocumentIndex(db, c, DocumentStatusError, int64(len(data)), 0,
			fmt.Sprintf("read: %v", err), now)
		return DocumentStatusError, 0, 0
	}

	// 3. Magic-byte check. Anything not starting with "%PDF-" goes
	//    to error — handing PDFKit a non-PDF would just give us
	//    "could not open document" with no signal about what
	//    actually went wrong.
	if len(data) < 5 || string(data[:5]) != "%PDF-" {
		writeDocumentIndex(db, c, DocumentStatusError, int64(len(data)), 0,
			"not a PDF (bad magic)", now)
		return DocumentStatusError, 0, 0
	}

	// 4. Write to disk atomically (tmp+rename). Same rationale as
	//    media.go: an interrupted write must not leave a half-file
	//    that future runs would barf on.
	out := filepath.Join(docDir, fmt.Sprintf("%d.pdf", c.rowid))
	if err := writeFileAtomic(out, data); err != nil {
		writeDocumentIndex(db, c, DocumentStatusError, int64(len(data)), 0,
			fmt.Sprintf("write: %v", err), now)
		return DocumentStatusError, 0, 0
	}

	// 5. PDFKit + Vision via the Swift helper.
	resp, err := worker.extract(c.rowid, out)
	if err != nil {
		writeDocumentIndex(db, c, DocumentStatusError, int64(len(data)), 0,
			fmt.Sprintf("pdfkit: %v", err), now)
		return DocumentStatusError, 0, 0
	}
	if !resp.OK {
		writeDocumentIndex(db, c, DocumentStatusError, int64(len(data)), 0,
			fmt.Sprintf("pdfkit: %s", resp.Error), now)
		return DocumentStatusError, 0, 0
	}

	text := strings.TrimSpace(resp.Text)
	if text == "" {
		// PDFKit opened it but found nothing extractable (and OCR
		// also found nothing). The file is on disk, the row is
		// terminal-but-clean, and a future re-run shouldn't bother
		// repeating the work.
		writeDocumentIndex(db, c, DocumentStatusExtractedEmpty,
			int64(len(data)), resp.PageCount, "", now)
		return DocumentStatusExtractedEmpty, 0, 0
	}

	// 6. Persist results. Two-table atomic commit (wa_document_text
	//    and document_index together) — same pattern as media.go.
	//    INSERT OR REPLACE so a --retry-errors run cleanly overwrites.
	tx, err := db.Begin()
	if err != nil {
		log(fmt.Sprintf("[rowid=%d] begin tx: %v", c.rowid, err))
		return DocumentStatusError, 0, 0
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO wa_document_text
		 (rowid, text, page_count, pages_with_text, pages_ocr, method, generated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.rowid, text, resp.PageCount, resp.PagesWithText, resp.PagesOCR, resp.Method, now,
	); err != nil {
		_ = tx.Rollback()
		log(fmt.Sprintf("[rowid=%d] insert wa_document_text: %v", c.rowid, err))
		return DocumentStatusError, 0, 0
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO document_index
		 (rowid, manifest_path, ext, status, bytes, page_count, error, attempted_at)
		 VALUES (?, ?, ?, ?, ?, ?, NULL, ?)`,
		c.rowid, c.manifestPath, c.ext, DocumentStatusExtracted,
		int64(len(data)), resp.PageCount, now,
	); err != nil {
		_ = tx.Rollback()
		log(fmt.Sprintf("[rowid=%d] insert document_index: %v", c.rowid, err))
		return DocumentStatusError, 0, 0
	}
	if err := tx.Commit(); err != nil {
		log(fmt.Sprintf("[rowid=%d] commit: %v", c.rowid, err))
		return DocumentStatusError, 0, 0
	}
	return DocumentStatusExtracted, resp.PagesWithText, resp.PagesOCR
}

// writeDocumentIndex commits a single document_index row outside
// any caller-managed transaction. Used for terminal-status writes
// (missing / error / unsupported / extracted_empty) where there's
// no wa_document_text row to keep atomic with.
func writeDocumentIndex(db *sql.DB, c documentCandidate, status string, bytesLen int64, pageCount int, errMsg string, now string) {
	var errVal any
	if errMsg != "" {
		errVal = errMsg
	}
	var pcVal any
	if pageCount > 0 {
		pcVal = pageCount
	}
	_, _ = db.Exec(
		`INSERT OR REPLACE INTO document_index
		 (rowid, manifest_path, ext, status, bytes, page_count, error, attempted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.rowid, c.manifestPath, c.ext, status, bytesLen, pcVal, errVal, now,
	)
}

func emitDocumentProgress(cb func(DocumentIndexProgress), res *DocumentIndexResult, total, already, pending int, started time.Time, current documentCandidate) {
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
		Done:         res.Processed,
		Total:        total,
		Pending:      pending,
		Extracted:    res.Extracted,
		Empty:        res.Empty,
		Missing:      res.Missing,
		Errors:       res.Errors,
		Unsupported:  res.Unsupported,
		PagesText:    res.PagesText,
		PagesOCR:     res.PagesOCR,
		CurrentLabel: current.label(),
		RatePerSec:   rate,
		ETASeconds:   eta,
		ElapsedSec:   elapsed,
	})
}

// -------------------------------------------------------------------
// PDF worker subprocess client
// -------------------------------------------------------------------

// pdfWorker is the request-response client for the Swift Vision
// helper's PDF mode. Synchronous, one PDF in flight at a time, with
// request/response types matching the helper's `kind:"pdf"` wire
// contract. document-index spawns its own helper process so it has a
// clean shutdown semantic per run.
type pdfWorker struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Reader
	stderr   io.ReadCloser
	closeErr error
}

type pdfRequest struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	Path string `json:"path"`
}

type pdfResponse struct {
	ID            int64  `json:"id"`
	OK            bool   `json:"ok"`
	Text          string `json:"text"`
	PageCount     int    `json:"page_count"`
	PagesWithText int    `json:"pages_with_text"`
	PagesOCR      int    `json:"pages_ocr"`
	Method        string `json:"method"`
	Error         string `json:"error"`
}

// startPDFWorker spawns the bundled Swift helper with PDF tunables
// pre-baked into its env. Pass maxOCRPages=0 / renderScale=0 to
// inherit the helper's own defaults (100 pages, 2.0× scale).
func startPDFWorker(ctx context.Context, path string, maxOCRPages int, renderScale float32) (*pdfWorker, error) {
	cmd := exec.CommandContext(ctx, path)
	env := os.Environ()
	if maxOCRPages > 0 {
		env = append(env, fmt.Sprintf("WHATSKEPT_PDF_MAX_OCR_PAGES=%d", maxOCRPages))
	}
	if renderScale > 0 {
		env = append(env, fmt.Sprintf("WHATSKEPT_PDF_RENDER_SCALE=%.2f", renderScale))
	}
	cmd.Env = env
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
	// Swift helper emits JSON lines that can exceed the default
	// bufio max (PDFs with hundreds of pages of OCR text easily
	// crest 1 MB per response). bufio.NewReader's default 4096-byte
	// buffer grows on demand via ReadBytes, but we still raise the
	// initial size so the common case avoids a few realloc cycles.
	return &pdfWorker{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdout, 256*1024),
		stderr: stderr,
	}, nil
}

func (w *pdfWorker) extract(rowid int64, path string) (*pdfResponse, error) {
	req := pdfRequest{ID: rowid, Kind: "pdf", Path: path}
	enc, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	enc = append(enc, '\n')
	if _, err := w.stdin.Write(enc); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	// ReadBytes grows its buffer past the bufio reader's initial
	// size as needed, so a 5 MB response (large OCR'd contract) is
	// handled without truncation. We only have to worry about
	// per-process memory, which the cap on max pages bounds.
	line, err := w.stdout.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var resp pdfResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		// Truncated dump only — these responses can be huge and
		// the full line would drown the DB error column.
		preview := string(line)
		if len(preview) > 256 {
			preview = preview[:256] + "…"
		}
		return nil, fmt.Errorf("decode response: %w (line=%q)", err, preview)
	}
	if resp.ID != rowid {
		return nil, fmt.Errorf("response id mismatch: want=%d got=%d", rowid, resp.ID)
	}
	return &resp, nil
}

// Close shuts the worker down cleanly. Closing stdin → Swift loop
// exits with status 0 → cmd.Wait returns. Called via defer from
// DocumentIndex; safe to call twice.
func (w *pdfWorker) Close() error {
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
