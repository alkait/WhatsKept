package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"whatskept/internal/postprocess"
	"whatskept/internal/secrets"
)

// `whatskept document-index` — walks every WhatsApp document
// message (ZMESSAGETYPE = 8) in the workspace's ChatStorage.sqlite,
// decrypts the PDF attachment from the iOS backup, extracts its
// body text via Apple PDFKit with a Vision OCR fallback for
// scanned pages, and persists the results in wa_document_text +
// document_index.
//
// Resumable per-row: the second `document-index` run picks up
// exactly where the previous one left off. Ctrl+C between rows is
// safe; every committed row is durable.
//
// Non-PDF documents (xlsx, docx, html, …) are recorded as
// status='unsupported' without any decrypt work — those are out
// of scope for v1. Their filenames are still in messages_fts via
// wa_document (rebuilt by views.sql on every sync).
//
// The matching GUI entry point is the "Sync documents" button on
// the Database tab (see internal/app/server.go).
func newDocumentIndexCmd() *cobra.Command {
	var (
		workspace    string
		backupPath   string
		backupRoot   string
		limit        int
		retryMissing bool
		retryErrors  bool
		maxOCRPages  int
		renderScale  float64
		emitJSON     bool
	)

	cmd := &cobra.Command{
		Use:   "document-index",
		Short: "Decrypt + extract text from WhatsApp PDF attachments, populate wa_document_text",
		Long: `Per-row pipeline:

  1. SELECT every ZWAMESSAGE.ZMESSAGETYPE = 8 row that isn't already
     in document_index with status='extracted', 'extracted_empty',
     or 'unsupported'.
  2. For PDFs: decrypt from the iOS backup and write to
     <workspace>/documents/<rowid>.pdf.
  3. Run Apple PDFKit via the bundled whatskept-vision helper. For
     each page, take the native text if available; otherwise
     rasterize and run Vision OCR.
  4. UPSERT wa_document_text (extracted body) and document_index
     (state) in one transaction.
  5. Rebuild messages_fts to include the new document text.

Non-PDFs (xlsx/docx/…) are recorded as 'unsupported' without any
decrypt work. Their filenames remain indexable via wa_document.

Resumability is per-row: interrupt with Ctrl+C and re-run later.
Use --retry-missing / --retry-errors to revisit terminal-status rows.

Password is read from $BACKUP_PASSWORD or a .env in the workspace.

After a successful run, agent queries can MATCH on document content:

  SELECT v.rowid, v.ts, v.sender_name, d.filename, t.text
    FROM messages_fts
    JOIN v_messages       v ON v.rowid = messages_fts.rowid
    JOIN wa_document      d ON d.rowid = v.rowid
    LEFT JOIN wa_document_text t ON t.rowid = v.rowid
    WHERE messages_fts MATCH 'tenancy OR cardamom';`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if workspace == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("getwd: %w", err)
				}
				workspace = cwd
			}
			absWS, err := filepath.Abs(workspace)
			if err != nil {
				return fmt.Errorf("abs workspace: %w", err)
			}

			password, err := secrets.GetBackupPassword(absWS)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithCancel(cmd.Context())
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sig)
			go func() {
				<-sig
				fmt.Fprintln(os.Stderr, "Stopping after current document (Ctrl+C again to force-exit)…")
				cancel()
				<-sig
				os.Exit(130)
			}()

			res, err := postprocess.DocumentIndex(postprocess.DocumentIndexOptions{
				Workspace:    absWS,
				BackupPath:   backupPath,
				BackupRoot:   backupRoot,
				Password:     password,
				Limit:        limit,
				RetryMissing: retryMissing,
				RetryErrors:  retryErrors,
				MaxOCRPages:  maxOCRPages,
				RenderScale:  float32(renderScale),
				Ctx:          ctx,
				Log: func(s string) {
					if !emitJSON {
						fmt.Fprintln(os.Stderr, s)
					}
				},
				Progress: func(p postprocess.DocumentIndexProgress) {
					if !emitJSON {
						pct := 0.0
						if p.Pending > 0 {
							pct = float64(p.Done) * 100 / float64(p.Pending)
						}
						fmt.Fprintf(os.Stderr,
							"[%d / %d] %.1f%%  %.1f/s  eta %s  ext=%d empty=%d miss=%d err=%d unsup=%d\n",
							p.Done, p.Pending, pct, p.RatePerSec, fmtEta(p.ETASeconds),
							p.Extracted, p.Empty, p.Missing, p.Errors, p.Unsupported,
						)
					}
				},
			})
			if err != nil {
				return err
			}

			if emitJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&workspace, "workspace", "", "Workspace dir containing ChatStorage.sqlite (default: cwd)")
	cmd.Flags().StringVar(&backupPath, "backup", "", "Path to a specific iOS backup directory (default: most-recent)")
	cmd.Flags().StringVar(&backupRoot, "backup-root", "", "iOS backup root (default: ~/Library/Application Support/MobileSync/Backup)")
	cmd.Flags().IntVar(&limit, "limit", 0, "Process at most N rows this run (0 = no cap)")
	cmd.Flags().BoolVar(&retryMissing, "retry-missing", false, "Re-attempt rows previously marked 'missing'")
	cmd.Flags().BoolVar(&retryErrors, "retry-errors", false, "Re-attempt rows previously marked 'error'")
	cmd.Flags().IntVar(&maxOCRPages, "max-ocr-pages", 0, "Per-PDF cap on pages to OCR-rasterize (0 = helper default of 100)")
	cmd.Flags().Float64Var(&renderScale, "render-scale", 0, "Rasterization scale for OCR (0 = helper default of 2.0× = 144 dpi)")
	cmd.Flags().BoolVar(&emitJSON, "json", false, "Emit final result as JSON on stdout (suppresses progress)")

	return cmd
}
