package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type failingLogDisk struct {
	err   error
	short bool
}

func (d *failingLogDisk) Write(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	if d.short && len(p) > 0 {
		return len(p) - 1, nil
	}
	return len(p), nil
}

func TestLogWriterStabilityRecovery(t *testing.T) {
	disk := &failingLogDisk{err: errors.New("disk full")}
	buf := NewLogBuf(256)
	w := NewLogWriter(disk, buf)
	for i := 0; i < 100; i++ {
		if n, err := w.Write([]byte("still running\n")); err != nil || n != 14 {
			t.Fatalf("disk failure reached the pipe reader: %d, %v", n, err)
		}
	}
	logs := strings.Join(buf.Tail(0), "\n")
	if strings.Count(logs, "disk log unavailable") != 1 {
		t.Fatal("one disk outage must produce exactly one warning")
	}
	if strings.Count(logs, "still running") != 100 {
		t.Fatal("disk failure prevented memory logging")
	}
	disk.err = nil
	_, _ = w.Write([]byte("recovered\n"))
	disk.short = true
	_, _ = w.Write([]byte("short write\n"))
	logs = strings.Join(buf.Tail(0), "\n")
	if !strings.Contains(logs, "disk logging recovered") || strings.Count(logs, "disk log unavailable") != 2 {
		t.Fatal("recovery and the next short-write outage were not reported")
	}
}

// A real child fills stdout beyond pipe capacity. The old MultiWriter stops
// draining on a disk error; the resilient sink lets the child finish normally.
func TestLogWriterStabilitySubprocess(t *testing.T) {
	for _, resilient := range []bool{false, true} {
		t.Run(fmt.Sprint(resilient), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLogWriterStabilityHelperProcess$")
			cmd.Env = append(os.Environ(), "KARING_STABILITY_LOG_HELPER=1")
			cmd.WaitDelay = 2 * time.Second
			disk := &failingLogDisk{err: errors.New("injected disk failure")}
			buf := NewLogBuf(32)
			var output io.Writer = io.MultiWriter(disk, buf)
			if resilient {
				output = NewLogWriter(disk, buf)
			}
			cmd.Stdout, cmd.Stderr = output, output
			err := cmd.Run()
			if ctx.Err() != nil {
				t.Fatal("child did not exit within its deadline")
			}
			if resilient && err != nil {
				t.Fatalf("logging failure killed the child: %v", err)
			}
			if !resilient && err == nil {
				t.Fatal("fault injection did not reproduce the old pipe failure")
			}
			if resilient && buf.Version() < 1024 {
				t.Fatal("child output was not fully drained")
			}
		})
	}
}

func TestLogWriterStabilityHelperProcess(t *testing.T) {
	if os.Getenv("KARING_STABILITY_LOG_HELPER") != "1" {
		return
	}
	line := bytes.Repeat([]byte("x"), 1023)
	line = append(line, '\n')
	for i := 0; i < 1024; i++ {
		if _, err := os.Stdout.Write(line); err != nil {
			os.Exit(23)
		}
	}
	if _, err := os.Stderr.Write([]byte("stderr drained\n")); err != nil {
		os.Exit(24)
	}
	os.Exit(0)
}

func TestManagerStabilityLoggingFailure(t *testing.T) {
	m, paths := newTestManager(t)
	script := `#!/bin/sh
case "$1" in
  version) echo "sing-box version 9.9.9"; exit 0 ;;
  check) exit 0 ;;
  run)
    trap 'exit 0' TERM
    while :; do printf 'heartbeat\n'; sleep 0.02; done ;;
esac
exit 1
`
	if err := os.WriteFile(paths.CoreBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Stop(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Start(ctx, paths.Config); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	disk := m.logFile
	m.mu.Unlock()
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	before := m.Output.Version()
	deadline := time.Now().Add(3 * time.Second)
	for m.Output.Version() < before+10 && time.Now().Before(deadline) && m.IsRunning() {
		time.Sleep(10 * time.Millisecond)
	}
	if !m.IsRunning() || m.Output.Version() < before+10 {
		t.Fatal("manager stopped draining output after persistent disk failure")
	}
	if !strings.Contains(strings.Join(m.Output.Tail(0), "\n"), "disk log unavailable") {
		t.Fatal("persistent logging failure was not visible in the memory log")
	}
}

func TestLogBufStabilityBoundedRecords(t *testing.T) {
	buf := NewLogBuf(8)
	long := strings.Repeat("\u4e2d", MaxLogLineBytes)
	for i := 0; i < 128; i++ {
		buf.AppendLine(long)
	}
	first, version, snapshot := buf.Snapshot()
	if first != 120 || version != 128 || len(snapshot) != 8 {
		t.Fatalf("unstable line identities: %d, %d, %d", first, version, len(snapshot))
	}
	for _, line := range snapshot {
		if len(line) > MaxLogLineBytes || !utf8.ValidString(line) || !strings.HasSuffix(line, " [truncated]") {
			t.Fatal("oversized log was not bounded at a UTF-8 boundary")
		}
	}
	for i := 0; i < 1000; i++ {
		buf.AppendLine("new record")
	}
	if !strings.HasSuffix(snapshot[0], " [truncated]") {
		t.Fatal("a previously returned snapshot changed after buffer rollover")
	}
	buf.AppendLine("https://user:password@example.com/secret?token=private " + long)
	if strings.Contains(buf.Tail(1)[0], "password") || strings.Contains(buf.Tail(1)[0], "private") {
		t.Fatal("truncation bypassed credential redaction")
	}
}

func TestLogBufStabilityConcurrent(t *testing.T) {
	buf := NewLogBuf(32)
	var wg sync.WaitGroup
	for writer := 0; writer < 4; writer++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				_, _ = buf.Write([]byte("record\n"))
				_, _, lines := buf.Snapshot()
				if len(lines) > 32 {
					t.Error("buffer exceeded its line bound")
					return
				}
			}
		}()
	}
	wg.Wait()
	if buf.Version() != 8000 || buf.Dropped() != 7968 {
		t.Fatal("concurrent writes lost line identities")
	}
}

func TestRotateWriterStabilityOversizedWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bounded.log")
	w, err := NewRotateWriter(path, 64, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	payload := bytes.Repeat([]byte("a"), 4*64+19)
	if n, err := w.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write: %d, %v", n, err)
	}
	for _, suffix := range []string{"", ".1", ".2"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 64 {
			t.Fatalf("%s exceeded its size limit: %d", suffix, info.Size())
		}
	}
}

func TestRotateWriterStabilityFailureRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.log")
	w, err := NewRotateWriter(path, 8, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	// A directory at the backup path makes rename fail on Linux and macOS,
	// regardless of whether the tests run with elevated permissions.
	if err := os.Mkdir(path+".1", 0o700); err != nil {
		t.Fatal(err)
	}
	if n, err := w.Write([]byte("x")); err == nil || n != 0 {
		t.Fatalf("rotation failure was hidden: %d, %v", n, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 8 {
		t.Fatal("failed rotation appended beyond the disk bound")
	}
	if err := os.Remove(path + ".1"); err != nil {
		t.Fatal(err)
	}
	if n, err := w.Write([]byte("restored")); err != nil || n != 8 {
		t.Fatalf("writer did not recover after rotation failure: %d, %v", n, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("closed")); !errors.Is(err, os.ErrClosed) {
		t.Fatal("an explicitly closed writer must not reopen")
	}
}
