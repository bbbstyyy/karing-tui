//go:build linux || darwin

package headless

import (
	"os/exec"
	"syscall"
)

func setDetached(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
