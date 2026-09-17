//go:build linux || darwin

package headless

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// 测试 helper 子进程的开关。用「独立可执行进程 + 环境变量开关 + setpgid」
// 构造真实进程组：不得对测试进程自身或无关进程发信号（V5-2 测试矩阵要求）。
const helperEnv = "KARING_HEADLESS_TEST_HELPER"

// TestHeadlessHelperProcess 不是测试，而是被其它测试 exec 出来的 helper 入口。
// 父进程用 `-test.run=^TestHeadlessHelperProcess$` 单独运行它。
func TestHeadlessHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return // 正常测试轮次：什么都不做
	}
	switch mode {
	case "term-exit":
		// 正常响应 SIGTERM 的普通进程（不派生 descendant）。
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		<-sig
		os.Exit(0)
	case "hold":
		// 忽略 SIGTERM，只能被 SIGKILL 带走——用来扮演「顽固 descendant」。
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(60 * time.Second)
	default:
		os.Exit(4)
	}
	os.Exit(0)
}

// helperProc 是一个独立进程组的 helper 子进程。
//
// 父进程在后台立刻回收它（不留下僵尸）。这不是洁癖：实测 macOS 上若组内
// 只剩一个未被回收的僵尸，kill(-pgid, sig) 与 kill(-pgid, 0) 都会返回
// EPERM，测试会观察到与生产完全不同的探测结果（生产路径上 orphan 的中间
// 父进程都已死亡，僵尸由 init 回收，不会停在这种状态）。
type helperProc struct {
	cmd  *exec.Cmd
	pid  int
	pgid int
	done chan struct{}
}

func (h *helperProc) killGroup() { _ = syscall.Kill(-h.pgid, syscall.SIGKILL) }

func (h *helperProc) wait() { <-h.done }

// spawnHelperGroup 启动一个独立进程组的 helper 并立即开始后台回收。
func spawnHelperGroup(t *testing.T, mode string) *helperProc {
	return spawnHelper(t, mode, 0)
}

// spawnHelperInGroup 启动一个**加入既有进程组**的 helper（Setpgid + Pgid）。
//
// 它必须由测试进程直接持有并回收：这样「组内只剩僵尸」的情形就不会出现。
// 实测教训——最初把 descendant 交给 leader 派生，leader 退出后 descendant
// 被 reparent 给 PID 1；在 Linux 容器里 PID 1 就是 `go test` 自己，它不回收
// 陌生子进程，于是 descendant 被 SIGKILL 后留下一个无人回收的僵尸。僵尸仍是
// 进程组成员，kill(-pgid, 0) 继续返回成功，探测永远报「存活」，判决性用例
// 在 Linux 上必然超时失败（macOS 由 launchd 回收，所以那时看不出问题）。
func spawnHelperInGroup(t *testing.T, mode string, pgid int) *helperProc {
	t.Helper()
	return spawnHelper(t, mode, pgid)
}

func spawnHelper(t *testing.T, mode string, pgid int) *helperProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHeadlessHelperProcess$", "--")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode)
	if pgid > 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	// helper 自己会打印 test 框架的输出，丢掉以免污染本测试的输出。
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 helper(%s): %v", mode, err)
	}
	ownPgid := cmd.Process.Pid
	if pgid > 0 {
		ownPgid = pgid
	}
	h := &helperProc{cmd: cmd, pid: cmd.Process.Pid, pgid: ownPgid, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(h.done)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-h.pgid, syscall.SIGKILL)
		<-h.done
	})
	return h
}

// deadPID 返回一个「曾经真实存在、现在已被回收」的 PID。
func deadPID(t *testing.T) int {
	t.Helper()
	h := spawnHelperGroup(t, "hold")
	h.killGroup()
	h.wait()
	if pidAlive(h.pid) {
		t.Fatalf("helper %d 应在回收后不可见", h.pid)
	}
	return h.pid
}
