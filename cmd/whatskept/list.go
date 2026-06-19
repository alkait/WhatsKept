package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"whatskept/internal/backup"
)

func newListCmd() *cobra.Command {
	var backupRoot string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List discovered iOS backups (newest first)",
		Long: `Probes the platform's iOS backup folder for valid
iOS backups and prints them with device, iOS version, last-backup date, and
encryption status. Newest first.

Pass --backup-root to override the discovery root.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if backupRoot == "" {
				backupRoot = backup.DefaultRoot()
			}
			backups, err := backup.Discover(backupRoot)
			if err != nil {
				if errors.Is(err, backup.ErrAccessDenied) {
					return fmt.Errorf(
						"%w\n\nOn macOS the iOS backup directory is protected. Grant your "+
							"terminal app (Terminal.app, iTerm, etc.) Full Disk Access:\n"+
							"  System Settings → Privacy & Security → Full Disk Access → "+
							"add your terminal, then restart it.", err,
					)
				}
				return err
			}
			fmt.Println(backup.FormatListing(backups))
			return nil
		},
	}

	cmd.Flags().StringVar(&backupRoot, "backup-root", "",
		"Backup discovery root (defaults to your platform's iOS backup folder)")
	return cmd
}
