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

	m.mu.Lock()
	wasStopping := m.stopping
	m.cmd = nil
	m.cancel = nil
	m.logFile = nil
	m.running = false
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

// Stop 停止 sing-box：先 SIGTERM，超时后 SIGKILL。
func (m *Manager) Stop() error {
	m.mu.Lock()
	if !m.running || m.cmd == nil {
		m.mu.Unlock()
		return nil
	}
	m.stopping = true
	cmd := m.cmd
	exited := m.exited
	m.mu.Unlock()

	killGroup(cmd.Process.Pid, syscallSIGTERM)

	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		killGroup(cmd.Process.Pid, syscallSIGKILL)
		<-exited
	}
	return nil
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
