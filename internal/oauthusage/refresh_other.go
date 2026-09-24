//go:build !windows

package oauthusage

import "os/exec"

// hideWindow is a no-op off Windows, where a child process opens no window.
func hideWindow(*exec.Cmd) {}
