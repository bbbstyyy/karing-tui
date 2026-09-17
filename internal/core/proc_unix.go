//go:build linux || darwin

package core

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// syscallSIGTERM / syscallSIGKILL 是 Unix 信号常量。
const (
	syscallSIGTERM = syscall.SIGTERM
	syscallSIGKILL = syscall.SIGKILL
)

// setPgid 将子进程放入独立进程组，避免终端信号波及 sing-box。
// 启动后 child PID 即可作为该进程组 PGID（Setpgid 使其成为组长）。
func setPgid(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// errProcessGroupGone 由 killGroup 返回：目标进程组已不存在（ESRCH）。
// 停止流程中这等价于成功——目标就是要它消失。
var errProcessGroupGone = errors.New("进程组已不存在")

// killGroup 向 pgid 进程组发送信号（负 PGID）。
// ESRCH 视为「进程组已不存在」，返回 errProcessGroupGone；其它错误原样上报。
// 此前错误被丢弃，SIGKILL 是否发出无从判断，Stop 只能对 `<-exited` 无限等待（C16）。
func killGroup(pgid int, sig syscall.Signal) error {
	switch err := syscall.Kill(-pgid, sig); {
	case err == nil:
		return nil
	case errors.Is(err, syscall.ESRCH):
		return errProcessGroupGone
	default:
		return err
	}
}

// processGroupAlive 探测进程组是否仍存在（signal 0 不实际投递信号）：
//   - nil   → 存在
//   - EPERM → 存在（无权限发送信号，但组在）
//   - ESRCH → 已消失
func processGroupAlive(pgid int) (bool, error) {
	if pgid <= 0 {
		return false, fmt.Errorf("非法进程组 ID: %d", pgid)
	}
	switch err := syscall.Kill(-pgid, 0); {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}

// signalFunc 是停止流程可注入的「发送信号」缝（测试记录调用序列用）。
type signalFunc func(pgid int, sig syscall.Signal) error

// probeFunc 是停止/收尾流程可注入的「进程组存活探测」缝。
type probeFunc func(pgid int) (bool, error)

// realSignal / realProbe 是生产实现。
func realSignal(pgid int, sig syscall.Signal) error { return killGroup(pgid, sig) }

func realProbe(pgid int) (bool, error) { return processGroupAlive(pgid) }
