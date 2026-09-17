//go:build linux || darwin

package headless

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fastShutdownCtl() shutdownCtl {
	return shutdownCtl{
		termTimeout:   20 * time.Millisecond,
		killTimeout:   2 * time.Second,
		probeInterval: 2 * time.Millisecond,
	}
}

// selfIdentity 取当前测试进程的可验证身份（真实 start token）。
func selfIdentity(t *testing.T) ProcessIdentity {
	t.Helper()
	id, err := processIdentity(os.Getpid())
	if err != nil {
		t.Fatalf("processIdentity(自己): %v", err)
	}
	return id
}

// TestActiveRequiresStartToken 钉住 Active 的判据：只有「PID + token 匹配」
// 才算在运行；token 为空或写错都必须返回 false（fail-closed）。
func TestActiveRequiresStartToken(t *testing.T) {
	self := selfIdentity(t)
	base := State{Status: StatusRunning, SupervisorPID: os.Getpid()}

	if Active(base) {
		t.Error("缺 start token 的旧 state 不得判为在运行")
	}
	base.SupervisorStartToken = "0:0"
	if Active(base) {
		t.Error("token 不匹配不得判为在运行")
	}
	base.SupervisorStartToken = self.Token
	if !Active(base) {
		t.Error("PID + token 匹配应判为在运行")
	}
	base.Status = StatusStopped
	if Active(base) {
		t.Error("已停止状态不是 Active")
	}
}

// TestReconcileRunningSupervisorIsLeftAlone 正常运行中的 supervisor 不走
// orphan cleanup，行为与 V5-2 之前一致。
func TestReconcileRunningSupervisorIsLeftAlone(t *testing.T) {
	self := selfIdentity(t)
	in := State{Status: StatusRunning, SupervisorPID: os.Getpid(), SupervisorStartToken: self.Token}

	got, outcome, err := ReconcileRecordedState(in)
	if err != nil {
		t.Fatalf("ReconcileRecordedState: %v", err)
	}
	if outcome != ReconcileRunning {
		t.Fatalf("outcome = %v, 期望 ReconcileRunning", outcome)
	}
	if got.SupervisorPID != in.SupervisorPID || got.SupervisorStartToken != in.SupervisorStartToken {
		t.Fatalf("运行中的 state 不应被改动: %+v", got)
	}
}

// TestReconcileStaleStateIsCleared 记录在案的进程都不存在时，可以安全地把
// state 清成 stopped 并继续启动。
func TestReconcileStaleStateIsCleared(t *testing.T) {
	dead := deadPID(t)
	in := State{
		Status:        StatusRunning,
		SupervisorPID: dead,
		CorePID:       dead,
		CorePGID:      dead,
	}
	got, outcome, err := ReconcileRecordedState(in)
	if err != nil {
		t.Fatalf("ReconcileRecordedState: %v", err)
	}
	if outcome != ReconcileStale {
		t.Fatalf("outcome = %v, 期望 ReconcileStale", outcome)
	}
	if got.Status != StatusStopped || got.SupervisorPID != 0 || got.CorePID != 0 || got.CorePGID != 0 {
		t.Fatalf("stale state 应被清空: %+v", got)
	}
}

// TestReconcileCleansVerifiedOrphanCore supervisor 已死、core leader 身份
// 可验证且匹配：取得 PGID 所有权证据后清理 orphan。
func TestReconcileCleansVerifiedOrphanCore(t *testing.T) {
	h := spawnHelperGroup(t, "hold")
	id, err := processIdentity(h.pid)
	if err != nil {
		t.Fatalf("processIdentity(helper): %v", err)
	}
	in := State{
		Status:         StatusRunning,
		SupervisorPID:  deadPID(t),
		CorePID:        h.pid,
		CoreStartToken: id.Token,
		CorePGID:       h.pgid,
	}

	got, outcome, err := reconcileRecordedState(in, fastShutdownCtl())
	if err != nil {
		t.Fatalf("reconcileRecordedState: %v", err)
	}
	if outcome != ReconcileOrphanCleared {
		t.Fatalf("outcome = %v, 期望 ReconcileOrphanCleared", outcome)
	}
	h.wait()
	if pidAlive(h.pid) {
		t.Fatalf("orphan core %d 应已被清理", h.pid)
	}
	if alive, err := processGroupAlive(h.pgid); err != nil || alive {
		t.Fatalf("orphan 进程组应已消失: alive=%v err=%v", alive, err)
	}
	if got.Status != StatusStopped || got.CorePID != 0 || got.CorePGID != 0 {
		t.Fatalf("清理后 state 应归位: %+v", got)
	}
}

