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

// `whatskept voice-index` — transcribe WhatsApp voice notes via the cloud.
//
// Two-phase, mirroring images:
//   - `voice-download` decrypts the .opus blobs from the iOS backup into
//     <workspace>/voice/ (needs the backup password). The GUI does this
//     as part of the messages sync.
//   - `voice-index` (this) transcribes every downloaded clip via an
//     OpenRouter audio model and persists wa_voice_text. No password —
//     it reads voice/ off disk. Needs OPENROUTER_API_KEY.
//
// Resumable per-row: re-running picks up where the previous run left off.
func newVoiceIndexCmd() *cobra.Command {
	var (
		workspace   string
		limit       int
		retryErrors bool
		emitJSON    bool
		model       string
	)

	cmd := &cobra.Command{
		Use:   "voice-index",
		Short: "Transcribe downloaded WhatsApp voice notes via the cloud, populate wa_voice_text",
		Long: `Transcribe phase (no backup access):

  1. SELECT every 'downloaded' .opus voice note not yet transcribed.
  2. Send the raw Ogg/Opus to an OpenRouter audio model.
  3. UPSERT wa_voice_text (transcript) and flip voice_index to
     'transcribed'.
  4. Rebuild messages_fts to include the new transcripts.

Run 'whatskept voice-download' (or a GUI sync) first to put the .opus
files on disk. Requires OPENROUTER_API_KEY (exported, or in the
workspace .env). Resumable per-row: interrupt with Ctrl+C and re-run.

After a successful run, agent queries can MATCH on transcript text:

  SELECT v.rowid, v.author, v.sent_at, t.transcript
    FROM messages_fts
    JOIN v_messages    v ON v.rowid = messages_fts.rowid
    JOIN wa_voice_text t ON t.rowid = v.rowid
    WHERE messages_fts MATCH 'meeting OR rendezvous';`,
		RunE: func(cmd *cobra.Command, args []string) error {
			absWS, err := resolveWorkspace(workspace)
			if err != nil {
				return err
			}
			apiKey := os.Getenv("OPENROUTER_API_KEY")
			if apiKey == "" {
				return fmt.Errorf("voice-index requires OPENROUTER_API_KEY to be set")
			}
			ctx, cancel := signalCtx(cmd.Context(), "Stopping after current clip (Ctrl+C again to force-exit)…")
			defer cancel()

			res, err := postprocess.VoiceIndex(postprocess.VoiceIndexOptions{
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
				Progress: func(p postprocess.VoiceIndexProgress) {
					if !emitJSON {
						pct := 0.0
						if p.Pending > 0 {
							pct = float64(p.Done) * 100 / float64(p.Pending)
						}
						fmt.Fprintf(os.Stderr,
							"[%d / %d] %.1f%%  %.1f clips/s  eta %s  ok=%d err=%d  $%.4f\n",
							p.Done, p.Pending, pct, p.RatePerSec, fmtEta(p.ETASeconds),
							p.Transcribed, p.Errors, p.CostUSD,
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
	cmd.Flags().IntVar(&limit, "limit", 0, "Process at most N clips this run (0 = no cap)")
	cmd.Flags().BoolVar(&retryErrors, "retry-errors", false, "Re-attempt clips previously marked 'error'")
	cmd.Flags().BoolVar(&emitJSON, "json", false, "Emit final result as JSON on stdout (suppresses progress)")
	cmd.Flags().StringVar(&model, "model", "", "Cloud audio model slug (default: "+postprocess.DefaultVoiceModel+")")
	return cmd
}

// `whatskept voice-download` — phase 1: decrypt every WhatsApp voice note
// from the iOS backup into <workspace>/voice/. Needs the backup password.
func newVoiceDownloadCmd() *cobra.Command {
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
		Use:   "voice-download",
		Short: "Decrypt WhatsApp voice notes from the iOS backup into <workspace>/voice/",
		Long: `Decrypts every .opus voice note referenced in ChatStorage.sqlite from
the encrypted iOS backup and writes it to <workspace>/voice/<rowid>.opus,
marking each row 'downloaded'. This is the only voice command that needs
the backup password; 'voice-index' (cloud transcribe) is a pure consumer
of the voice/ folder.

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
			ctx, cancel := signalCtx(cmd.Context(), "Stopping after current clip (Ctrl+C again to force-exit)…")
			defer cancel()

			res, err := postprocess.DownloadVoice(postprocess.VoiceIndexOptions{
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
				Progress: func(p postprocess.VoiceIndexProgress) {
					if !emitJSON {
						fmt.Fprintf(os.Stderr,
							"[download %d / %d]  %.1f/s  eta %s  dl=%d miss=%d err=%d\n",
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
	cmd.Flags().StringVar(&backupRoot, "backup-root", "", "iOS backup root (default: ~/Library/Application Support/MobileSync/Backup)")
	cmd.Flags().IntVar(&limit, "limit", 0, "Process at most N clips this run (0 = no cap)")
	cmd.Flags().BoolVar(&retryMissing, "retry-missing", false, "Re-attempt rows previously marked 'missing'")
	cmd.Flags().BoolVar(&retryErrors, "retry-errors", false, "Re-attempt rows previously marked 'error'")
	cmd.Flags().BoolVar(&emitJSON, "json", false, "Emit final result as JSON on stdout (suppresses progress)")
	return cmd
}

// resolveWorkspace defaults to cwd and returns an absolute path.
func resolveWorkspace(ws string) (string, error) {
	if ws == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getwd: %w", err)
		}
		ws = cwd
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return "", fmt.Errorf("abs workspace: %w", err)
	}
	return abs, nil
}

// signalCtx wires Ctrl+C to cancel the context (a second hit force-exits).
func signalCtx(parent context.Context, msg string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, msg)
		cancel()
		<-sig
		os.Exit(130)
	}()
	return ctx, cancel
}
