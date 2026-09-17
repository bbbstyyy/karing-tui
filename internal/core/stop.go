package core

import (
	"errors"
	"time"
)

// 停止流程默认宽限期：SIGTERM 后等待进程组退出 5 秒，SIGKILL 后再确认 2 秒。
const (
	defaultTermTimeout   = 5 * time.Second
	defaultKillTimeout   = 2 * time.Second
	defaultProbeInterval = 50 * time.Millisecond
)

// stopCtl 汇集停止流程的可注入参数（C16 第 10 条：timeout / signal / probe
// 可注入，测试用毫秒级时间）。零值字段取默认实现。
type stopCtl struct {
	termTimeout   time.Duration // SIGTERM 宽限期
	killTimeout   time.Duration // SIGKILL 后的确认宽限期
	probeInterval time.Duration // watch 收尾阶段探测进程组的轮询间隔
	signal        signalFunc    // nil → realSignal
	probe         probeFunc     // nil → realProbe
}

func (c stopCtl) withDefaults() stopCtl {
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
		c.signal = realSignal
	}
	if c.probe == nil {
		c.probe = realProbe
	}
	return c
}

// ErrProcessGroupDidNotExit 表示 SIGTERM → SIGKILL 两段宽限后进程组仍存活。
// 文案挂在错误类型上，TUI 任务回执与 CLI 各调用点无需分支即可展示（C16 第 8 条）。
type ErrProcessGroupDidNotExit struct {
	Group int // 仍未消失的进程组 PGID
}

func (e *ErrProcessGroupDidNotExit) Error() string {
	return "进程组停止超时，可能存在残留子进程"
}

// stopProcess 执行「SIGTERM → 宽限 → SIGKILL → 再宽限」两段停止序列，
// 返回是否在限期内等到 exited 关闭。
//
// exited 的语义由 watch 保证：direct child 已被 Wait 回收 **且** 进程组
// 探测为已消失（C16 第 5 条：状态转 stopped 不能只看 child Wait）。
// 因此「exited 未关闭」同时涵盖 child 未退、descendant 仍持有继承管道
// 等所有需要升级到 SIGKILL 的情形。
//
// warn 非空时接收非 ESRCH 的发送失败（ESRCH 表示组已消失，按成功处理；
// 发送失败不中断序列，最终结论由 exited/超时给出）。
func stopProcess(pgid int, exited <-chan struct{}, ctl stopCtl, warn func(error)) bool {
	if err := ctl.signal(pgid, syscallSIGTERM); err != nil && !errors.Is(err, errProcessGroupGone) && warn != nil {
		warn(err)
	}
	if awaitClosed(exited, ctl.termTimeout) {
		return true
	}
	if err := ctl.signal(pgid, syscallSIGKILL); err != nil && !errors.Is(err, errProcessGroupGone) && warn != nil {
		warn(err)
	}
	return awaitClosed(exited, ctl.killTimeout)
}

// awaitClosed 等待 exited 关闭或超时。
func awaitClosed(exited <-chan struct{}, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-exited:
		return true
	case <-t.C:
		return false
	}
}

// awaitGroupGone 轮询探测直到进程组消失。watch 在 cmd.Wait 返回后调用：
// 此刻 direct child 已被回收，组若仍存活即为残留 descendant。
//
// 进程组消失是 stopped 转换的最终条件（C16 第 5 条）；若组长期不消失
// （如不可中断睡眠的进程），调用方保持 running/stuck 而不是谎报已停止。
// 已知理论限制：child 被回收后其 PID 若被新的进程组长复用，探测会误报
// 存活——正常停止路径中该窗口为微秒级，可忽略。
func awaitGroupGone(pgid int, ctl stopCtl) {
	for {
		alive, err := ctl.probe(pgid)
		if err != nil || !alive {
			// 探测出错（合法 pgid 下理论上只有 EINTR）：按消失处理，
			// 避免收尾流程被卡死——最坏退化为 C16 之前的行为。
			return
		}
		time.Sleep(ctl.probeInterval)
	}
}
