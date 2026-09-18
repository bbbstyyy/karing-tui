//go:build darwin

package procidentity

import (
	"errors"
	"os"
	"testing"
)

// TestLookupEIOOnMissingProcess 钉住 Darwin 上「进程不存在」的 errno 语义。
//
// 实测（x/sys v0.47.0）：SysctlKinfoProc 在进程不存在时返回 **EIO** 而不是 ESRCH。
// 只把 ESRCH 当「已消失」会让「进程已死」被归到「探测出错」，于是陈旧锁与 stale
// state 永远清不掉。本用例在两个平台都要求 ErrProcessGone（Linux 侧由
// lookup_linux_test.go 覆盖）。
func TestLookupEIOOnMissingProcess(t *testing.T) {
	pid := 999999 // 超过 kern.maxproc 的号码，必然不存在
	_, err := Lookup(pid)
	if !errors.Is(err, ErrProcessGone) {
		t.Fatalf("不存在的 PID 应返回 ErrProcessGone（EIO/ESRCH 都算），实际 %v", err)
	}
}

func TestLookupOnSelfDarwin(t *testing.T) {
	id, err := Lookup(os.Getpid())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if id.Token == "" {
		t.Fatal("Darwin token 不应为空")
	}
	if !Matches(id) {
		t.Fatal("Matches 对自身应为真")
	}
	if Matches(Identity{PID: os.Getpid(), Token: id.Token + ":junk"}) {
		t.Fatal("token 被改写后不得判为同一进程")
	}
	if Matches(Identity{PID: os.Getpid()}) {
		t.Fatal("空 token 必须 fail-closed（不得退回裸 PID）")
	}
}
