//go:build windows

package main

import (
	"strings"
	"syscall"
	"unsafe"
)

// ensureSingleInstance enforces one running GUI instance on Windows. It returns
// true if this is the first/primary instance (proceed) and false if another
// instance is already running — in which case it has brought that existing
// window to the foreground and the caller should exit without opening a window.
//
// The lock is a named mutex: the OS releases it automatically when the owning
// process exits (even on a crash), so there is no stale-lock problem. The
// primary intentionally keeps its mutex handle open for the process lifetime.
//
// This composes with the auto-updater: its relauncher waits ~2s for the old
// process to exit (releasing the mutex) before starting the new exe, so the
// updated instance acquires the mutex cleanly as the new primary.
func ensureSingleInstance() bool {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	createMutex := kernel32.NewProc("CreateMutexW")

	name, err := syscall.UTF16PtrFromString("WhatsKept-SingleInstance")
	if err != nil {
		return true // can't form the name; fail open rather than block startup
	}
	handle, _, callErr := createMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if handle == 0 {
		return true // couldn't create the mutex; fail open
	}

	const errorAlreadyExists = syscall.Errno(183)
	if errno, ok := callErr.(syscall.Errno); ok && errno == errorAlreadyExists {
		// A primary instance already owns the mutex: focus it and bow out.
		focusExistingWindow()
		return false
	}
	// Primary instance: deliberately never CloseHandle(handle) so the named
	// mutex persists until we exit, at which point the OS releases it.
	return true
}

// focusExistingWindow finds the running WhatsKept window by its title and
// brings it to the foreground, restoring it first if minimized. Best-effort —
// SetForegroundWindow can be refused by the foreground-lock policy, in which
// case the taskbar button flashes instead, which is still adequate feedback.
func focusExistingWindow() {
	user32 := syscall.NewLazyDLL("user32.dll")
	enumWindows := user32.NewProc("EnumWindows")
	getWindowText := user32.NewProc("GetWindowTextW")
	isWindowVisible := user32.NewProc("IsWindowVisible")
	setForeground := user32.NewProc("SetForegroundWindow")
	showWindow := user32.NewProc("ShowWindow")

	const swRestore = 9

	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if v, _, _ := isWindowVisible.Call(hwnd); v == 0 {
			return 1 // not visible; keep enumerating
		}
		var buf [256]uint16
		getWindowText.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if strings.HasPrefix(syscall.UTF16ToString(buf[:]), "WhatsKept") {
			showWindow.Call(hwnd, swRestore) // un-minimize if needed
			setForeground.Call(hwnd)
			return 0 // found it; stop enumerating
		}
		return 1
	})
	enumWindows.Call(cb, 0)
}
