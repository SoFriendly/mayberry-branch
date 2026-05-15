//go:build darwin || linux

package main

import (
	"os/exec"
	"syscall"
)

// detachChild marks cmd so its child becomes its own session leader. After
// Start() the parent can exit and the child keeps running independent of
// any controlling terminal.
func detachChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
