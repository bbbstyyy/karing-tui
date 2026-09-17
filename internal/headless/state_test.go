package headless

import (
	"os"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

func testPaths(t *testing.T) *platform.Paths {
	t.Helper()
	t.Setenv("KARING_HOME", t.TempDir())
	p, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStateRoundTripAndAtomicWrite(t *testing.T) {
	p := testPaths(t)
	self, err := processIdentity(os.Getpid())
	if err != nil {
		t.Fatalf("processIdentity: %v", err)
	}
	want := State{
		Status: StatusRunning, SupervisorPID: os.Getpid(), SupervisorStartToken: self.Token,
		CorePID: 42, CoreStartToken: "tok", CorePGID: 42,
		StartedAt: time.Now().Truncate(time.Second), MixedPort: 24080,
	}
	if err := WriteState(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != want.Status || got.SupervisorPID != want.SupervisorPID || got.CorePID != want.CorePID || got.MixedPort != want.MixedPort {
		t.Fatalf("state mismatch: got %+v want %+v", got, want)
	}
	// V5-2 新增的身份字段必须真的落盘并读回，否则重启后一切身份判定都会失败。
	if got.SupervisorStartToken != want.SupervisorStartToken {
		t.Fatalf("SupervisorStartToken 未往返: %q", got.SupervisorStartToken)
	}
	if got.CoreStartToken != want.CoreStartToken || got.CorePGID != want.CorePGID {
		t.Fatalf("core 身份字段未往返: %q / %d", got.CoreStartToken, got.CorePGID)
	}
	// 身份可验证时 Active 为真；把 token 抹掉就必须变假（fail-closed）。
	if !Active(got) {
		t.Fatal("当前进程应被判定为 active")
	}
	got.SupervisorStartToken = ""
	if Active(got) {
		t.Fatal("缺 start token 时不得判定为 active")
	}
}

func TestLockAndStopRequest(t *testing.T) {
	p := testPaths(t)
	l, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(p); err == nil {
		t.Fatal("second lock unexpectedly succeeded")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := RequestStop(p); err != nil {
		t.Fatal(err)
	}
	if !ConsumeStopForTest(p) || ConsumeStopForTest(p) {
		t.Fatal("stop request consumption mismatch")
	}
}

func TestRunServiceLockConflictPreservesState(t *testing.T) {
	p := testPaths(t)
	want := State{Status: StatusRunning, SupervisorPID: os.Getpid(), CorePID: 42}
	if err := WriteState(p, want); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if code := RunService(p); code == 0 {
		t.Fatal("锁冲突时 RunService 应失败")
	}
	got, err := ReadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != want.Status || got.CorePID != want.CorePID {
		t.Fatalf("锁冲突不应覆盖已有状态: got %+v want %+v", got, want)
	}
}
