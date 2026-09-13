package storage

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

const (
	migrationLockName = "migration.lock"
	migrationWait     = 30 * time.Second
)

type migrationLock struct {
	path  string
	file  *os.File
	owned bool
}

// acquireMigrationLock serializes schema initialization across processes.
// The lock is held only for the migration phase, so normal read access can
// continue after Open returns.
func acquireMigrationLock(paths *platform.Paths) (*migrationLock, error) {
	path := paths.Runtime + string(os.PathSeparator) + migrationLockName
	deadline := time.Now().Add(migrationWait)
	for {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if _, writeErr := fmt.Fprintf(f, "%d\n", os.Getpid()); writeErr != nil {
				_ = f.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("写入迁移锁失败: %w", writeErr)
			}
			return &migrationLock{path: path, file: f, owned: true}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("创建迁移锁失败: %w", err)
		}

		// A live owner is allowed to finish. A stale lock from a crashed
		// process can be removed after confirming its PID is no longer alive.
		pid := readLockPID(path)
		if pid <= 0 {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("迁移锁存在且无法确认所有者: %s", path)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if processAlive(pid) {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("等待迁移锁超时（PID %d）", pid)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("清理过期迁移锁失败: %w", err)
		}
	}
}

func readLockPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

func (l *migrationLock) Close() error {
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
