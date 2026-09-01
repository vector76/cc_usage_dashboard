//go:build windows

package main

import "golang.org/x/sys/windows"

// GetConsoleWindow is not among the wrappers x/sys/windows exports, so bind it
// by hand. NewLazySystemDLL resolves out of System32 rather than the process
// working directory, which matters for a binary users launch from arbitrary
// places.
var (
	consoleKernel32      = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleWindow = consoleKernel32.NewProc("GetConsoleWindow")
)

// consoleAttached reports whether this process has a console to write to.
//
// GetConsoleWindow returns the HWND of the process's console, or NULL when it
// has none. A binary linked with -H=windowsgui (the release and `go install
// -ldflags` builds) is never given one, so this is false and logging falls
// back to a file. A console-subsystem build gets one either from the shell
// that launched it or freshly allocated on double-click, so this is true and
// logs stay on stderr where the operator can see them.
func consoleAttached() bool {
	hwnd, _, _ := procGetConsoleWindow.Call()
	return hwnd != 0
}
