// whatskept — keep a searchable, agent-queryable copy of your WhatsApp
// history from an iOS backup. Go port; in progress.
package main

import (
	"fmt"
	"os"
	"runtime"
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
	root.AddCommand(newMediaDownloadCmd())
	root.AddCommand(newMediaIndexCmd())
	root.AddCommand(newVoiceDownloadCmd())
	root.AddCommand(newVoiceIndexCmd())
	root.AddCommand(newDocumentDownloadCmd())
	root.AddCommand(newDocumentIndexCmd())

	return root
}

func main() {
	// Disable Cobra's Windows "mousetrap": by default, when a Cobra program is
	// double-clicked in Explorer, Cobra prints "This is a command line tool.
	// You need to open cmd.exe and run it from there." and exits — BEFORE our
	// app-subcommand injection below can run. Clearing the text disables that
	// interception so a double-click falls through to the GUI. No-op off
	// Windows (the hook only exists there).
	cobra.MousetrapHelpText = ""

	// GUI-launch path: inject the `app` subcommand so the window opens
	// instead of Cobra dumping help text into a console the user can't see.
	//   - macOS: a Finder/Dock/`open` launch comes through the .app wrapper,
	//     detectable via os.Args[0].
	//   - Windows: there is no .app wrapper; a double-click (or a bare
	//     `whatskept.exe`) arrives with no args and the user expects the GUI.
	//     CLI users pass an explicit subcommand (`whatskept.exe list`), which
	//     is unaffected — so defaulting a bare Windows invocation to `app` is
	//     the right call for a primarily-GUI app.
	if len(os.Args) == 1 && (strings.Contains(os.Args[0], bundleExecutableHint) || runtime.GOOS == "windows") {
		os.Args = append(os.Args, "app")
	}
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
