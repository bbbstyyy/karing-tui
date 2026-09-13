package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

const instanceLockName = "instance.lock"

// InstanceLock serializes processes that own the application database.  The
// lock is deliberately separate from migration.lock: it remains held for the
// lifetime of the TUI/headless application, not just schema initialization.
type InstanceLock struct {
	path  string
	file  *os.File
	owned bool
}

// AcquireInstance acquires the application-wide lock. A lock left by a
// crashed process is removed only after its PID is confirmed dead.
func AcquireInstance(paths *platform.Paths) (*InstanceLock, error) {
	path := filepath.Join(paths.Runtime, instanceLockName)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if _, writeErr := fmt.Fprintf(f, "%d\n", os.Getpid()); writeErr != nil {
				_ = f.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("写入应用实例锁失败: %w", writeErr)
			}
			return &InstanceLock{path: path, file: f, owned: true}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("创建应用实例锁失败: %w", err)
		}

		pid := readLockPID(path)
		if pid > 0 && processAlive(pid) {
			return nil, fmt.Errorf("应用已在运行（PID %d）", pid)
		}
		if pid <= 0 {
			return nil, fmt.Errorf("应用实例锁存在且无法确认所有者: %s", path)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("清理过期应用实例锁失败: %w", err)
		}
	}
	return nil, fmt.Errorf("获取应用实例锁失败")
}

// Close releases the lock. It is safe to call more than once.
func (l *InstanceLock) Close() error {
	if l == nil || !l.owned {
		return nil
	}
	l.owned = false
	if err := l.file.Close(); err != nil {
		_ = os.Remove(l.path)
		return err
	}
	return os.Remove(l.path)
}
