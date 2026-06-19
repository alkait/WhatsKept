package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"whatskept/internal/postprocess"
	"whatskept/internal/secrets"
)

// `whatskept document-index` — extract WhatsApp PDF text via the cloud.
//
// Two-phase, mirroring voice:
//   - `document-download` decrypts the PDF blobs from the iOS backup into
//     <workspace>/documents/ (needs the backup password). The GUI does this
//     as part of the messages sync.
//   - `document-index` (this) extracts every downloaded PDF's text via
//     OpenRouter's file-parser plugin (free pdf-text for the native layer,
//     mistral-ocr for scanned pages) and persists wa_document_text. No
//     password — it reads documents/ off disk. Needs OPENROUTER_API_KEY.
//
// This replaces the previous macOS-only Apple PDFKit + Vision path, so it
// works on macOS, Windows, and Linux. Resumable per-row.
func newDocumentIndexCmd() *cobra.Command {
	var (
		workspace   string
		limit       int
		retryErrors bool
		emitJSON    bool
		model       string
	)

	cmd := &cobra.Command{
		Use:   "document-index",
		Short: "Extract text from downloaded WhatsApp PDFs via the cloud, populate wa_document_text",
		Long: `Extract phase (no backup access):

  1. SELECT every 'downloaded' PDF not yet extracted.
  2. Send it to OpenRouter's file-parser plugin: free pdf-text for the
     native text layer, escalating to mistral-ocr for scanned pages.
     Oversized PDFs are split into page-range chunks (pdfcpu) and stitched.
  3. UPSERT wa_document_text (body text) and flip document_index to
     'extracted'.
  4. Rebuild messages_fts to include the new document text.

Run 'whatskept document-download' (or a GUI sync) first to put the PDFs on
disk. Requires OPENROUTER_API_KEY (exported, or in the workspace .env).
Resumable per-row: interrupt with Ctrl+C and re-run.

After a successful run, agent queries can MATCH on document content:

  SELECT v.rowid, v.ts, d.filename, t.text
    FROM messages_fts
    JOIN v_messages       v ON v.rowid = messages_fts.rowid
    JOIN wa_document      d ON d.rowid = v.rowid
    LEFT JOIN wa_document_text t ON t.rowid = v.rowid
    WHERE messages_fts MATCH 'tenancy OR cardamom';`,
		RunE: func(cmd *cobra.Command, args []string) error {
			absWS, err := resolveWorkspace(workspace)
			if err != nil {
				return err
			}
			apiKey := os.Getenv("OPENROUTER_API_KEY")
			if apiKey == "" {
				return fmt.Errorf("document-index requires OPENROUTER_API_KEY to be set")
			}
			ctx, cancel := signalCtx(cmd.Context(), "Stopping after current document (Ctrl+C again to force-exit)…")
			defer cancel()

			res, err := postprocess.DocumentIndex(postprocess.DocumentIndexOptions{
				Workspace:   absWS,
				Engine:      postprocess.SourceCloud,
				APIKey:      apiKey,
				Model:       model,
				Limit:       limit,
				RetryErrors: retryErrors,
				Ctx:         ctx,
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
							"[%d / %d] %.1f%%  %.1f docs/s  eta %s  ext=%d empty=%d err=%d  $%.4f\n",
							p.Done, p.Pending, pct, p.RatePerSec, fmtEta(p.ETASeconds),
							p.Extracted, p.Empty, p.Errors, p.CostUSD,
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
	cmd.Flags().IntVar(&limit, "limit", 0, "Process at most N documents this run (0 = no cap)")
	cmd.Flags().BoolVar(&retryErrors, "retry-errors", false, "Re-attempt PDFs that failed a prior extract")
	cmd.Flags().BoolVar(&emitJSON, "json", false, "Emit final result as JSON on stdout (suppresses progress)")
	cmd.Flags().StringVar(&model, "model", "", "Cloud carrier model slug (default: "+postprocess.DefaultDocumentModel+")")
	return cmd
}

// `whatskept document-download` — phase 1: decrypt every WhatsApp PDF from the
// iOS backup into <workspace>/documents/. Needs the backup password. Non-PDF
// documents are parked 'unsupported' (no extractor for them yet).
func newDocumentDownloadCmd() *cobra.Command {
	var (
		workspace    string
		backupPath   string
		backupRoot   string
		limit        int
		retryMissing bool
		retryErrors  bool
		emitJSON     bool
	)
	cmd := &cobra.Command{
		Use:   "document-download",
		Short: "Decrypt WhatsApp PDFs from the iOS backup into <workspace>/documents/",
		Long: `Decrypts every PDF document referenced in ChatStorage.sqlite from the
encrypted iOS backup and writes it to <workspace>/documents/<rowid>.pdf,
marking each row 'downloaded'. This is the only document command that needs
the backup password; 'document-index' (cloud extract) is a pure consumer of
the documents/ folder. Non-PDF formats (xlsx/docx/…) are parked 'unsupported'.

Resumable per-row. Password is read from $BACKUP_PASSWORD or a .env.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			absWS, err := resolveWorkspace(workspace)
			if err != nil {
				return err
			}
			password, err := secrets.GetBackupPassword(absWS)
			if err != nil {
				return err
			}
			ctx, cancel := signalCtx(cmd.Context(), "Stopping after current document (Ctrl+C again to force-exit)…")
			defer cancel()

			res, err := postprocess.DownloadDocument(postprocess.DocumentIndexOptions{
				Workspace:    absWS,
				BackupPath:   backupPath,
				BackupRoot:   backupRoot,
				Password:     password,
				Limit:        limit,
				RetryMissing: retryMissing,
				RetryErrors:  retryErrors,
				Ctx:          ctx,
				Log: func(s string) {
					if !emitJSON {
						fmt.Fprintln(os.Stderr, s)
					}
				},
				Progress: func(p postprocess.DocumentIndexProgress) {
					if !emitJSON {
						fmt.Fprintf(os.Stderr,
							"[download %d / %d]  %.1f/s  eta %s  dl=%d miss=%d err=%d unsup=%d\n",
							p.Done, p.Pending, p.RatePerSec, fmtEta(p.ETASeconds),
							p.Downloaded, p.Missing, p.Errors, p.Unsupported,
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
	cmd.Flags().StringVar(&backupRoot, "backup-root", "", "iOS backup root (defaults to your platform's backup folder)")
	cmd.Flags().IntVar(&limit, "limit", 0, "Process at most N documents this run (0 = no cap)")
	cmd.Flags().BoolVar(&retryMissing, "retry-missing", false, "Re-attempt rows previously marked 'missing'")
	cmd.Flags().BoolVar(&retryErrors, "retry-errors", false, "Re-attempt rows previously marked 'error'")
	cmd.Flags().BoolVar(&emitJSON, "json", false, "Emit final result as JSON on stdout (suppresses progress)")
	return cmd
}
