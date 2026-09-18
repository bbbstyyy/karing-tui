//go:build darwin

package procidentity

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Lookup 用 kern.proc.pid 读取进程启动时间作为身份 token。
//
// 实测（x/sys v0.47.0）：**进程不存在时 SysctlKinfoProc 返回 EIO 而不是
// ESRCH**——它内部以 `n != SizeofKinfoProc` 判定，取不到数据即返回 EIO。
// 因此这两个 errno 都归入 ErrProcessGone；漏掉 EIO 会让「进程已死」被当成
// 「探测出错」，进而让陈旧锁与 stale state 永远清不掉。
func Lookup(pid int) (Identity, error) {
	if pid <= 0 {
		return Identity{}, fmt.Errorf("非法 PID: %d", pid)
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH) {
			return Identity{}, ErrProcessGone
		}
		return Identity{}, fmt.Errorf("读取进程 %d 身份失败: %w", pid, err)
	}
	tv := kp.Proc.P_starttime
	return Identity{PID: pid, Token: fmt.Sprintf("%d:%d", tv.Sec, tv.Usec)}, nil
}

// GroupID 从同一份 kinfo_proc 里取进程组 ID（Eproc.Pgid）。
func GroupID(pid int) (int, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("非法 PID: %d", pid)
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH) {
			return 0, ErrProcessGone
		}
		return 0, fmt.Errorf("读取进程 %d 进程组失败: %w", pid, err)
	}
	return int(kp.Eproc.Pgid), nil
}
