package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"whatskept/internal/helpers"
	"whatskept/internal/postprocess"
	"whatskept/internal/secrets"
)

// `whatskept voice-index` — port of the Python whatskept.voice_indexer
// CLI. Walks all WhatsApp .opus voice-note messages in the workspace's
// ChatStorage.sqlite, decrypts each from the iOS backup, transcribes
// via the bundled whisper-cli (Metal-accelerated on Apple Silicon),
// and persists results in wa_voice_text + voice_index.
//
// Resumable per-row: re-running picks up where the previous run left
// off. Ctrl+C between rows is safe; every committed row is durable.
//
// The matching GUI entry point is the "Sync voice notes" button on
// the Database tab (see internal/app/server.go).
//
// First-run UX: the speech model (~574 MB) is NOT bundled in the
// binary. If it's missing, voice-index returns ErrModelNotInstalled
// and the user is prompted to either re-run with --fetch-model or
// download via the GUI. The model lives at
// ~/Library/Application Support/whatskept/models/ and persists
// across app updates.
func newVoiceIndexCmd() *cobra.Command {
	var (
		workspace    string
		backupPath   string
		backupRoot   string
		language     string
		limit        int
		retryMissing bool
		retryErrors  bool
		fetchModel   bool
		emitJSON     bool
	)

	cmd := &cobra.Command{
		Use:   "voice-index",
		Short: "Decrypt + transcribe WhatsApp voice notes, populate wa_voice_text",
		Long: `Per-clip pipeline:

  1. SELECT every ZWAMEDIAITEM.ZMEDIALOCALPATH ending in '.opus' that
     isn't already in voice_index with status='transcribed'.
  2. Decrypt the OPUS from the iOS backup and write it to
     <workspace>/voice/<rowid>.opus (so the agent can replay it).
  3. afconvert OPUS -> 16 kHz mono Int16 WAV (whisper-cli's input
     format).
  4. whisper-cli with -l auto + the bundled large-v3-turbo model.
  5. UPSERT wa_voice_text (transcript + segments) and voice_index
     (state) in one transaction.
  6. Rebuild messages_fts to include the new transcripts.

The 574 MB speech model is downloaded on first use to
~/Library/Application Support/whatskept/models/ and persists across
app updates. Pass --fetch-model on the first run, or download via
the GUI.

Resumability is per-row: interrupt with Ctrl+C and re-run later.
Use --retry-missing / --retry-errors to revisit terminal-status rows.

Password is read from $BACKUP_PASSWORD or a .env in the workspace.

After a successful run, agent queries can MATCH on transcript text:

  SELECT v.rowid, v.author, v.sent_at, t.transcript, t.language
    FROM messages_fts
    JOIN v_messages    v ON v.rowid = messages_fts.rowid
    JOIN wa_voice_text t ON t.rowid = v.rowid
    WHERE messages_fts MATCH 'meeting OR rendezvous';`,
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

			// Ctrl+C → cancel the context (same pattern as media-index).
			ctx, cancel := context.WithCancel(cmd.Context())
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sig)
			go func() {
				<-sig
				fmt.Fprintln(os.Stderr, "Stopping after current clip (Ctrl+C again to force-exit)…")
				cancel()
				<-sig
				os.Exit(130)
			}()

			// First-run: ensure the speech model is on disk.
			st, _, err := helpers.CheckModel(helpers.WhisperModel, false)
			if err != nil {
				return fmt.Errorf("check model: %w", err)
			}
			if st != helpers.ModelPresent && st != helpers.ModelVerified {
				if !fetchModel {
					path, _ := helpers.ModelPath(helpers.WhisperModel)
					return fmt.Errorf(
						"speech model not installed at %s\n"+
							"  Re-run with --fetch-model to download (%d MB), or use the GUI.\n"+
							"  Source: %s",
						path, helpers.WhisperModel.Bytes/(1024*1024), helpers.WhisperModel.URL,
					)
				}
				if err := downloadModelCLI(ctx); err != nil {
					return fmt.Errorf("download model: %w", err)
				}
			}

			res, err := postprocess.VoiceIndex(postprocess.VoiceIndexOptions{
				Workspace:    absWS,
				BackupPath:   backupPath,
				BackupRoot:   backupRoot,
				Password:     password,
				Language:     language,
				Limit:        limit,
				RetryMissing: retryMissing,
				RetryErrors:  retryErrors,
				Ctx:          ctx,
				Log: func(s string) {
					if !emitJSON {
						fmt.Fprintln(os.Stderr, s)
					}
				},
				Progress: func(p postprocess.VoiceIndexProgress) {
					if !emitJSON {
						pct := 0.0
						if p.Pending > 0 {
							pct = float64(p.Done) * 100 / float64(p.Pending)
						}
						fmt.Fprintf(os.Stderr,
							"[%d / %d] %.1f%%  %.1f clips/s  eta %s  ok=%d miss=%d err=%d  %s\n",
							p.Done, p.Pending, pct, p.RatePerSec, fmtEta(p.ETASeconds),
							p.Transcribed, p.Missing, p.Errors, p.CurrentLabel,
						)
					}
				},
			})
			if err != nil {
				if errors.Is(err, postprocess.ErrModelNotInstalled) {
					// Shouldn't happen — we just downloaded it — but keep
					// the message useful in case CheckModel disagrees with
					// VoiceIndex's view (e.g. permission flake).
					return fmt.Errorf("model still not installed; try `whatskept voice-index --fetch-model`")
				}
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
	cmd.Flags().StringVar(&language, "language", "", "Force a single transcription language (BCP-47 code, e.g. 'ar', 'en'). Default: auto-detect per clip")
	cmd.Flags().IntVar(&limit, "limit", 0, "Process at most N clips this run (0 = no cap)")
	cmd.Flags().BoolVar(&retryMissing, "retry-missing", false, "Re-attempt rows previously marked 'missing'")
	cmd.Flags().BoolVar(&retryErrors, "retry-errors", false, "Re-attempt rows previously marked 'error'")
	cmd.Flags().BoolVar(&fetchModel, "fetch-model", false, "Download the speech model if it's not yet installed (~574 MB, one-time)")
	cmd.Flags().BoolVar(&emitJSON, "json", false, "Emit final result as JSON on stdout (suppresses progress)")

	return cmd
}

// downloadModelCLI runs helpers.DownloadModel with a simple stderr
// progress line. Refresh frequency is throttled inside helpers
// (default 250 ms) so we don't have to debounce here.
func downloadModelCLI(ctx context.Context) error {
	spec := helpers.WhisperModel
	fmt.Fprintf(os.Stderr,
		"Downloading speech model: %s (%d MB)\n  Source: %s\n",
		spec.Display, spec.Bytes/(1024*1024), spec.URL)

	tStart := time.Now()
	return helpers.DownloadModel(spec, helpers.DownloadOptions{
		Ctx: ctx,
		Progress: func(p helpers.DownloadProgress) {
			switch p.Stage {
			case "downloading":
				pct := 0.0
				if p.BytesTotal > 0 {
					pct = float64(p.BytesRead) * 100 / float64(p.BytesTotal)
				}
				fmt.Fprintf(os.Stderr, "\r  %.1f%%  %d / %d MB  %.1f MB/s  eta %s    ",
					pct,
					p.BytesRead/(1024*1024),
					p.BytesTotal/(1024*1024),
					p.RateBPS/(1024*1024),
					fmtEta(p.ETASeconds),
				)
			case "verifying":
				fmt.Fprintf(os.Stderr, "\r  verifying sha256…                                    ")
			case "done":
				fmt.Fprintf(os.Stderr, "\r  installed in %s.                                    \n",
					fmtEta(time.Since(tStart).Seconds()))
			}
		},
	})
}