// TestReconcileCleansOnlyTheRecordedGroup 是「不误杀」的判决性用例：
// 同时存在两个进程组时，只清理 state 里记录的那一个。
func TestReconcileCleansOnlyTheRecordedGroup(t *testing.T) {
	victim := spawnHelperGroup(t, "hold")
	bystander := spawnHelperGroup(t, "hold")

	id, err := processIdentity(victim.pid)
	if err != nil {
		t.Fatal(err)
	}
	in := State{
		Status:         StatusRunning,
		SupervisorPID:  deadPID(t),
		CorePID:        victim.pid,
		CoreStartToken: id.Token,
		CorePGID:       victim.pgid,
	}
	if _, outcome, err := reconcileRecordedState(in, fastShutdownCtl()); err != nil || outcome != ReconcileOrphanCleared {
		t.Fatalf("清理失败: outcome=%v err=%v", outcome, err)
	}

	victim.wait()
	if pidAlive(victim.pid) {
		t.Error("记录在案的进程组应被清理")
	}
	// 反向核对：无关进程组必须毫发无伤（否则说明按数字乱杀）。
	if !pidAlive(bystander.pid) {
		t.Fatal("无关进程组被误杀")
	}
	if alive, err := processGroupAlive(bystander.pgid); err != nil || !alive {
		t.Fatalf("无关进程组应仍存活: alive=%v err=%v", alive, err)
	}
}

// TestReconcileLegacyStateWithLiveProcessIsBlocked 旧版无 token 且进程仍在：
// fail-closed——不发任何信号，也不允许启动第二份 core。
func TestReconcileLegacyStateWithLiveProcessIsBlocked(t *testing.T) {
	h := spawnHelperGroup(t, "hold")
	in := State{
		Status:        StatusRunning,
		SupervisorPID: os.Getpid(), // 活着的 supervisor（旧版无 token，无法验证）
		CorePID:       h.pid,
		CorePGID:      h.pgid,
	}

	got, outcome, err := reconcileRecordedState(in, fastShutdownCtl())
	if outcome != ReconcileBlocked {
		t.Fatalf("outcome = %v, 期望 ReconcileBlocked", outcome)
	}
	var unverifiable *ErrStateUnverifiable
	if !errors.As(err, &unverifiable) {
		t.Fatalf("应返回 ErrStateUnverifiable，实际 %v", err)
	}
	// 判决性：一个信号都不能发。
	if !pidAlive(h.pid) {
		t.Fatal("身份不可验证时绝不能对记录 PID 发信号")
	}
	if got.SupervisorPID != in.SupervisorPID || got.CorePID != in.CorePID {
		t.Fatalf("被拦下时不应改动 state: %+v", got)
	}
	if !strings.Contains(err.Error(), "无法验证") {
		t.Fatalf("错误文案应说明无法验证，实际 %q", err.Error())
	}
}

// TestReconcileMismatchedCoreTokenIsBlocked PID 被复用 / token 不匹配：
// 记录号码上确实有进程，但不是原来那个 —— 同样必须 fail-closed。
func TestReconcileMismatchedCoreTokenIsBlocked(t *testing.T) {
	h := spawnHelperGroup(t, "hold")
	in := State{
		Status:         StatusRunning,
		SupervisorPID:  deadPID(t),
		CorePID:        h.pid,
		CoreStartToken: "1:1", // 与真实 token 必然不同
		CorePGID:       h.pgid,
	}
	_, outcome, err := reconcileRecordedState(in, fastShutdownCtl())
	if outcome != ReconcileBlocked {
		t.Fatalf("outcome = %v, 期望 ReconcileBlocked", outcome)
	}
	var unverifiable *ErrStateUnverifiable
	if !errors.As(err, &unverifiable) {
		t.Fatalf("应返回 ErrStateUnverifiable，实际 %v", err)
	}
	if !pidAlive(h.pid) {
		t.Fatal("token 不匹配时绝不能对记录 PID 发信号")
	}
}

