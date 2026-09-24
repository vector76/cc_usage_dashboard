package oauthusage

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW. The trayapp is a GUI-subsystem
// program with no console, so a console child would otherwise flash a
// terminal window on screen at every refresh.
const createNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}
