// Package headless 提供无 TUI 场景下的 sing-box supervisor。
package headless

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

const (
	stateFile = "headless.json"
	lockFile  = "headless.lock"
	stopFile  = "headless.stop"
)

const (
	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusStopped  = "stopped"
	StatusCrashed  = "crashed"
)

// State 是 supervisor 的跨进程状态快照。字段保持稳定，供脚本读取。
//
// 三个 *StartToken / CorePGID 字段是 V5-2 新增的可验证身份：PID 会被复用，
// 因此「记录在案的 PID」只有配上「当时那个进程的启动身份」才能回答
// 「它还是不是原来那个进程」。字段名与含义见 ProcessIdentity。
type State struct {
	Status        string    `json:"status"`
	SupervisorPID int       `json:"supervisor_pid,omitempty"`
	CorePID       int       `json:"core_pid,omitempty"`
	StartedAt     time.Time `json:"started_at,omitempty"`
	StoppedAt     time.Time `json:"stopped_at,omitempty"`
	ExitError     string    `json:"exit_error,omitempty"`
	Version       string    `json:"core_version,omitempty"`
	MixedPort     int       `json:"mixed_port,omitempty"`
	Config        string    `json:"config,omitempty"`

	// SupervisorStartToken / CoreStartToken 为空的含义是「不可验证」，
	// 不能退回裸 PID 判定（详见 sameProcess）。
	SupervisorStartToken string `json:"supervisor_start_token,omitempty"`
	CoreStartToken       string `json:"core_start_token,omitempty"`
	// CorePGID 在 core 成功启动后立即记录，不等 leader 退出后再推导：
	// leader 退出后 PID 已无从查询，而 descendant 可能仍留在该进程组里。
	CorePGID int `json:"core_pgid,omitempty"`
}

var stateMu sync.Mutex

func statePath(paths *platform.Paths) string { return filepath.Join(paths.Runtime, stateFile) }
func lockPath(paths *platform.Paths) string  { return filepath.Join(paths.Runtime, lockFile) }
func stopPath(paths *platform.Paths) string  { return filepath.Join(paths.Runtime, stopFile) }

// ReadState 读取最近一次 supervisor 状态；没有状态文件时返回 stopped。
func ReadState(paths *platform.Paths) (State, error) {
	b, err := os.ReadFile(statePath(paths))
	if errors.Is(err, os.ErrNotExist) {
		return State{Status: StatusStopped}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("解析 headless 状态失败: %w", err)
	}
	if st.Status == "" {
		st.Status = StatusStopped
	}
	return st, nil
}

