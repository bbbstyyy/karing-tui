//go:build linux || darwin

package core

import (
	"os/exec"
	"syscall"
)

// syscallSIGTERM / syscallSIGKILL 是 Unix 信号常量。
const (
	syscallSIGTERM = syscall.SIGTERM
	syscallSIGKILL = syscall.SIGKILL
)

// setPgid 将子进程放入独立进程组，避免终端信号波及 sing-box。
func setPgid(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup 向进程组发送信号。
func killGroup(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig)
}
