//go:build linux || darwin

package headless

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

// 测试用毫秒级超时（生产默认 5s + 2s）。
const (
	testTermTimeout   = 5 * time.Millisecond
	testKillTimeout   = 5 * time.Millisecond
	testProbeInterval = time.Millisecond
)

// groupScript 是 shutdownConfirmedGroup 的可编程进程组替身。
//
// 之所以需要替身：真实子进程无法构造「leader 收到 TERM 后先退出、descendant
// 继续留在同一个 PGID 里」这种时序，而这恰恰是本项要处理的判决性场景。
// 真实进程组的端到端验证另有 TestShutdownRealGroupAfterLeaderExits。
type groupScript struct {
	// aliveAt 决定第 n 次探测（从 1 开始）时进程组是否存在；nil 视为恒存在。
	aliveAt func(n int) bool
	// errAt 决定第 n 次探测是否返回错误。
	errAt func(n int) error
	// sigErrAt 决定第 n 次信号是否返回错误。
	sigErrAt func(n int) error

	probes  int
	signals []syscall.Signal
}

func (g *groupScript) sent(sig syscall.Signal) bool {
	for _, s := range g.signals {
		if s == sig {
			return true
		}
	}
	return false
}

func (g *groupScript) ctl() shutdownCtl {
	return shutdownCtl{
		termTimeout:   testTermTimeout,
		killTimeout:   testKillTimeout,
		probeInterval: testProbeInterval,
		signal: func(_ int, sig syscall.Signal) error {
			g.signals = append(g.signals, sig)
			if g.sigErrAt != nil {
				return g.sigErrAt(len(g.signals))
			}
			return nil
		},
		probe: func(_ int) (bool, error) {
			g.probes++
			if g.errAt != nil {
				if err := g.errAt(g.probes); err != nil {
					return false, err
				}
			}
			if g.aliveAt == nil {
				return true, nil
			}
			return g.aliveAt(g.probes), nil
		},
	}
}

// TestShutdownUpgradesToKillWhenGroupSurvivesTerm 是 V5-2 的判决性用例：
// 进程组在收到 SIGKILL 之前一直存在（等价于「leader 已经退出，descendant
// 仍留在原 PGID 里」）。实现若把「leader 必须活着」的要求套到整个关闭流程，
// 就会卡在 TERM 阶段、把 descendant 留下。
func TestShutdownUpgradesToKillWhenGroupSurvivesTerm(t *testing.T) {
	g := &groupScript{}
	g.aliveAt = func(int) bool { return !g.sent(syscall.SIGKILL) }

	if err := shutdownConfirmedGroup(4242, g.ctl()); err != nil {
		t.Fatalf("shutdownConfirmedGroup: %v", err)
	}
	if len(g.signals) != 2 || g.signals[0] != syscall.SIGTERM || g.signals[1] != syscall.SIGKILL {
		t.Fatalf("应在 TERM 超时后升级 SIGKILL，实际信号序列 %v", g.signals)
	}
}

// TestShutdownStopsSignalingOnceGroupObservedGone 钉住反向不变量：
// 一旦观察到 group 消失，本轮永久结束；即使同一数字的 PGID 之后「重新出现」
// （aliveAt 对 n>=4 返回 true），也绝不能再发任何信号。
func TestShutdownStopsSignalingOnceGroupObservedGone(t *testing.T) {
	g := &groupScript{}
	g.aliveAt = func(n int) bool { return n != 3 }

	if err := shutdownConfirmedGroup(7, g.ctl()); err != nil {
		t.Fatalf("shutdownConfirmedGroup: %v", err)
	}
	if len(g.signals) != 1 || g.signals[0] != syscall.SIGTERM {
		t.Fatalf("观察到 group gone 后不得再发信号，实际 %v", g.signals)
	}
	if g.probes != 3 {
		t.Fatalf("应在第 3 次探测（观察到消失）即结束，实际探测 %d 次", g.probes)
	}
}

// TestShutdownReportsResidualGroupWhenKillTimesOut 两段超时后 group 仍存活
// 必须报「残留进程组」，不得静默成功。
func TestShutdownReportsResidualGroupWhenKillTimesOut(t *testing.T) {
	g := &groupScript{} // aliveAt 为 nil：恒存在
	err := shutdownConfirmedGroup(9, g.ctl())

	var residual *ErrProcessGroupDidNotExit
	if !errors.As(err, &residual) {
		t.Fatalf("应返回 ErrProcessGroupDidNotExit，实际 %v", err)
	}
	if residual.Group != 9 {
		t.Fatalf("错误里应带上残留 PGID，实际 %d", residual.Group)
	}
	if len(g.signals) != 2 {
		t.Fatalf("应发 TERM + KILL，实际 %v", g.signals)
	}
}

