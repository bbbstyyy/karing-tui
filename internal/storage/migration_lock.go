package storage

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

const migrationLockName = "migration.lock"

// migrationWait 是等待活持有者跑完迁移的上限。
//
// 声明为变量而非常量：测试需要把它缩短到毫秒级，否则每条「等待」与
// 「身份不可验证则 fail-closed」的用例都要真等 30 秒。生产路径不修改它。
var migrationWait = 30 * time.Second

type migrationLock struct {
	path  string
	file  *os.File
	owned bool
}

// acquireMigrationLock serializes schema initialization across processes.
// The lock is held only for the migration phase, so normal read access can
// continue after Open returns.
//
// 判定矩阵与 instance.lock 完全一致（V7-5，见 classifyLock），区别只在
// 「活持有者」与「身份不可验证」的处理方式：这里是等到时限再报错，而不是
// 立刻拒绝——迁移只持续很短时间，让活持有者跑完是正常的。
func acquireMigrationLock(paths *platform.Paths) (*migrationLock, error) {
	path := paths.Runtime + string(os.PathSeparator) + migrationLockName
	self := selfIdentity()
	deadline := time.Now().Add(migrationWait)
	for {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if writeErr := writeLockOwner(f, self); writeErr != nil {
				_ = f.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("写入迁移锁失败: %w", writeErr)
			}
			return &migrationLock{path: path, file: f, owned: true}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("创建迁移锁失败: %w", err)
		}

		owner, parseable := readLockOwner(path)
		switch classifyLock(owner, parseable) {
		case lockHeld:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("等待迁移锁超时（PID %d）", owner.pid)
			}
			time.Sleep(50 * time.Millisecond)
		case lockUnknown:
			// 旧单行格式 + 活 PID，或身份读不出来：绝不删锁，只等时限。
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("迁移锁存在且无法确认所有者身份，请手工确认后删除 %s", path)
			}
			time.Sleep(50 * time.Millisecond)
		case lockStale:
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("清理过期迁移锁失败: %w", err)
			}
		}
	}
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
