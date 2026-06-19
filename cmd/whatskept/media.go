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

// `whatskept media-index` — download + describe WhatsApp images.
//
// As a CLI convenience it runs BOTH phases in sequence so the one
// command still does what it always did:
//
//  1. DownloadMedia — decrypt every image from the iOS backup into
//     <workspace>/media/ (needs the backup password).
//  2. MediaIndex — run the cloud describer over the downloaded images
//     and persist wa_image_text + media_index.
//
// The GUI splits these into two buttons ("Download images" then "AI
// image descriptions") so only the download needs the password; use the
// separate `whatskept media-download` command for the download phase
// alone.
//
// Resumable per-row in both phases: Ctrl+C between rows is safe and a
// re-run picks up where it left off.
func newMediaIndexCmd() *cobra.Command {
	var (
		workspace    string
		backupPath   string
		backupRoot   string
		limit        int
		retryMissing bool
		retryErrors  bool
		emitJSON     bool
		model        string
	)

	cmd := &cobra.Command{
		Use:   "media-index",
		Short: "Download + describe WhatsApp images, populate wa_image_text",
		Long: `Two-phase pipeline (download, then describe):

  Phase 1 — download (needs the backup password):
    a. SELECT every ZWAMEDIAITEM.ZMEDIALOCALPATH ending in '.jpg' that
       isn't already on disk.
    b. Decrypt the JPEG from the iOS backup and write it to
       <workspace>/media/<rowid>.<ext>, marking the row 'downloaded'.

  Phase 2 — describe (no backup access):
    c. Run the cloud vision model over every 'downloaded' image to get
       OCR text + a description.
    d. UPSERT wa_image_text (results) and flip media_index to 'described'.
    e. Rebuild messages_fts to include the new ocr_text + description.

Run 'whatskept media-download' to do phase 1 alone. Resumability is
per-row in both phases: interrupt with Ctrl+C and re-run later. Use
--retry-missing / --retry-errors to revisit terminal-status rows.

The describe phase requires OPENROUTER_API_KEY (exported, or in the
workspace .env). The backup password is read from $BACKUP_PASSWORD or a
.env in the workspace.

After a successful run, agent queries can MATCH on image content:

  SELECT v.rowid, v.author, v.sent_at, t.ocr_text, t.description
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

			// The cloud describer reads its key from the environment so
			// it never lands on argv. The GUI sets it per-session; the
			// CLI honours an exported OPENROUTER_API_KEY (or one in the
			// workspace .env, already loaded into the process by now).
			apiKey := os.Getenv("OPENROUTER_API_KEY")
			if apiKey == "" {
				return fmt.Errorf("media-index requires OPENROUTER_API_KEY to be set")
			}

			logLine := func(s string) {
				if !emitJSON {
					fmt.Fprintln(os.Stderr, s)
				}
			}

			// Phase 1 — download. Decrypts the backup into media/; the
			// only phase that needs the password.
			if _, err := postprocess.DownloadMedia(postprocess.MediaIndexOptions{
				Workspace:    absWS,
				BackupPath:   backupPath,
				BackupRoot:   backupRoot,
				Password:     password,
				Limit:        limit,
				RetryMissing: retryMissing,
				RetryErrors:  retryErrors,
				Ctx:          ctx,
				Log:          logLine,
				Progress: func(p postprocess.MediaIndexProgress) {
					if !emitJSON {
						fmt.Fprintf(os.Stderr,
							"[download %d / %d]  %.1f/s  eta %s  dl=%d miss=%d err=%d\n",
							p.Done, p.Pending, p.RatePerSec, fmtEta(p.ETASeconds),
							p.Downloaded, p.Missing, p.Errors,
						)
					}
				},
			}); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return nil // cancelled during download; re-run to resume
			}

			// Phase 2 — describe the downloaded images. No backup access.
			res, err := postprocess.MediaIndex(postprocess.MediaIndexOptions{
				Workspace:   absWS,
				Limit:       limit,
				RetryErrors: retryErrors,
				Engine:      postprocess.SourceCloud,
				APIKey:      apiKey,
				Model:       model,
				Ctx:         ctx,
				Log:         logLine,
				Progress: func(p postprocess.MediaIndexProgress) {
					if !emitJSON {
						pct := 0.0
						if p.Total > 0 {
							pct = float64(p.Done) * 100 / float64(p.Total)
						}
						fmt.Fprintf(os.Stderr,
							"[scan %d / %d] %.1f%%  %.1f/s  eta %s  ocr=%d desc=%d miss=%d err=%d\n",
							p.Done, p.Pending, pct, p.RatePerSec, fmtEta(p.ETASeconds),
							p.WithOCR, p.WithDesc, p.Missing, p.Errors,
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
	cmd.Flags().IntVar(&limit, "limit", 0, "Process at most N rows this run (0 = no cap)")
	cmd.Flags().BoolVar(&retryMissing, "retry-missing", false, "Re-attempt rows previously marked 'missing'")
	cmd.Flags().BoolVar(&retryErrors, "retry-errors", false, "Re-attempt rows previously marked 'error'")
	cmd.Flags().BoolVar(&emitJSON, "json", false, "Emit final result as JSON on stdout (suppresses progress)")
	cmd.Flags().StringVar(&model, "model", "", "Cloud model slug (default: "+postprocess.DefaultCloudModel+")")

	return cmd
}

// `whatskept media-download` — phase 1 alone: decrypt every WhatsApp
// image from the iOS backup into <workspace>/media/. This is the only
// image command that needs the backup password. Run `media-index`
// afterwards (or the enrichment buttons in the GUI) to describe them.
func newMediaDownloadCmd() *cobra.Command {
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
		Use:   "media-download",
		Short: "Decrypt WhatsApp images from the iOS backup into <workspace>/media/",
		Long: `Decrypts every WhatsApp image referenced in ChatStorage.sqlite from
the encrypted iOS backup and writes it to <workspace>/media/<rowid>.<ext>,
marking each row 'downloaded'. This is the only image command that needs
the backup password; the describers (media-index / cloud) are pure
consumers of the media/ folder.

Resumable per-row: Ctrl+C between rows is safe and a re-run resumes.
Use --retry-missing / --retry-errors to revisit terminal-status rows
(e.g. after pulling a fresher backup).

Password is read from $BACKUP_PASSWORD or a .env in the workspace.`,
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
				fmt.Fprintln(os.Stderr, "Stopping after current image (Ctrl+C again to force-exit)…")
				cancel()
				<-sig
				os.Exit(130)
			}()

			res, err := postprocess.DownloadMedia(postprocess.MediaIndexOptions{
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
				Progress: func(p postprocess.MediaIndexProgress) {
					if !emitJSON {
						fmt.Fprintf(os.Stderr,
							"[%d / %d]  %.1f/s  eta %s  dl=%d miss=%d err=%d\n",
							p.Done, p.Pending, p.RatePerSec, fmtEta(p.ETASeconds),
							p.Downloaded, p.Missing, p.Errors,
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
	cmd.Flags().IntVar(&limit, "limit", 0, "Download at most N images this run (0 = no cap)")
	cmd.Flags().BoolVar(&retryMissing, "retry-missing", false, "Re-attempt rows previously marked 'missing'")
	cmd.Flags().BoolVar(&retryErrors, "retry-errors", false, "Re-attempt rows previously marked 'error'")
	cmd.Flags().BoolVar(&emitJSON, "json", false, "Emit final result as JSON on stdout (suppresses progress)")

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
