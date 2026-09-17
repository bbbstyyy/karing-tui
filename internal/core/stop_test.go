//go:build linux || darwin

package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// fakeProcess 构造一个仅用于停止流程测试的最小 cmd（不真正启动进程）。
func fakeProcess(t *testing.T, pid int) *exec.Cmd {
	t.Helper()
	return &exec.Cmd{Process: &os.Process{Pid: pid}}
}

func nopWriteCloser() io.WriteCloser { return nopCloser{} }

type nopCloser struct{}

func (nopCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopCloser) Close() error                { return nil }

// TestProcessGroupAliveWithDescendant 是清单点名的 Unix 判决性测试：
// child 派生同 PGID descendant 后先退出，processGroupAlive 必须仍报存活。
func TestProcessGroupAliveWithDescendant(t *testing.T) {
	script := fmt.Sprintf("#!/bin/sh\nsh -c 'sleep %d' &\nexit 0\n", 60)
	dir := t.TempDir()
	path := filepath.Join(dir, "spawner")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path)
	setPgid(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = killGroup(pgid, syscallSIGKILL) }) // 不残留 sleep

	if err := cmd.Wait(); err != nil {
		t.Fatalf("child 退出: %v", err)
	}

	// child 已被 Wait 回收，但同组 descendant（sleep）仍活着。
	deadline := time.Now().Add(2 * time.Second)
	for {
		alive, err := processGroupAlive(pgid)
		if err != nil {
			t.Fatalf("探测出错: %v", err)
		}
		if alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child 退出后进程组应仍存活（descendant sleep）")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// SIGKILL 整组后，进程组最终消失（ESRCH → gone）。
	if err := killGroup(pgid, syscallSIGKILL); err != nil {
		t.Fatalf("killGroup: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		alive, err := processGroupAlive(pgid)
		if err == nil && !alive {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("SIGKILL 后进程组应消失: alive=%v err=%v", alive, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProcessGroupAliveESRCH(t *testing.T) {
	// 一个极小概率被占用的 PGID 不可靠，改用已退出且无 descendant 的进程组：
	// 起一个立即退出的进程并等 Wait 回收，组内没有其他成员。
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	setPgid(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	alive, err := processGroupAlive(pgid)
	if err != nil {
		t.Fatalf("探测出错: %v", err)
	}
	if alive {
		t.Fatal("无成员的进程组应报告已消失")
	}
	if _, err := processGroupAlive(0); err == nil {
		t.Error("非法 pgid 应返回错误")
	}
}

// TestStopProcessSequencing 验证两段序列与超时判定（毫秒级注入时间）。
func TestStopProcessSequencing(t *testing.T) {
	var sigs []syscall.Signal
	never := make(chan struct{})
	ctl := stopCtl{
		termTimeout: 30 * time.Millisecond,
		killTimeout: 30 * time.Millisecond,
		signal: func(pgid int, sig syscall.Signal) error {
			sigs = append(sigs, sig)
			return nil
		},
		probe: func(pgid int) (bool, error) { return true, nil },
	}
	if stopProcess(1234, never, ctl, nil) {
		t.Fatal("exited 永不关闭时应返回 false")
	}
	if len(sigs) != 2 || sigs[0] != syscallSIGTERM || sigs[1] != syscallSIGKILL {
		t.Errorf("信号序列 = %v, 期望 [TERM KILL]", sigs)
	}

	// 正常路径：SIGTERM 后 exited 关闭，不应升级 SIGKILL。
	sigs = nil
	closed := make(chan struct{})
	close(closed)
	ctl.signal = func(pgid int, sig syscall.Signal) error {
		sigs = append(sigs, sig)
		return nil
	}
	if !stopProcess(1234, closed, ctl, nil) {
		t.Fatal("exited 已关闭时应返回 true")
	}
	if len(sigs) != 1 || sigs[0] != syscallSIGTERM {
		t.Errorf("正常路径应只发 SIGTERM，得到 %v", sigs)
	}
}

// TestStopProcessESRCHIsSuccess 验证 ESRCH 不触发 warn、不中断序列。
func TestStopProcessESRCHIsSuccess(t *testing.T) {
	var warned []error
	never := make(chan struct{})
	ctl := stopCtl{
		termTimeout: 20 * time.Millisecond,
		killTimeout: 20 * time.Millisecond,
		signal:      func(pgid int, sig syscall.Signal) error { return errProcessGroupGone },
		probe:       func(pgid int) (bool, error) { return false, nil },
	}
	if stopProcess(1, never, ctl, func(err error) { warned = append(warned, err) }) {
		t.Fatal("exited 未关闭应返回 false")
	}
	if len(warned) != 0 {
		t.Errorf("ESRCH 不应触发 warn: %v", warned)
	}
}

// TestManagerStopStuckOnTimeout 伪造运行态：两段超时后进入 stuck，
// Start 被拒绝；watch 收尾（组消失）后恢复。
func TestManagerStopStuckOnTimeout(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(paths, NewBinaryManager(paths, ""))

	var sigs []syscall.Signal
	m.stop = stopCtl{
		termTimeout: 30 * time.Millisecond,
		killTimeout: 30 * time.Millisecond,
		signal: func(pgid int, sig syscall.Signal) error {
			sigs = append(sigs, sig)
			return nil
		},
		probe: func(pgid int) (bool, error) { return true, nil }, // 组永远存活
	}

	m.mu.Lock()
	m.running = true
	m.stopping = false
	m.exited = make(chan struct{}) // 永不关闭：模拟 Wait 长期不返回
	m.cmd = fakeProcess(t, 999999)
	m.mu.Unlock()

	err = m.Stop()
	var target *ErrProcessGroupDidNotExit
	if !errors.As(err, &target) {
		t.Fatalf("Stop 应返回 ErrProcessGroupDidNotExit，得到 %v", err)
	}
	if len(sigs) != 2 {
		t.Errorf("应依次发 TERM/KILL，得到 %v", sigs)
	}

	// stuck 状态下 Start 被拒绝（即便 running 为真，错误也不能只是「已运行」）。
	if err := m.Start(context.Background(), "unused"); err == nil {
		t.Fatal("stuck 状态下 Start 应被拒绝")
	} else if want := "残留子进程"; !strings.Contains(err.Error(), want) {
		t.Errorf("Start 错误应提示残留进程，得到 %q", err.Error())
	}

	// 模拟 watch 收尾：进程组最终消失 → 恢复 stopped。
	m.mu.Lock()
	m.running = false
	m.stuck = false
	close(m.exited)
	m.mu.Unlock()

	if m.stuckState() {
		t.Error("组消失后 stuck 应被清除")
	}
	// Start 不再被 stuck 拦截（会走到 Resolve 等后续步骤，此处二进制缺失报错属正常）。
	if err := m.Start(context.Background(), "unused"); err != nil && strings.Contains(err.Error(), "残留子进程") {
		t.Errorf("恢复后 Start 不应再因 stuck 被拒: %v", err)
	}
}

// stuckState 读取 stuck 标记（测试辅助）。
func (m *Manager) stuckState() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stuck
}

// TestManagerWatchGatesExitedOnGroupGone 验证 watch 在探测确认进程组
// 消失后才关闭 exited / 清除 running（C16 第 5 条）。
func TestManagerWatchGatesExitedOnGroupGone(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(paths, NewBinaryManager(paths, ""))

	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	setPgid(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = killGroup(pgid, syscallSIGKILL) })

	var probes int
	m.stop = stopCtl{
		probeInterval: 5 * time.Millisecond,
		probe: func(p int) (bool, error) {
			if p != pgid {
				return false, nil
			}
			probes++
			return probes < 3, nil // 前两次报存活，第三次确认消失
		},
	}

	exited := make(chan struct{})
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.watch(cmd, nopWriteCloser(), cancel, exited)

	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("watch 应在组消失后完成收尾")
	}
	if probes < 3 {
		t.Errorf("exited 关闭前应探测到组消失，实际探测 %d 次", probes)
	}
	if m.IsRunning() {
		t.Error("watch 收尾后 running 应为 false")
	}
	if m.stuckState() {
		t.Error("正常收尾后 stuck 不应置位")
	}
}

// TestManagerStopNormalPath 真实进程端到端：TERM 宽限内退出，Stop 返回 nil。
func TestManagerStopNormalPath(t *testing.T) {
	m, paths := newTestManager(t)
	cfg := filepath.Join(paths.Runtime, "config.json")
	if err := os.WriteFile(cfg, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Stop() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("正常路径 Stop 不应超时")
	}
	if m.IsRunning() {
		t.Fatal("Stop 后应为停止")
	}
}

// TestAdhocStopSequence 验证 Adhoc.Stop 走同一序列且超时有界、有记录。
func TestAdhocStopSequence(t *testing.T) {
	var sigs []syscall.Signal
	a := &Adhoc{
		cmd:    fakeProcess(t, 888888),
		exited: make(chan struct{}), // 永不关闭
		Output: NewLogBuf(10),
		stop: stopCtl{
			termTimeout: 20 * time.Millisecond,
			killTimeout: 20 * time.Millisecond,
			signal: func(pgid int, sig syscall.Signal) error {
				sigs = append(sigs, sig)
				return nil
			},
			probe: func(pgid int) (bool, error) { return true, nil },
		},
	}
	done := make(chan struct{})
	go func() { a.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Adhoc.Stop 超时路径应有界返回")
	}
	if len(sigs) != 2 || sigs[0] != syscallSIGTERM || sigs[1] != syscallSIGKILL {
		t.Errorf("信号序列 = %v, 期望 [TERM KILL]", sigs)
	}
	if got := a.Output.Tail(1); len(got) == 0 || !strings.Contains(got[len(got)-1], "残留子进程") {
		t.Errorf("超时应写入 Output，得到 %v", got)
	}
}
