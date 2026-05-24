// whatskept — keep a searchable, agent-queryable copy of your WhatsApp
// history from an iOS backup. Go port; in progress.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// Version is overridden at build time via `-ldflags "-X main.Version=..."`.
var Version = "0.0.0-dev"

// bundleExecutableHint is the path fragment macOS uses for an
// `.app` bundle's executable. When os.Args[0] contains this, we
// were launched by `open WhatsKept.app` (Finder, Dock, aerospace,
// etc.) rather than from a shell, and the user expects the GUI —
// not Cobra's help screen on stderr.
const bundleExecutableHint = ".app/Contents/MacOS/"

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
	root.AddCommand(newAppCmd())

	return root
}

func main() {
	// .app-bundle launch path: macOS invokes us with no args. Inject
	// the `app` subcommand so the GUI opens instead of Cobra dumping
	// help text into a terminal that the user can't see.
	if len(os.Args) == 1 && strings.Contains(os.Args[0], bundleExecutableHint) {
		os.Args = append(os.Args, "app")
	}
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
