package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"whatskept/internal/backup"
	"whatskept/internal/secrets"
)

const outputDBName = "ChatStorage.sqlite"

func newExtractCmd() *cobra.Command {
	var (
		backupPath string
		backupRoot string
		outDir     string
	)

	cmd := &cobra.Command{
		Use:   "extract",
		Short: "Extract WhatsApp ChatStorage.sqlite from an iOS backup",
		Long: `Decrypts WhatsApp's ChatStorage.sqlite from an iOS backup and writes
it to the workspace directory.

The backup password is read from $BACKUP_PASSWORD or a .env file in the
workspace (or any parent directory). Never prompts.

If --backup is not given, the most recent backup under --backup-root is used.

Note: post-processing (views.sql, FTS5, AGENTS.md) is not yet ported from
the Python implementation — this command only does the decryption + write.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if outDir == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("getwd: %w", err)
				}
				outDir = cwd
			}

			info, err := pickBackup(backupPath, backupRoot)
			if err != nil {
				return err
			}
			fmt.Printf("Backup:     %s\n", info.Path)
			fmt.Printf("Device:     %s\n", info.DisplayName())
			fmt.Printf("Last:       %s\n", info.LastBackupString())
			if !info.IsEncrypted {
				return errors.New("this command currently requires an encrypted backup")
			}

			password, err := secrets.GetBackupPassword(outDir)
			if err != nil {
				return err
			}

			outPath := filepath.Join(outDir, outputDBName)
			fmt.Printf("Output:     %s\n\n", outPath)
			fmt.Println("Unlocking keybag + decrypting (may take a few seconds) ...")

			t0 := time.Now()
			n, err := backup.ExtractChatStorage(*info, password, outPath)
			if err != nil {
				return err
			}
			fmt.Printf("Extracted:  %d bytes in %s\n", n, time.Since(t0).Round(time.Millisecond))
			return nil
		},
	}

	cmd.Flags().StringVar(&backupPath, "backup", "",
		"Use the backup at this path instead of auto-discovering")
	cmd.Flags().StringVar(&backupRoot, "backup-root", "",
		"Backup discovery root (defaults to your platform's iOS backup folder)")
	cmd.Flags().StringVarP(&outDir, "out", "o", "",
		"Output workspace directory (default: current directory)")
	return cmd
}

// pickBackup resolves the backup to use: explicit --backup wins, otherwise
// the most recent backup under --backup-root.
func pickBackup(backupPath, backupRoot string) (*backup.Info, error) {
	if backupPath != "" {
		info, err := backup.LoadInfo(backupPath)
		if err != nil {
			return nil, fmt.Errorf("load %q: %w", backupPath, err)
		}
		if info == nil {
			return nil, fmt.Errorf("not a valid iOS backup: %s", backupPath)
		}
		return info, nil
	}

	if backupRoot == "" {
		backupRoot = backup.DefaultRoot()
	}
	backups, err := backup.Discover(backupRoot)
	if err != nil {
		return nil, err
	}
	if len(backups) == 0 {
		return nil, fmt.Errorf("no backups found under %s", backupRoot)
	}
	return &backups[0], nil
}
