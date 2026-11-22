package main

import (
	"fmt"
	"os"
	"path/filepath"

	"whatskept/internal/backup"
)

// version is stamped by the release workflow via
// -ldflags "-X main.version=v0.1.N"; source builds report "dev".
var version = "dev"

const usage = `whatskept — WhatsApp history, kept.

Usage:
  whatskept init [dir]               initialize a workspace (default: current directory)
  whatskept import <ios-backup-path> import history from an iOS backup
  whatskept import --list            list iOS backups on this machine
  whatskept enrich                   turn queued media into searchable text via OpenRouter [--concurrency n]
  whatskept live                     capture new messages as they arrive (links via QR on first run)
  whatskept send <chat> <text>       send a text through the running live (chat: phone number or JID)
                                     [--reply-to <stanza-id>] quotes a message in that chat
  whatskept send <chat> --forward <stanza-id>
                                     forward a message (text, image, voice note, document) to a chat
  whatskept archive <chat>           archive a chat through the running live
                                     [--undo] unarchives it instead
  whatskept mcp --database <file>    serve a database file over MCP (HTTP) [--addr host:port]
                                     [--stdio] serves over stdin/stdout instead (no token)
  whatskept -v | --version           show the version
  whatskept -h | --help              show this help
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Print(usage)
	case "-v", "--version", "version":
		fmt.Println(version)
	case "init":
		dir := "."
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		if err := runInit(filepath.Clean(dir)); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "import":
		if len(os.Args) < 3 {
			fmt.Fprint(os.Stderr, "import requires the path to an iOS backup, or --list\n\n"+usage)
			os.Exit(2)
		}
		if os.Args[2] == "--list" {
			root := backup.DefaultRoot()
			if len(os.Args) > 3 {
				root = os.Args[3]
			}
			if err := runList(root); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
		if err := runImport(filepath.Clean(os.Args[2])); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "enrich":
		if err := runEnrich(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "live":
		if err := runLive(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "send":
		var text, replyTo, forward string
		switch {
		case len(os.Args) == 5 && os.Args[3] == "--forward" && os.Args[4] != "":
			forward = os.Args[4]
		case len(os.Args) == 6 && os.Args[4] == "--reply-to" && os.Args[5] != "":
			text, replyTo = os.Args[3], os.Args[5]
		case len(os.Args) == 4 && os.Args[3] != "--forward":
			text = os.Args[3]
		default:
			fmt.Fprint(os.Stderr, "send requires a chat (phone number or JID) and the message text, then optionally --reply-to <stanza-id>; or a chat and --forward <stanza-id>\n\n"+usage)
			os.Exit(2)
		}
		if err := runSend(os.Args[2], text, replyTo, forward); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "archive":
		var undo bool
		switch {
		case len(os.Args) == 3 && os.Args[2] != "--undo":
		case len(os.Args) == 4 && os.Args[2] != "--undo" && os.Args[3] == "--undo":
			undo = true
		default:
			fmt.Fprint(os.Stderr, "archive requires a chat (phone number or JID), then optionally --undo to unarchive it\n\n"+usage)
			os.Exit(2)
		}
		if err := runArchive(os.Args[2], undo); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "mcp":
		if err := runMCP(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}
