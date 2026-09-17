package core

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// State 是 sing-box 进程状态。
type State int

const (
	StateStopped State = iota
	StateRunning
)

func (s State) String() string {
	if s == StateRunning {
		return "运行中"
	}
	return "已停止"
}

// Status 是运行状态快照。
type Status struct {
	State     State
	Version   string
	StartedAt time.Time // 零值表示未运行
	Uptime    time.Duration
}

// Manager 管理 sing-box 进程生命周期：Start / Stop / Restart、启动前校验、日志采集。
type Manager struct {
	paths *platform.Paths
	bin   *BinaryManager

	// Output 是 sing-box stdout/stderr 的环形缓冲，供 TUI 实时查看。
	Output *LogBuf

	mu        sync.Mutex
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	exited    chan struct{} // 进程退出信号，每次 Start 重建
	logFile   io.WriteCloser
	running   bool
	startedAt time.Time
	stopping  bool // 正在主动停止，用于区分崩溃
	stuck     bool // 停止超时且进程组仍存活；Start/Restart 拒绝新实例（C16）

	// stop 汇集停止流程可注入参数（timeout / signal / probe），
	// 零值取默认实现；仅供测试在启动前覆盖。
	stop stopCtl

	// OnExit 在 sing-box 进程退出时回调（含崩溃）；参数为退出错误（主动停止为 nil）。
	OnExit func(err error)
}

// NewManager 创建运行核心管理器。
func NewManager(paths *platform.Paths, bin *BinaryManager) *Manager {
	return &Manager{
		paths:  paths,
		bin:    bin,
		Output: NewLogBuf(1000),
	}
}

// Check 运行 `sing-box check` 校验配置文件。
func (m *Manager) Check(ctx context.Context, configPath string) error {
	bin, err := m.bin.Resolve()
	if err != nil {
		return err
	}
	if err := m.bin.ValidateVersion(ctx, bin); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "check", "-c", configPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("配置校验失败: %s", tailBytes(out, 2000))
	}
	return nil
}

// Status 返回当前状态快照。
func (m *Manager) Status() Status {
	m.mu.Lock()
	running := m.running
	startedAt := m.startedAt
	m.mu.Unlock()

	st := Status{State: StateStopped}
	if running {
		st.State = StateRunning
		st.StartedAt = startedAt
		st.Uptime = time.Since(startedAt).Round(time.Second)
	}
	if v, err := m.bin.Version(context.Background()); err == nil {
		st.Version = v
	}
	return st
}