// TestReconcileRejectsInconsistentRecordedPGID 交叉核对：leader 活着时它实际
// 所属的进程组必须与记录一致；不一致就宁可不清理，也不能按来路不明的数字
// 去发信号。
func TestReconcileRejectsInconsistentRecordedPGID(t *testing.T) {
	h := spawnHelperGroup(t, "hold")
	id, err := processIdentity(h.pid)
	if err != nil {
		t.Fatal(err)
	}
	other := spawnHelperGroup(t, "hold") // 另一个真实存在的组，拿来当「记错的 PGID」
	in := State{
		Status:         StatusRunning,
		SupervisorPID:  deadPID(t),
		CorePID:        h.pid,
		CoreStartToken: id.Token,
		CorePGID:       other.pgid,
	}

	_, outcome, rerr := reconcileRecordedState(in, fastShutdownCtl())
	if outcome != ReconcileBlocked {
		t.Fatalf("outcome = %v, 期望 ReconcileBlocked", outcome)
	}
	var mismatch *ErrProcessGroupMismatch
	if !errors.As(rerr, &mismatch) {
		t.Fatalf("应返回 ErrProcessGroupMismatch，实际 %v", rerr)
	}
	if !pidAlive(h.pid) || !pidAlive(other.pid) {
		t.Fatal("PGID 不一致时不得对任何进程组发信号")
	}
}

// TestReconcileStoppedStateWithReusedSupervisorPIDIsStale 反向核对上一轮修的
// 假阳性：干净停止写下的 state 会保留 SupervisorPID，号码回收后可能被无关
// 进程复用。状态已经声称停止时，不能因为这个号码被占用就拦住启动。
func TestReconcileStoppedStateWithReusedSupervisorPIDIsStale(t *testing.T) {
	in := State{
		Status:        StatusStopped,
		SupervisorPID: os.Getpid(), // 代表「号码上现在有个无关进程」
	}
	_, outcome, err := ReconcileRecordedState(in)
	if err != nil {
		t.Fatalf("ReconcileRecordedState: %v", err)
	}
	if outcome != ReconcileStale {
		t.Fatalf("outcome = %v, 期望 ReconcileStale（不能让号码复用挡住启动）", outcome)
	}

	// 同一号号码，但状态声称在运行且无 token ⇒ 必须拦住（旧版语义）。
	in.Status = StatusRunning
	if _, outcome, _ := ReconcileRecordedState(in); outcome != ReconcileBlocked {
		t.Fatalf("旧版无 token 且声称在运行时应 fail-closed，实际 %v", outcome)
	}
}

// TestReconcileToleratesUnusableRecordedPGID 记录值非正（旧格式或被写坏）时，
// 以**刚刚通过身份验证的 leader 实际进程组**为准：那正是我们验证过的那个
// 进程，用它比直接放弃更安全——放弃会留下抢端口的 orphan。
func TestReconcileToleratesUnusableRecordedPGID(t *testing.T) {
	for _, recorded := range []int{0, -1} {
		t.Run(strconv.Itoa(recorded), func(t *testing.T) {
			h := spawnHelperGroup(t, "hold")
			id, err := processIdentity(h.pid)
			if err != nil {
				t.Fatal(err)
			}
			in := State{
				Status:         StatusRunning,
				SupervisorPID:  deadPID(t),
				CorePID:        h.pid,
				CoreStartToken: id.Token,
				CorePGID:       recorded,
			}
			if _, outcome, rerr := reconcileRecordedState(in, fastShutdownCtl()); outcome != ReconcileOrphanCleared {
				t.Fatalf("应清理已验证 leader 的实际进程组，实际 outcome=%v err=%v", outcome, rerr)
			}
			h.wait()
			if pidAlive(h.pid) {
				t.Fatal("orphan core 应已被清理")
			}
		})
	}
}

// TestReconcileClearsStaleLockAndKeepsLiveLock 覆盖身份感知锁：
// 陈旧锁要能清理，活锁必须拦住。
func TestReconcileClearsStaleLockAndKeepsLiveLock(t *testing.T) {
	p := testPaths(t)

	l, err := Acquire(p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := Acquire(p); err == nil {
		t.Fatal("同一进程二次 Acquire 应被拦住")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// 陈旧锁：写入一个已回收的 PID + 不匹配的 token。
	dead := deadPID(t)
	if err := os.WriteFile(lockPath(p), []byte(strconv.Itoa(dead)+"\n1:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l2, err := Acquire(p)
	if err != nil {
		t.Fatalf("陈旧锁应被清理: %v", err)
	}
	defer l2.Close()

	// 旧格式锁（无 token）+ 号码上有活进程 ⇒ fail-closed。
	h := spawnHelperGroup(t, "hold")
	if err := os.WriteFile(lockPath(p), []byte(strconv.Itoa(h.pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(p); err == nil {
		t.Fatal("旧格式锁 + 活进程必须 fail-closed")
	}
}
