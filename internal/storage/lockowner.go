//go:build linux || darwin

package storage

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/procidentity"
)

// 进程锁的所有者身份与判定矩阵（V7-5）。
//
// 背景：instance.lock / migration.lock 原来只写一个裸 PID，判活靠 `kill -0`。
// PID 会被复用——上次异常退出留下的号码一旦被无关进程（编辑器、系统守护进程）
// 接手，用户就再也启动不了，只能手工去 runtime/ 删锁；迁移锁更糟，它会先等满
// 30 秒再失败。V5-2 已给 headless 状态文件上了「PID + start token」，本轮把
// 两处锁补齐（v5 清单里登记的后续项）。
//
// 锁文件固定两行：
//
//	<PID>
//	<TOKEN>
//
// TOKEN 的语义见 procidentity.Identity。旧版写出的**单行**格式继续可读，
// 此时 token 为空、按 fail-closed 处理（见 classifyLock）。
type lockOwner struct {
	pid   int
	token string
}

// readLockOwner 读取锁文件；第二个返回值为 false 表示内容解析不出合法 PID。
func readLockOwner(path string) (lockOwner, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return lockOwner{}, false
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || pid <= 0 {
		return lockOwner{}, false
	}
	o := lockOwner{pid: pid}
	if len(lines) > 1 {
		o.token = strings.TrimSpace(lines[1])
	}
	return o, true
}

// writeLockOwner 写入 PID + TOKEN。token 为空时写回单行格式——见 selfIdentity。
func writeLockOwner(f *os.File, id procidentity.Identity) error {
	if id.Token == "" {
		_, err := fmt.Fprintf(f, "%d\n", id.PID)
		return err
	}
	_, err := fmt.Fprintf(f, "%d\n%s\n", id.PID, id.Token)
	return err
}

// selfIdentity 返回当前进程身份；读不出来时退化为「只有 PID」。
//
// 刻意**不**在身份不可得时拒绝启动：那会把 /proc 受限的机器（部分容器）整个
// 锁死，而这类机器上的用户连自救命令都跑不起来。退化成单行格式是安全的——
// 后来者看到「单行 + PID 活着」会 fail-closed，不会误清锁，数据库层面仍不会
// 出现双实例并发写。代价只是这份锁需要人工清理。
func selfIdentity() procidentity.Identity {
	id, err := procidentity.Current()
	if err != nil {
		return procidentity.Identity{PID: os.Getpid()}
	}
	return id
}

// lockVerdict 是锁判定结论。
type lockVerdict int

const (
	lockHeld    lockVerdict = iota // 原持有者仍在 → 拒绝第二实例 / 继续等待
	lockStale                      // 持有者已确认消失，或 PID 被复用 → 可清理
	lockUnknown                    // 无法证明身份 → fail-closed，绝不清锁
)

// classifyLock 实现判定矩阵（每一行都有对应测试）：
//
//	token 匹配 + 进程存在     → lockHeld
//	token 不匹配 + 进程存在   → lockStale（PID 被无关进程复用）
//	进程不存在（ErrProcessGone）→ lockStale
//	身份读取其它错误          → lockUnknown
//	单行格式 + PID 活着       → lockUnknown（无法证明身份）
//	单行格式 + PID 已死       → lockStale
//	内容解析不出 PID          → lockUnknown
//
// 「单行格式 + 活 PID」必须 fail-closed：单凭一个号码不能证明对方是 karing-tui，
// 贸然删锁会让第二个实例与活着的第一个同时写同一个数据库。
// 「PID 已死」则是正面证据，不需要 token。
// 「身份读不出来」也 fail-closed：探测出错不等于对方已走。
func classifyLock(owner lockOwner, parseable bool) lockVerdict {
	if !parseable {
		return lockUnknown
	}
	cur, err := procidentity.Lookup(owner.pid)
	switch {
	case err == nil:
		if owner.token == "" {
			return lockUnknown
		}
		if cur.Token == owner.token {
			return lockHeld
		}
		return lockStale
	case errors.Is(err, procidentity.ErrProcessGone):
		return lockStale
	default:
		return lockUnknown
	}
}