// TestShutdownSkipsSignalsWhenGroupAlreadyGone 首次探测就明确判定消失时
// 一个信号都不发。
func TestShutdownSkipsSignalsWhenGroupAlreadyGone(t *testing.T) {
	g := &groupScript{}
	g.aliveAt = func(int) bool { return false }

	if err := shutdownConfirmedGroup(11, g.ctl()); err != nil {
		t.Fatalf("shutdownConfirmedGroup: %v", err)
	}
	if len(g.signals) != 0 {
		t.Fatalf("group 已不存在时不得发信号，实际 %v", g.signals)
	}
}

// TestShutdownRejectsInvalidPGID pid<=0 直接报错且绝不发信号——向 -0 发信号
// 会命中调用者自己所在的进程组。
func TestShutdownRejectsInvalidPGID(t *testing.T) {
	for _, pgid := range []int{0, -1, -4242} {
		g := &groupScript{}
		if err := shutdownConfirmedGroup(pgid, g.ctl()); err == nil {
			t.Fatalf("pgid=%d 应报错", pgid)
		}
		if len(g.signals) != 0 {
			t.Fatalf("pgid=%d 不得发信号，实际 %v", pgid, g.signals)
		}
	}
}

// TestShutdownTreatsProbeErrorAsGoneWhileWaiting 等待阶段的探测出错按「已消失」
// 处理（与 C16 core/stop.go 的 awaitGroupGone 同口径），不能让收尾流程卡到超时。
func TestShutdownTreatsProbeErrorAsGoneWhileWaiting(t *testing.T) {
	g := &groupScript{}
	g.aliveAt = func(int) bool { return true }
	g.errAt = func(n int) error {
		if n == 1 {
			return nil // 首次探测正常，据此发出 TERM
		}
		return errors.New("probe 不可判定")
	}
	if err := shutdownConfirmedGroup(13, g.ctl()); err != nil {
		t.Fatalf("shutdownConfirmedGroup: %v", err)
	}
	if len(g.signals) != 1 || g.signals[0] != syscall.SIGTERM {
		t.Fatalf("探测出错应按已消失结束，不再升级 KILL，实际 %v", g.signals)
	}
}

// TestShutdownReportsResidualInsteadOfSignalError 是判决性用例：TERM 与 KILL
// 都返回 EPERM、组始终存在时，报出的必须是「残留进程组」，而不是
// 「强制停止 sing-box 失败: operation not permitted」。
//
// 实测背景（macOS）：进程组内只剩一个未被回收的僵尸时，kill(-pgid, sig) 与
// kill(-pgid, 0) 都返回 EPERM。把信号错误当致命错误会在这里报出误导性文案，
// 而真正该说的是「无法确认该组已消失」。C16 core/stop.go 的 stopProcess 也
// 采用同一口径：信号失败只记警告，结论由 group 状态给出。
func TestShutdownReportsResidualInsteadOfSignalError(t *testing.T) {
	g := &groupScript{} // aliveAt 为 nil：恒存在
	g.sigErrAt = func(int) error { return errors.New("operation not permitted") }

	var warned []error
	ctl := g.ctl()
	ctl.warn = func(err error) { warned = append(warned, err) }

	err := shutdownConfirmedGroup(23, ctl)
	var residual *ErrProcessGroupDidNotExit
	if !errors.As(err, &residual) {
		t.Fatalf("应报残留进程组，实际 %v", err)
	}
	if len(warned) != 2 {
		t.Fatalf("TERM / KILL 的发送失败各应记一条警告，实际 %d 条: %v", len(warned), warned)
	}
	if !g.sent(syscall.SIGKILL) {
		t.Fatal("TERM 失败后仍必须继续尝试 KILL")
	}
}

// TestShutdownWarnsButContinuesOnSignalError 信号失败不致命：只记警告，
// 组随后消失则整体成功。
func TestShutdownWarnsButContinuesOnSignalError(t *testing.T) {
	g := &groupScript{}
	g.sigErrAt = func(n int) error {
		if n == 1 {
			return errors.New("EPERM")
		}
		return nil
	}
	g.aliveAt = func(int) bool { return !g.sent(syscall.SIGKILL) }

	var warned []error
	ctl := g.ctl()
	ctl.warn = func(err error) { warned = append(warned, err) }

	if err := shutdownConfirmedGroup(17, ctl); err != nil {
		t.Fatalf("shutdownConfirmedGroup: %v", err)
	}
	if len(warned) != 1 {
		t.Fatalf("应有 1 条警告，实际 %d 条: %v", len(warned), warned)
	}
	if !g.sent(syscall.SIGKILL) {
		t.Fatal("TERM 失败也必须在超时后升级 KILL")
	}
}

