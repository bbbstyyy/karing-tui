//go:build linux || darwin

package headless

import "github.com/bbbstyyy/karing-tui/internal/procidentity"

// 进程身份的实现在 internal/procidentity：那是一个零业务依赖的叶子包，
// internal/storage 的进程锁用的是同一套判定（V7-5）。本文件只做薄包装，
// 保持 headless 既有调用点与测试的签名不变；平台专属的 /proc 解析与
// kinfo_proc 读取都已迁走，本包不再自己读平台信息。

// ProcessIdentity 是「某个 PID 在某一时刻确实是那个进程」的可验证证据。
//
// Token 是平台私有的**不透明**标识，只用于相等比较，不要解析它：
//
//	Linux   <boot_id>:<starttime>
//	Darwin  <sec>:<usec>
//
// 两个平台都要能挡住「PID 被无关进程复用」。Linux 侧还必须额外带 boot_id：
// starttime 是「自本次 boot 起的时钟节拍」，机器重启后会从头计算，于是
// 「重启 + PID 复用 + starttime 恰好撞上」会让无关进程被误认成旧 core。
// 详见 procidentity.Identity 的注释。
type ProcessIdentity = procidentity.Identity

// ErrProcessGone 表示目标 PID 已不存在——此时连身份都谈不上。
var ErrProcessGone = procidentity.ErrProcessGone

// processIdentity 读取 pid 的身份。见 procidentity.Lookup。
func processIdentity(pid int) (ProcessIdentity, error) {
	return procidentity.Lookup(pid)
}

// processGroupID 读取 pid 所在进程组的 PGID。见 procidentity.GroupID。
func processGroupID(pid int) (int, error) {
	return procidentity.GroupID(pid)
}

// sameProcess 判断 pid 上此刻的进程是否就是 token 所描述的那个。
//
// token 为空表示「不可验证」（v0.4.0 写出的 headless state / 锁文件就是这种），
// 一律返回 false。**禁止**把它写成
// `if token == "" { return pidAlive(pid) }`：那等于让兼容分支把整轮安全边界
// 绕过去，正是 V5-2 要修的东西。
func sameProcess(pid int, token string) bool {
	return procidentity.Matches(procidentity.Identity{PID: pid, Token: token})
}
