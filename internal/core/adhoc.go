package core

import (
	"context"
	"fmt"
	"os/exec"
)

// Adhoc 是一次性的 sing-box 实例（如批量测速），与主实例互不影响。
type Adhoc struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	exited chan struct{}

	// stop 汇集停止流程可注入参数（timeout / signal / probe），
	// 零值取默认实现；仅供测试覆盖。
	stop stopCtl

	// Output 是临时实例 stdout/stderr 的环形缓冲，排错用。
	Output *LogBuf
}

// StartAdhoc 启动一个独立的 sing-box 实例（独立进程组、独立日志缓冲）。
// 调用方负责在用完后调用 Stop。
func StartAdhoc(ctx context.Context, bin, configPath, cacheDir string) (*Adhoc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(runCtx, bin, "run", "-c", configPath, "-D", cacheDir)
	out := NewLogBuf(200)
	cmd.Stdout = out
	cmd.Stderr = out
	setPgid(cmd)

	a := &Adhoc{cmd: cmd, cancel: cancel, exited: make(chan struct{}), Output: out}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("启动临时 sing-box 实例失败: %w", err)
	}

	// pgid 在 Start 后即固定（Setpgid 使 child 成为组长）。
	pgid := cmd.Process.Pid
	ctl := a.stop.withDefaults()
	go func() {
		err := cmd.Wait()
		if err != nil {
			out.AppendLine(fmt.Sprintf("[karing] 临时实例退出: %v", err))
		}
		// C16：与 Manager.watch 一致，exited 关闭前确认进程组已消失
		// （direct child 退出不代表同组 descendant 不在了）。
		awaitGroupGone(pgid, ctl)
		close(a.exited)
	}()
	return a, nil
}

// Exited 报告进程是否已退出。
func (a *Adhoc) Exited() bool {
	select {
	case <-a.exited:
		return true
	default:
		return false
	}
}

// Stop 停止临时实例：先对进程组 SIGTERM，超时后 SIGKILL。
// 与 Manager.Stop 相同的两段序列与组消失语义；两段超时后不再等待，
// 把超时事实写入 Output（C16 第 9 条）。
func (a *Adhoc) Stop() {
	if a.cmd == nil || a.cmd.Process == nil {
		return
	}
	pgid := a.cmd.Process.Pid
	ctl := a.stop.withDefaults()
	warn := func(err error) {
		a.Output.AppendLine(fmt.Sprintf("[karing] 向进程组发送信号失败: %v", err))
	}
	if stopProcess(pgid, a.exited, ctl, warn) {
		if a.cancel != nil {
			a.cancel()
		}
		return
	}
	a.Output.AppendLine("[karing] 临时实例进程组停止超时，可能存在残留子进程")
	if a.cancel != nil {
		a.cancel()
	}
}
