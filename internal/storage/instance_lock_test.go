package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/procidentity"
)

// deadPID 是一个不可能存在的号码，用来构造「持有者已消失」。
// 两个平台都会让 Lookup 返回 ErrProcessGone（Linux 无 /proc/<pid>，
// Darwin 的 SysctlKinfoProc 返回 EIO）。
const deadPID = 2147483647

func lockTestPaths(t *testing.T) *platform.Paths {
	t.Helper()
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

// currentIdentity 返回当前测试进程的身份——它就是「活着的持有者」。
func currentIdentity(t *testing.T) procidentity.Identity {
	t.Helper()
	id, err := procidentity.Current()
	if err != nil {
		t.Skipf("本机读不到进程身份，跳过: %v", err)
	}
	return id
}

func writeRawLock(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInstanceLockSerializesOwners(t *testing.T) {
	paths := lockTestPaths(t)
	first, err := AcquireInstance(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireInstance(paths); err == nil {
		t.Fatal("第二个实例不应获取锁")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireInstance(paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(paths.Runtime, instanceLockName)); !os.IsNotExist(err) {
		t.Fatalf("锁文件未清理: %v", err)
	}
}

func TestInstanceLockRemovesDeadOwner(t *testing.T) {
	paths := lockTestPaths(t)
	path := filepath.Join(paths.Runtime, instanceLockName)
	writeRawLock(t, path, fmt.Sprintf("%d\n", deadPID))
	lock, err := AcquireInstance(paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

// 锁文件里是「活 PID + 匹配 token」时必须拒绝，且不得动锁文件。
func TestInstanceLockRejectsSameLiveProcessIdentity(t *testing.T) {
	paths := lockTestPaths(t)
	path := filepath.Join(paths.Runtime, instanceLockName)
	id := currentIdentity(t)
	writeRawLock(t, path, fmt.Sprintf("%d\n%s\n", id.PID, id.Token))

	if _, err := AcquireInstance(paths); err == nil {
		t.Fatal("同一活进程身份的锁必须拒绝第二实例")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("拒绝时不得清理锁文件: %v", err)
	}
}

// PID 被无关进程复用（活号码 + 不匹配的 token）时必须能自动清理。
//
// 判决性：旧实现只做 `kill -0`，看到号码活着就报「应用已在运行」，
// 用户被永久挡在启动之外，只能手工删锁。
func TestInstanceLockCleansReusedPID(t *testing.T) {
	paths := lockTestPaths(t)
	path := filepath.Join(paths.Runtime, instanceLockName)
	id := currentIdentity(t)
	writeRawLock(t, path, fmt.Sprintf("%d\n%s\n", id.PID, id.Token+":reused"))

	lock, err := AcquireInstance(paths)
	if err != nil {
		t.Fatalf("PID 被复用时锁应可清理，实际 %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

// 旧单行格式 + 活 PID：无法证明身份，必须 fail-closed 且不碰锁文件。
//
// 判决性：这是「宁可拒绝自动清理，也不猜它已经走了」的那条边界。
// 放宽它会让第二实例与活着的第一个实例同时写同一个数据库。
func TestInstanceLockLegacyLivePIDFailsClosed(t *testing.T) {
	paths := lockTestPaths(t)
	path := filepath.Join(paths.Runtime, instanceLockName)
	id := currentIdentity(t)
	writeRawLock(t, path, fmt.Sprintf("%d\n", id.PID))

	if _, err := AcquireInstance(paths); err == nil {
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

// 旧单行格式 + 已死 PID：号码层面就是正面证据，可以清理。
func TestInstanceLockLegacyDeadPIDCanBeCleaned(t *testing.T) {
	paths := lockTestPaths(t)
	path := filepath.Join(paths.Runtime, instanceLockName)
	writeRawLock(t, path, fmt.Sprintf("%d\n", deadPID))

	lock, err := AcquireInstance(paths)
	if err != nil {
		t.Fatalf("死 PID 的旧格式锁应可清理，实际 %v", err)
	}
	// 新锁必须写成两行（PID + token），后续实例才具备可验证身份。
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\n") {
		t.Fatalf("新锁应为 PID + token 两行，实得 %q", b)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestClassifyLockMatrix 把判定矩阵逐行钉住（含「身份读不出来」这一档）。
func TestClassifyLockMatrix(t *testing.T) {
	id := currentIdentity(t)
	cases := []struct {
		name      string
		owner     lockOwner
		parseable bool
		want      lockVerdict
	}{
		{"token 匹配 + 进程存在", lockOwner{pid: id.PID, token: id.Token}, true, lockHeld},
		{"token 不匹配 + 进程存在", lockOwner{pid: id.PID, token: id.Token + ":x"}, true, lockStale},
		{"进程不存在", lockOwner{pid: deadPID, token: "whatever"}, true, lockStale},
		{"旧单行格式 + 活 PID", lockOwner{pid: id.PID}, true, lockUnknown},
		{"旧单行格式 + 死 PID", lockOwner{pid: deadPID}, true, lockStale},
		{"内容解析不出 PID", lockOwner{}, false, lockUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyLock(tc.owner, tc.parseable); got != tc.want {
				t.Fatalf("classifyLock = %v, 期望 %v", got, tc.want)
			}
		})
	}
}
