package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotateWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	w, err := NewRotateWriter(path, 100, 3)
	if err != nil {
		t.Fatalf("NewRotateWriter: %v", err)
	}

	// 写满第一份（>100B 触发轮转）
	line := strings.Repeat("a", 40) + "\n"
	for i := 0; i < 3; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// 第 3 次写入会超限：先轮转再写入新文件
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("轮转文件 .1 不存在: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() > 100 {
		t.Errorf("轮转后主文件应不超过上限: size=%v, err=%v", info, err)
	}

	// 连续多轮转：.2、.3 出现，.3 为最旧
	for i := 0; i < 10; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	for _, suffix := range []string{".1", ".2", ".3"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Errorf("轮转文件 %s 不存在: %v", suffix, err)
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Errorf("超出保留份数的 .4 不应存在")
	}

	// 关闭后再写应报错而非 panic
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("关闭后写入应返回错误")
	}
}

func TestRotateWriterAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	// 已有内容时打开应追加（不轮转不截断）
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := NewRotateWriter(path, 1<<20, 3)
	if err != nil {
		t.Fatalf("NewRotateWriter: %v", err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("new\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "old\nnew\n" {
		t.Errorf("追加写入不符: %q", data)
	}
}

func TestRotateWriterConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	w, err := NewRotateWriter(filepath.Join(dir, "concurrent.log"), 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	const writers = 8
	const writes = 100
	done := make(chan struct{}, writers)
	for i := 0; i < writers; i++ {
		go func() {
			for j := 0; j < writes; j++ {
				if _, err := w.Write([]byte("line\n")); err != nil {
					t.Errorf("Write: %v", err)
				}
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < writers; i++ {
		<-done
	}
}
