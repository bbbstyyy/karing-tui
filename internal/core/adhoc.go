package core

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// Adhoc 是一次性的 sing-box 实例（如批量测速），与主实例互不影响。
type Adhoc struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	exited chan struct{}

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
	go func() {
		err := cmd.Wait()
		if err != nil {
			out.AppendLine(fmt.Sprintf("[karing] 临时实例退出: %v", err))
		}
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

// Stop 停止临时实例：先 SIGTERM，超时后 SIGKILL。
func (a *Adhoc) Stop() {
	if a.cmd == nil || a.cmd.Process == nil {
		return
	}
	killGroup(a.cmd.Process.Pid, syscallSIGTERM)
	select {
	case <-a.exited:
	case <-time.After(3 * time.Second):
		killGroup(a.cmd.Process.Pid, syscallSIGKILL)
		<-a.exited
	}
	a.cancel()
}
