//go:build darwin

package app

import (
	"fmt"
	"os/exec"
)

// applyUpdate (macOS) opens a Terminal window running the official curl|bash
// installer and returns immediately. The UI quits the app right after — the
// running binary can't replace itself, and the installer relaunches us when it
// finishes (see build/install.sh). We run it visibly in Terminal rather than
// in the background because once the app quits there's no UI left to show
// progress; the installer prints its own steps and the relaunch.
func (s *server) applyUpdate() error {
	// `clear` keeps the window tidy on a reused shell; the heredoc-free
	// curl|bash is the exact command the README documents.
	shellCmd := "clear && echo 'Updating WhatsKept — this window will close itself shortly.' && " +
		"/bin/bash -c \"$(curl -fsSL " + installerURL + ")\""
	quoted := appleScriptQuote(shellCmd)

	// Same cold-launch window-reuse dance as handleOpenAgent: if Terminal
	// isn't already running, the `tell` block launches it and it opens an
	// empty default-profile window; reuse that window instead of leaving it
	// orphaned beside a second one.
	ascript := "tell application \"Terminal\"\n" +
		"set wasRunning to running\n" +
		"activate\n" +
		"if wasRunning then\n" +
		"do script " + quoted + "\n" +
		"else\n" +
		"repeat 20 times\n" +
		"if (count of windows) > 0 then exit repeat\n" +
		"delay 0.05\n" +
		"end repeat\n" +
		"if (count of windows) > 0 then\n" +
		"do script " + quoted + " in window 1\n" +
		"else\n" +
		"do script " + quoted + "\n" +
		"end if\n" +
		"end if\n" +
		"end tell"

	cmd := exec.Command("/usr/bin/osascript", "-e", ascript)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to launch installer: %w", err)
	}
	go cmd.Wait() // reap; it outlives us
	return nil
}

// cleanupStaleUpdate is a no-op on macOS — the installer replaces the .app
// bundle wholesale, leaving no .old binary behind to sweep.
func cleanupStaleUpdate() {}
