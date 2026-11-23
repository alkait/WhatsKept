package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"whatskept/internal/live"
	"whatskept/internal/workspace"
)

// runRead hands one read change to the `whatskept live` process running
// in this workspace — live owns the WhatsApp connection, so it makes the
// change and records it in the database. undo marks the chat unread
// instead.
func runRead(chat string, undo bool) error {
	root, err := workspace.Find()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	got, err := live.MarkRead(ctx, root, chat, !undo)
	if err != nil {
		return err
	}
	verb := "read"
	if !got.Read {
		verb = "unread"
	}
	fmt.Printf("marked %s chat=%s\n", verb, got.Chat)
	if got.Warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", got.Warning)
	}
	return nil
}
