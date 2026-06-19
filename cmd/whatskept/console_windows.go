//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// hideConsoleIfOwned removes the console window when whatskept created it
// itself — i.e. a double-click / Explorer launch, where Windows allocates a
// fresh console for our console-subsystem binary. In that case we are the only
// process attached to it (GetConsoleProcessList returns 1), so we FreeConsole
// and the stray black window vanishes, leaving just the GUI.
//
// When launched from an existing shell (cmd/powershell), the console is shared
// with the parent — GetConsoleProcessList returns >1 — so we leave it alone:
// detaching would orphan the user's terminal, and the CLI subcommands still
// need it for their output. This is why we keep the console subsystem instead
// of linking with -H windowsgui, which would suppress CLI output entirely.
//
// Called only from the GUI (`app`) path, so CLI subcommands are never affected.
func hideConsoleIfOwned() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getConsoleProcessList := kernel32.NewProc("GetConsoleProcessList")
	freeConsole := kernel32.NewProc("FreeConsole")

	var pids [4]uint32
	n, _, _ := getConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n == 1 {
		freeConsole.Call()
	}
}
