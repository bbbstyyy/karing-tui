//go:build linux || darwin

package headless

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func processAlive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func stopRecordedCore(pid int) error {
	if !processAlive(pid) {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("发送 sing-box 停止信号失败: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("强制停止 sing-box 失败: %w", err)
	}
	return nil
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
