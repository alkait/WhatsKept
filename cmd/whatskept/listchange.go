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

// runListChange hands one change to the user's chat lists to the
// `whatskept live` process running in this workspace, which makes it on
// WhatsApp and records it. args are what follow `list`, already checked.
func runListChange(op string, args []string) error {
	root, err := workspace.Find()
	if err != nil {
		return err
	}
	var list, name, chat string
	switch op {
	case "create":
		name = args[0]
	case "rename":
		list, name = args[0], args[1]
	case "delete":
		list = args[0]
	case "add", "remove":
		list, chat = args[0], args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	got, err := live.ManageList(ctx, root, op, list, name, chat)
	if err != nil {
		return err
	}
	switch op {
	case "create", "rename", "delete":
		fmt.Printf("%sd list id=%d name=%q\n", op, got.ID, got.Name)
	case "add":
		fmt.Printf("added chat=%s to list id=%d name=%q\n", got.Chat, got.ID, got.Name)
	case "remove":
		fmt.Printf("removed chat=%s from list id=%d name=%q\n", got.Chat, got.ID, got.Name)
	}
	if got.Warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", got.Warning)
	}
	return nil
}

// listArgs is how many arguments each `list` operation takes.
var listArgs = map[string]int{"create": 1, "rename": 2, "delete": 1, "add": 2, "remove": 2}
