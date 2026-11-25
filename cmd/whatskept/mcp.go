package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"whatskept/internal/mcpserve"
)

// runMCP serves a database file over MCP (streamable HTTP) until the
// process is signalled. The database is addressed explicitly with
// --database — mcp is decoupled from workspace discovery. The auth
// token comes from the environment (never a flag — secrets don't
// belong in process lists).
func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	database := fs.String("database", "", "path to the database file (required), e.g. <workspace>/ChatStorage.sqlite")
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	stdio := fs.Bool("stdio", false, "serve over stdin/stdout instead of HTTP (no token; for clients that launch the server)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *database == "" {
		return errors.New("mcp requires --database <path-to-database-file>")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mcpserve.Version = version
	if *stdio {
		return mcpserve.ServeStdio(ctx, *database)
	}
	return mcpserve.Serve(ctx, *database, *addr, os.Getenv(mcpserve.TokenEnv))
}
