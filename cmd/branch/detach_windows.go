//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

const (
	detachedProcess  = 0x00000008
	createNoWindow   = 0x08000000
	createNewProcessGroup = 0x00000200
)

// detachChild marks cmd to launch without a console window and outside the
// parent's process group, so closing the launching terminal doesn't take
// the child with it.
func detachChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess | createNoWindow | createNewProcessGroup,
	}
}
