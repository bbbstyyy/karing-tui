//go:build linux || darwin

// Package procidentity 提供「某个 PID 在某一时刻确实是那个进程」的可验证证据。
//
// 它是**零业务依赖的叶子包**：内部只有平台事实（Linux 的 /proc、Darwin 的
// kern.proc.pid），不 import 本仓库其它任何包。这样 internal/storage（进程锁）与
// internal/headless（core 生命周期）可以共用同一套判定，而不必让 storage 反向依赖
// headless——storage 是被各层 import 的底层包，把上层子树拖进它的依赖面会破坏分层。
//
// 关键约定：**token 为空 = 不可验证 = fail-closed**（见 Matches）。
// 这是 V5-2 与 V7-5 共同的安全边界，不要为了「兼容旧格式」而放宽。
package procidentity

import (
	"errors"
	"os"
)

// Identity 是「某个 PID 在某一时刻确实是那个进程」的可验证证据。
//
// Token 是平台私有的**不透明**标识，只用于相等比较，不要解析它：
//
//	Linux   <boot_id>:<starttime>   /proc/sys/kernel/random/boot_id
//	                                + /proc/<pid>/stat field 22
//	Darwin  <sec>:<usec>            kern.proc.pid → p_starttime
//
// 两个平台都要能挡住「PID 被无关进程复用」。Linux 侧还必须额外带 boot_id：
// starttime 是「自本次 boot 起的时钟节拍」，机器重启后会从头计算，于是
// 「重启 + PID 复用 + starttime 恰好撞上」会让无关进程被误认成旧持有者。
// Darwin 的 p_starttime 是绝对墙钟时间（实测为 Unix 秒级数值），本身不随重启重置，
// 因此不需要 boot id。
type Identity struct {
	PID   int
	Token string
}

// ErrProcessGone 表示目标 PID 已不存在——此时连身份都谈不上。
var ErrProcessGone = errors.New("进程已不存在")

// Current 返回当前进程的身份。
func Current() (Identity, error) {
	return Lookup(os.Getpid())
}

// Matches 判断 pid 上此刻的进程是否就是 id 所描述的那个。
//
// token 为空表示「不可验证」（旧版本写出的锁文件 / state 就是这种），一律返回
// false。**禁止**写成 `if id.Token == "" { return pidAlive(id.PID) }`：那等于让
// 兼容分支把整轮安全边界绕过去，正是 V5-2 / V7-5 要修的东西。
//
// 身份读取出错（含 ErrProcessGone）同样返回 false——本函数只回答「能证明是同一个吗」。
// 需要区分「进程已消失」与「读不出来」的调用方必须自己用 Lookup，按 ErrProcessGone
// 分流（见 storage 的锁判定矩阵）。
func Matches(id Identity) bool {
	if id.PID <= 0 || id.Token == "" {
		return false
	}
	cur, err := Lookup(id.PID)
	if err != nil {
		return false
	}
	return cur.Token == id.Token
}
