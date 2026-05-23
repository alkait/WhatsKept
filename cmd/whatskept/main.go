// whatskept — keep a searchable, agent-queryable copy of your WhatsApp
// history from an iOS backup. Go port; in progress.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version is overridden at build time via `-ldflags "-X main.Version=..."`.
var Version = "0.0.0-dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "whatskept",
		Short: "Searchable, agent-queryable WhatsApp history from an iOS backup",
		Long: `whatskept extracts WhatsApp ChatStorage.sqlite from an iOS backup,
applies SQL views + an FTS5 index, and writes AGENTS.md so an agent can
query your WhatsApp history with plain SQL.

Go rewrite in progress. The Python source-of-truth implementation lives
in a separate repository.`,
		Version:           Version,
		SilenceUsage:      true,
		DisableAutoGenTag: true,
	}

	root.AddCommand(newListCmd())
	root.AddCommand(newExtractCmd())

	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
