//go:build !windows

package main

// consoleAttached always reports true off Windows. There is no console
// subsystem to detach from: stderr is a valid file descriptor whether it is a
// terminal, a pipe, or captured by a service manager such as systemd, so
// writing there is never a silent discard.
func consoleAttached() bool { return true }
