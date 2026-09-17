//go:build linux || darwin

package headless

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// 关闭流程默认宽限期：与 C16（internal/core/stop.go）取同一量级，
// 保证 headless 侧与 core 侧的停止节奏一致。
const (
	defaultTermTimeout   = 5 * time.Second
	defaultKillTimeout   = 2 * time.Second
	defaultProbeInterval = 50 * time.Millisecond
)

// errProcessGroupGone 由 killGroup 返回：目标进程组已不存在（ESRCH）。
// 关闭流程中这等价于成功——目标就是要它消失。
var errProcessGroupGone = errors.New("进程组已不存在")

// ErrProcessGroupDidNotExit 表示 TERM → KILL 两段宽限后进程组仍存活。
// 文案挂在错误类型上，CLI/TUI 无需分支即可直接展示（沿用 C16 第 8 条的做法）。
type ErrProcessGroupDidNotExit struct {
	Group int // 仍未消失的进程组 PGID
}

func (e *ErrProcessGroupDidNotExit) Error() string {
	return fmt.Sprintf("残留进程组 %d 在停止超时后仍未消失，请手工确认后清理", e.Group)
}

// pidAlive 探测单个 PID 是否存在（signal 0 不实际投递信号）：
//   - nil   → 存在
//   - EPERM → 存在（无权限发信号，但进程在）
//   - ESRCH → 不存在
//   - 其它  → 按存在处理（保守：宁可拒绝自动清理，也不猜它已经走了）
//
// 它只回答「这个号码上有没有东西」，**不**回答「是不是原来那个进程」；
// 身份判定一律走 sameProcess。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	switch err := syscall.Kill(pid, 0); {
	case err == nil:
		return true
	case errors.Is(err, syscall.EPERM):
		return true
	case errors.Is(err, syscall.ESRCH):
		return false
	default:
		return true
	}
}

// processGroupAlive 探测进程组是否仍存在（signal 0）：
//   - nil / EPERM → 存在
//   - ESRCH       → 已消失
//   - 其它        → 原样上抛，由调用方决定怎么处理
//
// pgid <= 0 直接报错且绝不发信号：syscall.Kill(-0, sig) 会命中调用者自己
// 所在的进程组，等于自杀。
func processGroupAlive(pgid int) (bool, error) {
	if pgid <= 0 {
		return false, fmt.Errorf("非法进程组 ID: %d", pgid)
	}
	switch err := syscall.Kill(-pgid, 0); {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}

// killGroup 向 pgid 进程组发送信号（负 PGID）。
// ESRCH 视为「进程组已不存在」返回 errProcessGroupGone；其它错误原样上报。
// pgid <= 0 直接报错且绝不发信号（理由同 processGroupAlive）。
func killGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 0 {
		return fmt.Errorf("非法进程组 ID: %d", pgid)
	}
	switch err := syscall.Kill(-pgid, sig); {
	case err == nil:
		return nil
	case errors.Is(err, syscall.ESRCH):
		return errProcessGroupGone
	default:
		return err
	}
}

// shutdownCtl 汇集 orphan 关闭流程的可注入参数；零值字段取默认实现。
// 与 C16 的 core.stopCtl 同构，只覆盖 headless 需要的部分。
type shutdownCtl struct {
	termTimeout   time.Duration // SIGTERM 宽限期
	killTimeout   time.Duration // SIGKILL 后的确认宽限期
	probeInterval time.Duration // 探测轮询间隔
	signal        func(pgid int, sig syscall.Signal) error
	probe         func(pgid int) (bool, error)
	// warn 接收非致命问题（信号发送失败、探测不可判定）。nil 表示丢弃。
	warn func(error)
}

func (c shutdownCtl) withDefaults() shutdownCtl {
	if c.termTimeout <= 0 {
		c.termTimeout = defaultTermTimeout
	}
	if c.killTimeout <= 0 {
		c.killTimeout = defaultKillTimeout
	}
	if c.probeInterval <= 0 {
		c.probeInterval = defaultProbeInterval
	}
	if c.signal == nil {
		c.signal = killGroup
	}
	if c.probe == nil {
		c.probe = processGroupAlive
	}
	return c
}

