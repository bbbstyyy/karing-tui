//go:build linux || darwin

package headless

import (
	"errors"
)

// ProcessIdentity 是「某个 PID 在某一时刻确实是那个进程」的可验证证据。
//
// Token 是平台私有的**不透明**标识，只用于相等比较，不要解析它：
//
//	Linux   <boot_id>:<starttime>   /proc/sys/kernel/random/boot_id
//	                                + /proc/<pid>/stat field 22
//	Darwin  <sec>:<usec>            kern.proc.pid → p_starttime
//
// 两个平台都要能挡住「PID 被无关进程复用」。Linux 侧还必须额外带 boot_id：
// starttime 是「自本次 boot 起的时钟节拍」，机器重启后会从头计算，
// 于是「重启 + PID 复用 + starttime 恰好撞上」会让无关进程被误认成旧 core。
// Darwin 的 p_starttime 是绝对墙钟时间（实测本机为 Unix 秒级数值），
// 本身就不随重启重置，因此不需要 boot id。
type ProcessIdentity struct {
	PID   int
	Token string
}

// ErrProcessGone 表示目标 PID 已不存在——此时连身份都谈不上。
var ErrProcessGone = errors.New("进程已不存在")

// sameProcess 判断 pid 上此刻的进程是否就是 token 所描述的那个。
//
// token 为空表示「不可验证」（v0.4.0 写出的 headless state / 锁文件就是
// 这种），一律返回 false。**禁止**把它写成
// `if token == "" { return pidAlive(pid) }`：那等于让兼容分支把整轮安全边界
// 绕过去，正是 V5-2 要修的东西。
func sameProcess(pid int, token string) bool {
	if pid <= 0 || token == "" {
		return false
	}
	id, err := processIdentity(pid)
	if err != nil {
		return false
	}
	return id.Token == token
}
