// Package headless 提供无 TUI 场景下的 sing-box supervisor。
package headless

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

const (
	stateFile = "headless.json"
	lockFile  = "headless.lock"
	stopFile  = "headless.stop"
)

const (
	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusStopped  = "stopped"
	StatusCrashed  = "crashed"
)

// State 是 supervisor 的跨进程状态快照。字段保持稳定，供脚本读取。
type State struct {
	Status        string    `json:"status"`
	SupervisorPID int       `json:"supervisor_pid,omitempty"`
	CorePID       int       `json:"core_pid,omitempty"`
	StartedAt     time.Time `json:"started_at,omitempty"`
	StoppedAt     time.Time `json:"stopped_at,omitempty"`
	ExitError     string    `json:"exit_error,omitempty"`
	Version       string    `json:"core_version,omitempty"`
	MixedPort     int       `json:"mixed_port,omitempty"`
	Config        string    `json:"config,omitempty"`
}

var stateMu sync.Mutex

func statePath(paths *platform.Paths) string { return filepath.Join(paths.Runtime, stateFile) }
func lockPath(paths *platform.Paths) string  { return filepath.Join(paths.Runtime, lockFile) }
func stopPath(paths *platform.Paths) string  { return filepath.Join(paths.Runtime, stopFile) }

// ReadState 读取最近一次 supervisor 状态；没有状态文件时返回 stopped。
func ReadState(paths *platform.Paths) (State, error) {
	b, err := os.ReadFile(statePath(paths))
	if errors.Is(err, os.ErrNotExist) {
		return State{Status: StatusStopped}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("解析 headless 状态失败: %w", err)
	}
	if st.Status == "" {
		st.Status = StatusStopped
	}
	return st, nil
}

// WriteState 原子写入状态文件，避免 status 读到半个 JSON。
func WriteState(paths *platform.Paths, st State) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	if err := os.MkdirAll(paths.Runtime, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(paths.Runtime, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(paths.Runtime, ".headless-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, statePath(paths))
}

// RequestStop 请求 supervisor 优雅退出。使用文件协议在 Linux/macOS 上工作。
func RequestStop(paths *platform.Paths) error {
	if err := os.WriteFile(stopPath(paths), []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0o600); err != nil {
		return fmt.Errorf("写入停止请求失败: %w", err)
	}
	return nil
}

func consumeStop(paths *platform.Paths) bool {
	if _, err := os.Stat(stopPath(paths)); err != nil {
		return false
	}
	_ = os.Remove(stopPath(paths))
	return true
}

// ClearStop 清除上一次遗留的停止请求。
func ClearStop(paths *platform.Paths) { _ = os.Remove(stopPath(paths)) }

// Active reports whether a recorded supervisor PID is still alive.
func Active(st State) bool {
	return (st.Status == StatusRunning || st.Status == StatusStarting) && processAlive(st.SupervisorPID)
}

// StopRecordedCore terminates a core left behind by a supervisor that exited
// unexpectedly. The recorded PID is only used when the supervisor is already
// known to be dead, so normal stop requests continue to go through the
// supervisor's graceful shutdown path.
func StopRecordedCore(st State) error {
	if st.CorePID <= 0 || processAlive(st.SupervisorPID) {
		return nil
	}
	return stopRecordedCore(st.CorePID)
}

// Lock 表示当前 headless supervisor 对数据目录的独占权。
type Lock struct {
	paths *platform.Paths
	path  string
	pid   int
	owned bool
}

// Acquire 获取 supervisor 锁；只清理确认已经退出的旧 PID。
func Acquire(paths *platform.Paths) (*Lock, error) {
	l := &Lock{paths: paths, path: lockPath(paths), pid: os.Getpid()}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", l.pid)
			_ = f.Close()
			l.owned = true
			return l, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("创建 headless 锁失败: %w", err)
		}
		b, readErr := os.ReadFile(l.path)
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(b)))
		if readErr != nil || parseErr != nil || pid <= 0 || processAlive(pid) {
			if pid > 0 && processAlive(pid) {
				return nil, fmt.Errorf("headless 已在运行（supervisor PID %d）", pid)
			}
			return nil, fmt.Errorf("headless 锁文件存在且无法确认旧实例已退出: %s", l.path)
		}
		if err := os.Remove(l.path); err != nil {
			return nil, fmt.Errorf("清理旧 headless 锁失败: %w", err)
		}
	}
	return nil, fmt.Errorf("获取 headless 锁失败")
}

func (l *Lock) Close() error {
	if l == nil || !l.owned {
		return nil
	}
	l.owned = false
	return os.Remove(l.path)
}

// ConsumeStopForTest 保留给同包测试，避免暴露文件协议细节。
func ConsumeStopForTest(paths *platform.Paths) bool { return consumeStop(paths) }
