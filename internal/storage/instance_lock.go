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

// AcquireInstance acquires the application-wide lock.
//
// 锁文件记录「PID + 进程身份 token」（V7-5，格式见 lockOwner）。判定矩阵见
// classifyLock：只有「持有者确认已消失」或「PID 被无关进程复用」才允许清理，
// 「无法证明身份」一律拒绝并保持锁不动。
//
// 不能退回「裸 PID + kill -0」：PID 复用会把无关进程误判成持有者，让用户
// 永远启动不了（旧行为），或者在更坏的方向上删掉活进程的锁。
func AcquireInstance(paths *platform.Paths) (*InstanceLock, error) {
	path := filepath.Join(paths.Runtime, instanceLockName)
	self := selfIdentity()
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if writeErr := writeLockOwner(f, self); writeErr != nil {
				_ = f.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("写入应用实例锁失败: %w", writeErr)
			}
			return &InstanceLock{path: path, file: f, owned: true}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("创建应用实例锁失败: %w", err)
		}

		owner, parseable := readLockOwner(path)
		switch classifyLock(owner, parseable) {
		case lockHeld:
			return nil, fmt.Errorf("应用已在运行（PID %d）", owner.pid)
		case lockUnknown:
			// 绝不清理：无法证明对方不是活着的 karing-tui。
			return nil, fmt.Errorf("应用实例锁存在且无法确认所有者身份，请确认没有其它实例后手工删除 %s", path)
		case lockStale:
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("清理过期应用实例锁失败: %w", err)
			}
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
