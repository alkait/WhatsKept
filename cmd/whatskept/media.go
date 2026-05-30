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

// `whatskept media-index` — port of the Python whatskept.media_indexer
// CLI. Walks all WhatsApp image messages in the workspace's
// ChatStorage.sqlite, decrypts each JPEG from the iOS backup, runs
// Apple Vision (OCR + classification) via the bundled Swift helper,
// and persists the results in wa_image_text + media_index.
//
// Resumable per-row: the second `media-index` run picks up exactly
// where the previous one left off. Ctrl+C between rows is safe;
// every committed row is durable.
//
// The matching GUI entry point is the "Sync images" button on the
// Database tab (see internal/app/server.go).
func newMediaIndexCmd() *cobra.Command {
	var (
		workspace    string
		backupPath   string
		backupRoot   string
		limit        int
		retryMissing bool
		retryErrors  bool
		labelTopN    int
		labelMinConf float64
		emitJSON     bool
		engine       string
		model        string
	)

	cmd := &cobra.Command{
		Use:   "media-index",
		Short: "Decrypt + OCR + classify WhatsApp images, populate wa_image_text",
		Long: `Per-row pipeline:

  1. SELECT every ZWAMEDIAITEM.ZMEDIALOCALPATH ending in '.jpg' that
     isn't already in media_index with status='described'.
  2. Decrypt the JPEG from the iOS backup and write it to
     <workspace>/media/<rowid>.jpg.
  3. Run Apple Vision (OCR + classification) via the bundled
     whatskept-vision helper.
  4. UPSERT wa_image_text (results) and media_index (state) in one
     transaction.
  5. Rebuild messages_fts to include the new ocr_text + labels.

Resumability is per-row: interrupt with Ctrl+C and re-run later.
Use --retry-missing / --retry-errors to revisit terminal-status rows.

Password is read from $BACKUP_PASSWORD or a .env in the workspace.

After a successful run, agent queries can MATCH on image content:

  SELECT v.rowid, v.author, v.sent_at, t.ocr_text, t.labels
    FROM messages_fts
    JOIN v_messages    v ON v.rowid = messages_fts.rowid
    JOIN wa_image_text t ON t.rowid = v.rowid
    WHERE messages_fts MATCH 'passport OR receipt';`,
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

			// Ctrl+C → cancel the context so the loop exits
			// between rows (current row finishes first, so no
			// torn write). Hitting it a second time within a
			// few seconds gets the OS default kill behaviour.
			ctx, cancel := context.WithCancel(cmd.Context())
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sig)
			go func() {
				<-sig
				fmt.Fprintln(os.Stderr, "Stopping after current image (Ctrl+C again to force-exit)…")
				cancel()
				<-sig
				os.Exit(130)
			}()

			// Cloud engine reads its key from the environment so it
			// never lands on argv. The GUI sets it per-session; the CLI
			// honours an exported OPENROUTER_API_KEY (or one in the
			// workspace .env, already loaded into the process by now).
			apiKey := ""
			if engine == postprocess.SourceCloud {
				apiKey = os.Getenv("OPENROUTER_API_KEY")
				if apiKey == "" {
					return fmt.Errorf("--engine cloud requires OPENROUTER_API_KEY to be set")
				}
			}

			res, err := postprocess.MediaIndex(postprocess.MediaIndexOptions{
				Workspace:    absWS,
				BackupPath:   backupPath,
				BackupRoot:   backupRoot,
				Password:     password,
				Limit:        limit,
				RetryMissing: retryMissing,
				RetryErrors:  retryErrors,
				LabelTopN:    labelTopN,
				LabelMinConf: float32(labelMinConf),
				Engine:       engine,
				APIKey:       apiKey,
				Model:        model,
				Ctx:          ctx,
				Log: func(s string) {
					if !emitJSON {
						fmt.Fprintln(os.Stderr, s)
					}
				},
				Progress: func(p postprocess.MediaIndexProgress) {
					if !emitJSON {
						pct := 0.0
						if p.Total > 0 {
							pct = float64(p.Done) * 100 / float64(p.Total)
						}
						fmt.Fprintf(os.Stderr,
							"[%d / %d] %.1f%%  %.1f/s  eta %s  ocr=%d labels=%d miss=%d err=%d\n",
							p.Done, p.Pending, pct, p.RatePerSec, fmtEta(p.ETASeconds),
							p.WithOCR, p.WithLabels, p.Missing, p.Errors,
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
	cmd.Flags().IntVar(&labelTopN, "label-top-n", 5, "Keep at most this many classification labels per image")
	cmd.Flags().Float64Var(&labelMinConf, "label-min-conf", 0.50, "Drop classification labels below this confidence")
	cmd.Flags().BoolVar(&emitJSON, "json", false, "Emit final result as JSON on stdout (suppresses progress)")
	cmd.Flags().StringVar(&engine, "engine", "apple", "Describer: 'apple' (on-device Vision) or 'cloud' (OpenRouter; needs OPENROUTER_API_KEY)")
	cmd.Flags().StringVar(&model, "model", "", "Cloud model slug (default: "+postprocess.DefaultCloudModel+"; ignored for --engine apple)")

	return cmd
}

func fmtEta(secs float64) string {
	if secs < 60 {
		return fmt.Sprintf("%.0fs", secs)
	}
	if secs < 3600 {
		return fmt.Sprintf("%.1fm", secs/60)
	}
	return fmt.Sprintf("%.1fh", secs/3600)
}