// WriteState 原子写入状态文件，避免 status 读到半个 JSON。
func WriteState(paths *platform.Paths, st State) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	if err := os.MkdirAll(paths.Runtime, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(paths.Runtime, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(paths.Runtime, ".headless-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, statePath(paths))
}

// RequestStop 请求 supervisor 优雅退出。使用文件协议在 Linux/macOS 上工作。
func RequestStop(paths *platform.Paths) error {
	if err := os.WriteFile(stopPath(paths), []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0o600); err != nil {
		return fmt.Errorf("写入停止请求失败: %w", err)
	}
	return nil
}

func consumeStop(paths *platform.Paths) bool {
	if _, err := os.Stat(stopPath(paths)); err != nil {
		return false
	}
	_ = os.Remove(stopPath(paths))
	return true
}

// ClearStop 清除上一次遗留的停止请求。
func ClearStop(paths *platform.Paths) { _ = os.Remove(stopPath(paths)) }

// Active 判断记录在案的 supervisor 是否仍在运行。
//
// V5-2 起判据是「PID + start token 匹配」，不再是裸 PID 存活：
//   - v0.4.0 写出的旧 state 没有 token → 不可验证 → false；
//   - PID 被无关进程复用 → token 不匹配 → false。
//
// 返回 false 只代表「不能证明它在运行」，**不代表可以安全启动第二份**。
// 决定能不能继续的是 ReconcileRecordedState。
func Active(st State) bool {
	if st.Status != StatusRunning && st.Status != StatusStarting {
		return false
	}
	return sameProcess(st.SupervisorPID, st.SupervisorStartToken)
}

// IdentityUnverifiable 报告 state 是否为「不可验证」的旧版布局：supervisor
// 尚未停止，却没写下 start token（v0.4.0 就是这种）。
//
// 仅用于 status 展示，把「进程真的没了」与「进程在、但无法证明是它」区分开
// ——否则升级后 `karing status` 会对一个还活着的旧 supervisor 报「PID 已退出」，
// 把人引向错误的排查方向。
func IdentityUnverifiable(st State) bool {
	return st.Status != StatusStopped && st.SupervisorStartToken == ""
}

// ReconcileOutcome 描述 reconcile 之后调用方该怎么走。
type ReconcileOutcome int

const (
	// ReconcileRunning：supervisor 身份可验证且匹配，服务确实在运行。
	// 调用方走正常的 stop/restart 协议（文件请求），不要发信号。
	ReconcileRunning ReconcileOutcome = iota
	// ReconcileStale：记录在案的进程都不存在了，state 可直接写回 stopped
	// 并继续启动。
	ReconcileStale
	// ReconcileOrphanCleared：supervisor 已死，但记录在案的 core 进程组经
	// 身份验证确认属于旧 core，已完成 TERM → KILL 关闭。
	ReconcileOrphanCleared
	// ReconcileBlocked：存在无法安全处理的残留——身份不可验证、身份不匹配，
	// 或 orphan 关闭未能在期限内完成。调用方必须报错停止，**绝不**启动第二份
	// core（否则会与残留 core 抢端口并把状态写成 crashed）。
	ReconcileBlocked
)

// ErrStateUnverifiable 表示 headless.json 里记录着仍然存在的进程，但无法证明
// 它们就是本应用启动的 core / supervisor。
//
// 两个典型来源：PID 被无关进程复用；以及 v0.4.0 写出的、不含 start token 的
// 旧 state。处理口径是 fail-closed——不发任何终止信号（误杀无关进程组的代价
// 不可接受），也不启动第二份 core（会与残留 core 抢端口）。
type ErrStateUnverifiable struct {
	SupervisorPID int
	CorePID       int
	CorePGID      int
}

func (e *ErrStateUnverifiable) Error() string {
	target := "相关进程"
	if e.CorePGID > 0 {
		target = fmt.Sprintf("进程组 %d", e.CorePGID)
	}
	return fmt.Sprintf(
		"headless 运行状态无法验证（记录 supervisor PID %d、core PID %d、core PGID %d 仍存在，但缺少可验证的进程身份）: "+
			"为避免误杀无关进程，已放弃自动清理，也不会启动第二份 core；请确认 %s 确实是残留的 sing-box 后手工停止（kill -TERM -%d）再重试",
		e.SupervisorPID, e.CorePID, e.CorePGID, target, e.CorePGID)
}

// ErrProcessGroupMismatch 表示 state 记录的 CorePGID 与当前 leader 实际所属的
// 进程组不一致。正常不会发生（PGID 是 core 启动后立刻读出来写下的），出现即
// 说明状态被手工改过或写坏了：此时宁可不清理，也不能按一个来路不明的数字
// 去向进程组发信号。
type ErrProcessGroupMismatch struct {
	PID          int
	RecordedPGID int
	ActualPGID   int
}

func (e *ErrProcessGroupMismatch) Error() string {
	return fmt.Sprintf(
		"headless 状态自相矛盾：core PID %d 实际属于进程组 %d，而记录的 CorePGID 是 %d: "+
			"为避免误杀无关进程组，已放弃自动清理；请确认后手工处理 headless.json",
		e.PID, e.ActualPGID, e.RecordedPGID)
}

// ReconcileRecordedState 把落盘 state 与实际进程对齐，并在能取得身份证据时
// 清理孤儿 core。start / restart / stop **都必须**先过这一关。
//
// 决策树：
//
//	supervisor 身份可验证且匹配 ──→ ReconcileRunning
//	core leader 身份可验证且匹配 ──→ 取得 PGID 所有权证据 → orphan shutdown
//	记录在案的进程都已不存在     ──→ ReconcileStale
//	其余（无法验证 / 不匹配）    ──→ ReconcileBlocked
//
// 返回的 State 是「对齐后」的快照，落盘时机由调用方决定：只有
// ReconcileOrphanCleared / ReconcileStale 才值得写回。
func ReconcileRecordedState(st State) (State, ReconcileOutcome, error) {
	return reconcileRecordedState(st, shutdownCtl{})
}

// reconcileRecordedState 是 ReconcileRecordedState 的可注入版本：唯一区别是
// orphan 关闭的超时/信号/探测可替换。测试要能在毫秒级跑完真实进程组的关闭，
// 又不愿意引入 package global 可变状态（那会给 -race 添共享可变状态）。
func reconcileRecordedState(st State, ctl shutdownCtl) (State, ReconcileOutcome, error) {
	if Active(st) {
		return st, ReconcileRunning, nil
	}

	// core leader 的身份是「这个 PGID 属于旧 core」的唯一证据来源。
	if sameProcess(st.CorePID, st.CoreStartToken) {
		pgid, pgidErr := resolveOwnedPGID(st)
		if pgidErr != nil {
			return st, ReconcileBlocked, pgidErr
		}
		if err := shutdownConfirmedGroup(pgid, ctl); err != nil {
			return st, ReconcileBlocked, err
		}
		return clearedState(st), ReconcileOrphanCleared, nil
	}

	// 身份不可验证或已不匹配：只有在确认「记录在案的进程都不存在」时才允许
	// 继续；否则一律 fail-closed。旧版无 token 的 state 会落在这里。
	if recordedLeftovers(st) {
		return st, ReconcileBlocked, &ErrStateUnverifiable{
			SupervisorPID: st.SupervisorPID,
			CorePID:       st.CorePID,
			CorePGID:      st.CorePGID,
		}
	}
	return clearedState(st), ReconcileStale, nil
}

// resolveOwnedPGID 在「leader 身份已验证」的前提下取得该 PGID 的所有权证据。
//
//   - 记录的 CorePGID > 0 时与 leader 实际所属进程组交叉核对，不一致即拒绝：
//     此时宁可不清理，也不能按一个来路不明的数字去发信号——发错就是误杀。
//   - 记录值缺失或非正（旧格式、被写坏）时以 leader 的实际进程组为准：那正是
//     我们刚刚验证过的那个进程，用它比直接放弃更安全（放弃会留下抢端口的
//     orphan）。
//   - leader 在身份验证之后、发信号之前退出是合法的，此时退回记录值——它是
//     启动瞬间由我们自己写下、且 leader 刚刚还通过过身份验证。
func resolveOwnedPGID(st State) (int, error) {
	actual, err := processGroupID(st.CorePID)
	switch {
	case err == nil && actual > 0:
		if st.CorePGID > 0 && st.CorePGID != actual {
			return 0, &ErrProcessGroupMismatch{
				PID: st.CorePID, RecordedPGID: st.CorePGID, ActualPGID: actual,
			}
		}
		return actual, nil
	case st.CorePGID > 0:
		return st.CorePGID, nil
	default:
		return 0, fmt.Errorf("无法确定 core PID %d 的进程组，且状态里没有可用的 CorePGID: %w", st.CorePID, err)
	}
}

// recordedLeftovers 判断 state 里记录的进程是否还有残留。
//
// 这一步刻意**不做身份判定**：它只在身份验证已经失败的分支上使用，回答的是
// 「记录在案的号码上还有没有东西」。有 → 拒绝继续；没有 → 才允许当 stale
// 处理。宁可拒绝启动，也不能猜一个无法验证的 PID 是不是自己的。
func recordedLeftovers(st State) bool {
	// core 的残留与状态无关：core 的号码一旦还有东西，就不能视而不见。
	if pidAlive(st.CorePID) {
		return true
	}
	if st.CorePGID > 0 {
		// 探测出错时按「可能还在」处理：这是拒绝启动的分支，保守方向一致。
		if alive, err := processGroupAlive(st.CorePGID); err != nil || alive {
			return true
		}
	}
	// supervisor 的残留**只在身份不可验证时**才作为拒绝依据：
	//   - token 存在但不匹配 ⇒ 号码上是另一个进程，原 supervisor 已确定退出，
	//     此时再拦就是纯假阳性；
	//   - 干净停止写下的 state 会保留 SupervisorPID（号码早已回收、可能已被
	//     无关进程复用），若不做「状态必须声称自己在运行」这一层限制，
	//     每一次 PID 复用都会把用户挡在 `headless start` 外面。
	// 而 v0.4.0 旧 state 恰好是「无 token + PID 仍在」，正是清单要求
	// fail-closed 的那一种，这里必须拦住。
	if st.SupervisorStartToken == "" && pidAlive(st.SupervisorPID) &&
		(st.Status == StatusRunning || st.Status == StatusStarting) {
		return true
	}
	return false
}

// clearedState 返回「已确认无残留」的对齐快照：清空进程身份并转 stopped。
// ExitError 特意保留，供 `karing headless status` 继续展示上一次的失败原因。
func clearedState(st State) State {
	st.Status = StatusStopped
	st.SupervisorPID, st.CorePID, st.CorePGID = 0, 0, 0
	st.SupervisorStartToken, st.CoreStartToken = "", ""
	st.StoppedAt = time.Now()
	return st
}

// markCoreGone 清掉 core 的进程身份标记。仅在 core 进程组已消失时调用。
func markCoreGone(st *State) {
	st.CorePID, st.CoreStartToken, st.CorePGID = 0, "", 0
}

// markStopped 把 state 归位为「已停止」并清掉 core 的进程身份。
//
// 调用点（收到信号、收到停止文件、发现 core 自行退出）都发生在 core 进程组
// **已经消失**之后——core.Manager.watch 要等 awaitGroupGone 返回才会把
// running 置 false（C16 第 5 条）。因此清空是准确的，而且必须清：留着一个
// 已回收的 PGID，下次 reconcile 就可能撞上被无关进程组复用的同号 PGID，
// 把本该顺利启动的流程误判成需要人工清理。
func markStopped(st *State) {
	markCoreGone(st)
	st.Status, st.StoppedAt = StatusStopped, time.Now()
}

// Lock 表示当前 headless supervisor 对数据目录的独占权。
type Lock struct {
	paths *platform.Paths
	path  string
	pid   int
	owned bool
}

// lockFileSeparator 之后是持有者的 start token（第二行）。
const lockFileSeparator = "\n"

// Acquire 获取 supervisor 锁。
//
// 锁文件写入「PID + start token」，因此「旧锁能否清理」同样是身份判定，而不是
// 「号码上还有没有进程」：
//
//	token 匹配          → 确实还在运行，拒绝获取
//	进程已不存在        → 陈旧锁，清理后重试
//	token 不匹配        → 号码被复用，原持有者已死，清理后重试
//	token 缺失（v0.4.0）→ 不可验证：进程不在则清理，还在则 fail-closed
//
// 旧格式（只有一行 PID）必须继续可读，否则升级后残留的旧锁会让 headless
// 永久起不来。
func Acquire(paths *platform.Paths) (*Lock, error) {
	self, err := processIdentity(os.Getpid())
	if err != nil {
		return nil, fmt.Errorf("无法确定本进程身份，拒绝创建 headless 锁: %w", err)
	}
	l := &Lock{paths: paths, path: lockPath(paths), pid: os.Getpid()}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d%s%s%s", l.pid, lockFileSeparator, self.Token, lockFileSeparator)
			_ = f.Close()
			l.owned = true
			return l, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("创建 headless 锁失败: %w", err)
		}
		b, readErr := os.ReadFile(l.path)
		if readErr != nil {
			return nil, fmt.Errorf("读取 headless 锁失败: %w", readErr)
		}
		pid, token := parseLockFile(string(b))
		if pid <= 0 {
			return nil, fmt.Errorf("headless 锁文件内容无法解析: %s", l.path)
		}
		cur, idErr := processIdentity(pid)
		switch {
		case idErr != nil && !errors.Is(idErr, ErrProcessGone):
			// 探测本身失败：不能判定持有者已退出，按「可能还在」处理。
			return nil, fmt.Errorf("无法验证 headless 锁持有者（PID %d）: %w", pid, idErr)
		case idErr == nil && token == "":
			return nil, fmt.Errorf("headless 锁由旧版本写入且无法验证持有者身份（PID %d 仍存在）: 请确认后手工删除 %s", pid, l.path)
		case idErr == nil && cur.Token == token:
			return nil, fmt.Errorf("headless 已在运行（supervisor PID %d）", pid)
		}
		// 到这里只剩两种情况：进程确实已不存在，或该号码已被别的进程占用
		// （token 不匹配即「原持有者已死」的正面证据）——都是陈旧锁。
		if err := os.Remove(l.path); err != nil {
			return nil, fmt.Errorf("清理旧 headless 锁失败: %w", err)
		}
	}
	return nil, fmt.Errorf("获取 headless 锁失败")
}

// parseLockFile 解析锁文件：第一行 PID，第二行（旧格式没有）start token。
func parseLockFile(s string) (int, string) {
	lines := strings.SplitN(s, lockFileSeparator, 2)
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || pid <= 0 {
		return 0, ""
	}
	token := ""
	if len(lines) > 1 {
		token = strings.TrimSpace(lines[1])
	}
	return pid, token
}

func (l *Lock) Close() error {
	if l == nil || !l.owned {
		return nil
	}
	l.owned = false
	return os.Remove(l.path)
}

// ConsumeStopForTest 保留给同包测试，避免暴露文件协议细节。
func ConsumeStopForTest(paths *platform.Paths) bool { return consumeStop(paths) }