// shutdownConfirmedGroup 对「已经通过身份验证、确认属于旧 core」的进程组
// 执行 TERM → 宽限 → KILL → 再宽限 的关闭序列。
//
// 安全模型（V5-2 第 4/5 条）把两件事分开看待：
//
//	A. 首次发任何信号之前，调用方必须已经完成 leader 身份验证
//	   （PID + start token 匹配）——由 ReconcileRecordedState 负责。
//	B. 进入本流程后追踪的是**已经确认拥有的 PGID**，不再重新要求 leader 存活。
//
// 不能把 A 的要求套到整个 B 上：leader 完全可以在 SIGTERM 后先正常退出，而
// descendant 仍留在同一个 PGID 里。此时 leader 的身份查询会失败，若因此放弃
// SIGKILL，恰好留下本流程要处理的那个顽固 descendant。
//
// 反向不变量：只要任意一次探测观察到 group 已消失，本轮就永久进入 done；
// 之后即使同一数字的 PGID 重新出现，也绝不再向它发任何信号。
func shutdownConfirmedGroup(pgid int, ctl shutdownCtl) error {
	ctl = ctl.withDefaults()
	if pgid <= 0 {
		return fmt.Errorf("非法进程组 ID: %d", pgid)
	}

	observedGone := false
	// probeGone 报告「本轮是否已确认 group 消失」。进入等待阶段后探测出错按
	// 已消失处理（与 C16 core/stop.go 的 awaitGroupGone 一致），否则收尾流程
	// 会被一个不可判定的错误永久卡住。
	probeGone := func() bool {
		if observedGone {
			return true
		}
		alive, err := ctl.probe(pgid)
		if err != nil || !alive {
			observedGone = true
			return true
		}
		return false
	}

	// 首次探测只有「明确判定已消失」才跳过信号。此刻手上还有 leader 的身份
	// 证据，漏杀一个 orphan 会留下抢端口的残留 core；而向一个已不存在的 PGID
	// 发信号最坏只是 ESRCH（已按成功处理）。
	if alive, err := ctl.probe(pgid); err == nil && !alive {
		observedGone = true
		return nil
	}

	// 信号发送失败**不中断**序列，也不直接判失败（与 C16 core/stop.go 的
	// stopProcess 同口径）：ESRCH 表示组已消失，按成功处理；其它错误只记警告，
	// 最终结论一律由「group 是否消失」给出。
	//
	// 实测（macOS）：若组内只剩一个未被回收的僵尸进程，kill(-pgid, sig) 与
	// kill(-pgid, 0) 都会返回 EPERM。把信号错误当致命错误会报出误导性的
	// 「operation not permitted」，而此刻真正该说的是「无法确认该组已消失」。
	report := func(err error) {
		if err != nil && !errors.Is(err, errProcessGroupGone) && ctl.warn != nil {
			ctl.warn(err)
		}
	}
	report(ctl.signal(pgid, syscall.SIGTERM))
	if awaitGroupGone(probeGone, ctl.termTimeout, ctl.probeInterval) {
		return nil
	}
	report(ctl.signal(pgid, syscall.SIGKILL))
	if !awaitGroupGone(probeGone, ctl.killTimeout, ctl.probeInterval) {
		return &ErrProcessGroupDidNotExit{Group: pgid}
	}
	return nil
}

// awaitGroupGone 轮询直到 probeGone 报告消失或超时，返回是否在期限内消失。
//
// 注意循环顺序：每轮先探测、再判超时，因此退出前一定会做一次**新鲜**的探测
// ——TERM 到期到 KILL 之间不能停在旧结论上（V5-2 第 5 条）。
func awaitGroupGone(probeGone func() bool, timeout, interval time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if probeGone() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}

func spawnDetached(exe string, args []string, logPath string) (*os.Process, error) {
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := detachedCommand(exe, args)
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, err
	}
	_ = f.Close()
	return cmd.Process, nil
}