// TestKillGroupTreatsESRCHAsGone ESRCH 等价于「组已不存在」——关闭流程的目标
// 本来就是让它消失，这不算失败。
func TestKillGroupTreatsESRCHAsGone(t *testing.T) {
	if err := killGroup(1<<30, syscall.SIGTERM); !errors.Is(err, errProcessGroupGone) {
		t.Fatalf("不存在的进程组应返回 errProcessGroupGone，实际 %v", err)
	}
	if err := killGroup(0, syscall.SIGTERM); err == nil {
		t.Fatal("pgid<=0 必须报错")
	}
}

// TestProcessGroupAliveOnRealGroup 用真实 helper 进程组验证探测原语，
// 而不是只验证替身。
func TestProcessGroupAliveOnRealGroup(t *testing.T) {
	h := spawnHelperGroup(t, "hold")

	alive, err := processGroupAlive(h.pgid)
	if err != nil || !alive {
		t.Fatalf("helper 进程组应被探测为存活: alive=%v err=%v", alive, err)
	}
	if got, err := processGroupID(h.pgid); err != nil || got != h.pgid {
		t.Fatalf("helper 应是自己的组长: pgid=%d err=%v", got, err)
	}

	h.killGroup()
	h.wait()
	alive, err = processGroupAlive(h.pgid)
	if err != nil || alive {
		t.Fatalf("回收后进程组应消失: alive=%v err=%v", alive, err)
	}
}

// TestShutdownRealGroupAfterLeaderExits 是 §8 B2 的端到端判决性用例：
// 真实进程组里 leader 收到 TERM 后先退出，忽略 TERM 的 descendant 留在原
// PGID 中。必须升级 SIGKILL，最终整个组消失。
func TestShutdownRealGroupAfterLeaderExits(t *testing.T) {
	// leader 收到 TERM 就退出；descendant 由测试自己持有、加入 leader 的进程组，
	// 并忽略 TERM。这样组内始终只有「活进程」，不引入无人回收的僵尸。
	leader := spawnHelperGroup(t, "term-exit")
	desc := spawnHelperInGroup(t, "hold", leader.pgid)

	// 前置断言：两者确实在同一个进程组里。
	if got, err := processGroupID(leader.pid); err != nil || got != leader.pgid {
		t.Fatalf("leader 应在自己的进程组 %d 里: got=%d err=%v", leader.pgid, got, err)
	}
	if got, err := processGroupID(desc.pid); err != nil || got != leader.pgid {
		t.Fatalf("descendant 应加入 %d: got=%d err=%v", leader.pgid, got, err)
	}

	ctl := shutdownCtl{
		termTimeout:   200 * time.Millisecond,
		killTimeout:   5 * time.Second,
		probeInterval: 5 * time.Millisecond,
	}
	if err := shutdownConfirmedGroup(leader.pgid, ctl); err != nil {
		t.Fatalf("orphan 关闭失败: %v", err)
	}
	leader.wait()
	desc.wait()
	if pidAlive(leader.pid) {
		t.Error("leader 应已退出")
	}
	// 关键断言：只有升级到 SIGKILL 才能做到这一步。
	if pidAlive(desc.pid) {
		t.Fatalf("descendant %d 仍存活：说明 leader 退出后没有继续对同一 PGID 升级 SIGKILL", desc.pid)
	}
	if alive, err := processGroupAlive(leader.pgid); err != nil || alive {
		t.Fatalf("进程组应已消失: alive=%v err=%v", alive, err)
	}
}

// TestShutdownRealGroupWithoutDescendant 是真实进程组的对照组：leader 正常
// 响应 TERM 退出、组随即消失。信号序列的断言由替身用例负责，这里验证的是
// 「真实发信号 → 组真的没了」这条路径本身能跑通。
func TestShutdownRealGroupWithoutDescendant(t *testing.T) {
	h := spawnHelperGroup(t, "term-exit")
	ctl := shutdownCtl{
		termTimeout:   5 * time.Second,
		killTimeout:   200 * time.Millisecond,
		probeInterval: 5 * time.Millisecond,
	}
	if err := shutdownConfirmedGroup(h.pgid, ctl); err != nil {
		t.Fatalf("shutdownConfirmedGroup: %v", err)
	}
	h.wait() // 回收后才可能观察到组消失
	if pidAlive(h.pid) {
		t.Errorf("leader %d 应已退出", h.pid)
	}
	if alive, err := processGroupAlive(h.pgid); err != nil || alive {
		t.Errorf("进程组应已消失: alive=%v err=%v", alive, err)
	}
}