// Start 启动 sing-box。要求配置已生成并已通过 Check。
func (m *Manager) Start(ctx context.Context, configPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.stuck {
		// 上一次停止两段超时后进程组仍存活，新实例可能与残留进程抢端口（C16 第 6 条）。
		m.mu.Unlock()
		return fmt.Errorf("上一次停止未完成（可能存在残留子进程），请稍后重试")
	}
	if m.running {
		m.mu.Unlock()
		return fmt.Errorf("sing-box 已在运行中")
	}
	bin, err := m.bin.Resolve()
	if err != nil {
		m.mu.Unlock()
		return err
	}

	logPath := filepath.Join(m.paths.Logs, "sing-box.log")
	logFile, err := NewRotateWriter(logPath, DefaultLogMaxBytes, DefaultLogKeep)
	if err != nil {
		m.mu.Unlock()
		return err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(runCtx, bin, "run", "-c", configPath, "-D", m.paths.Cache)
	cmd.Stdout = io.MultiWriter(logFile, m.Output)
	cmd.Stderr = io.MultiWriter(logFile, m.Output)
	setPgid(cmd)

	exited := make(chan struct{})
	m.stopping = false
	if err := cmd.Start(); err != nil {
		cancel()
		logFile.Close()
		m.mu.Unlock()
		return fmt.Errorf("启动 sing-box 失败: %w", err)
	}

	m.cmd = cmd
	m.cancel = cancel
	m.exited = exited
	m.logFile = logFile
	m.running = true
	m.startedAt = time.Now()

	go m.watch(cmd, logFile, cancel, exited)
	m.mu.Unlock()

	// 端口占用等启动期错误会在几百毫秒内退出，等待并确认。
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return fmt.Errorf("sing-box 启动后立即退出，最近日志:\n%s", m.Output.Tail(10))
		case <-ctx.Done():
			_ = m.Stop()
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}

// watch 等待进程退出并清理资源。
func (m *Manager) watch(cmd *exec.Cmd, logFile io.WriteCloser, cancel context.CancelFunc, exited chan struct{}) {
	err := cmd.Wait()

	// C16 第 5 条：cmd.Wait 只回收 direct child，同 PGID 的 descendant
	// 可能仍存活并持有继承的管道（这正是 Wait 迟迟不返回的常见原因）。
	// 状态转为 stopped 的最终条件必须包含「目标进程组已不存在」。
	// sing-box 正常不派生子进程，此探测在生产路径上几乎立即通过。
	pgid := 0
	if cmd.Process != nil {
		pgid = cmd.Process.Pid
	}
	if pgid > 0 {
		awaitGroupGone(pgid, m.stopCtl())
	}

	m.mu.Lock()
	wasStopping := m.stopping
	m.cmd = nil
	m.cancel = nil
	m.logFile = nil
	m.running = false
	m.stuck = false // stuck 期间进程组一旦消失，在这里恢复 stopped
	m.startedAt = time.Time{}
	m.mu.Unlock()

	cancel()
	logFile.Close()
	close(exited)

	if !wasStopping {
		m.Output.AppendLine(fmt.Sprintf("[karing] sing-box 异常退出: %v", err))
		if m.OnExit != nil {
			m.OnExit(err)
		}
	}
}

// IsRunning 报告 sing-box 是否正在运行。
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// PID 返回当前 sing-box PID；未运行时返回 0。
func (m *Manager) PID() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil || m.cmd.Process == nil || !m.running {
		return 0
	}
	return m.cmd.Process.Pid
}

// stopCtl 返回补全默认值后的停止参数快照。
func (m *Manager) stopCtl() stopCtl {
	return m.stop.withDefaults()
}

// Stop 停止 sing-box：先对进程组 SIGTERM，超时后 SIGKILL，再超时则进入
// stuck 并返回 ErrProcessGroupDidNotExit（C16）。
//
// exited 由 watch 保证在「direct child 已回收且进程组已消失」后关闭，
// 因此等待 exited 即等待停止的最终条件成立，不会无限阻塞。
func (m *Manager) Stop() error {
	m.mu.Lock()
	if !m.running || m.cmd == nil {
		m.mu.Unlock()
		return nil
	}
	m.stopping = true
	cmd := m.cmd
	exited := m.exited
	pgid := 0
	if cmd.Process != nil {
		pgid = cmd.Process.Pid
	}
	m.mu.Unlock()

	// 防御：pgid<=0 时 kill(-pgid) 的语义会变成「发给调用者自己的进程组」，
	// 绝不能执行。正常路径 cmd.Process 恒非空。
	if pgid <= 0 {
		<-exited
		return nil
	}

	ctl := m.stopCtl()
	warn := func(err error) {
		m.Output.AppendLine(fmt.Sprintf("[karing] 向进程组发送信号失败: %v", err))
	}
	if stopProcess(pgid, exited, ctl, warn) {
		return nil
	}

	// 两段宽限后进程组仍存活：进入 stuck，Start/Restart 拒绝新实例；
	// watch 在组最终消失后完成收尾并恢复 stopped（C16 第 6/7 条）。
	m.mu.Lock()
	if !m.running {
		// watch 恰在超时判定与加锁之间完成收尾，进程组实际已消失。
		m.mu.Unlock()
		return nil
	}
	m.stuck = true
	m.mu.Unlock()

	m.Output.AppendLine("[karing] 进程组停止超时，可能存在残留子进程")
	return &ErrProcessGroupDidNotExit{Group: pgid}
}

// Restart 重启 sing-box。
func (m *Manager) Restart(ctx context.Context, configPath string) error {
	if err := m.Stop(); err != nil {
		return fmt.Errorf("停止旧进程失败: %w", err)
	}
	return m.Start(ctx, configPath)
}

func tailBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
