package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// migration.lock 与 instance.lock 共用同一套判定矩阵（V7-5，见 classifyLock）。
// 单独成文件是因为两者对「活持有者」与「身份不可验证」的**处理方式**不同：
// 迁移锁会等到时限再报错，而不是立刻拒绝。

// shortenMigrationWait 把等待上限压到毫秒级并保证测试结束后还原。
func shortenMigrationWait(t *testing.T) {
	t.Helper()
	orig := migrationWait
	migrationWait = 30 * time.Millisecond
	t.Cleanup(func() { migrationWait = orig })
}

func migrationLockPath(t *testing.T, paths *platform.Paths) string {
	t.Helper()
	return filepath.Join(paths.Runtime, migrationLockName)
}

// 活持有者（token 匹配）：必须等待而不是抢占。
func TestMigrationLockWaitsForLiveOwner(t *testing.T) {
	shortenMigrationWait(t)
	paths := lockTestPaths(t)
	path := migrationLockPath(t, paths)
	id := currentIdentity(t)
	writeRawLock(t, path, fmt.Sprintf("%d\n%s\n", id.PID, id.Token))

	if _, err := acquireMigrationLock(paths); err == nil {
		t.Fatal("活持有者仍在时不得抢到迁移锁")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("等待超时后不得清理锁文件: %v", err)
	}
}

// PID 被复用（活号码 + 不匹配 token）：可以清理并取得锁。
func TestMigrationLockCleansReusedPID(t *testing.T) {
	shortenMigrationWait(t)
	paths := lockTestPaths(t)
	path := migrationLockPath(t, paths)
	id := currentIdentity(t)
	writeRawLock(t, path, fmt.Sprintf("%d\n%s\n", id.PID, id.Token+":reused"))

	lock, err := acquireMigrationLock(paths)
	if err != nil {
		t.Fatalf("PID 被复用时迁移锁应可清理，实际 %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

// 旧单行格式 + 活 PID：fail-closed，等满时限报错且不改锁文件。
func TestMigrationLockLegacyLivePIDFailsClosed(t *testing.T) {
	shortenMigrationWait(t)
	paths := lockTestPaths(t)
	path := migrationLockPath(t, paths)
	id := currentIdentity(t)
	writeRawLock(t, path, fmt.Sprintf("%d\n", id.PID))

	if _, err := acquireMigrationLock(paths); err == nil {
		t.Fatal("旧单行格式 + 活 PID 必须 fail-closed")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fail-closed 时锁文件必须仍在: %v", err)
	}
	if strings.TrimSpace(string(b)) != fmt.Sprint(id.PID) {
		t.Fatalf("fail-closed 时不得改写锁文件，实得 %q", b)
	}
}

// 已死 PID：号码层面即正面证据，可清理。
func TestMigrationLockCleansDeadOwner(t *testing.T) {
	shortenMigrationWait(t)
	paths := lockTestPaths(t)
	path := migrationLockPath(t, paths)
	writeRawLock(t, path, fmt.Sprintf("%d\n", deadPID))

	lock, err := acquireMigrationLock(paths)
	if err != nil {
		t.Fatalf("死 PID 的迁移锁应可清理，实际 %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
