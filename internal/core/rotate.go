package core

import (
	"fmt"
	"os"
	"sync"
)

// 日志轮转默认参数：单文件上限与保留份数。
const (
	DefaultLogMaxBytes int64 = 5 << 20 // 5 MiB
	DefaultLogKeep           = 3
)

// RotateWriter 追加写日志文件，超过 maxBytes 后轮转：
// name → name.1 → … → name.keep（超出保留份数的最旧文件被覆盖删除）。
type RotateWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int

	f    *os.File
	size int64
}

// NewRotateWriter 打开（必要时创建）日志文件。
func NewRotateWriter(path string, maxBytes int64, keep int) (*RotateWriter, error) {
	if keep < 1 {
		keep = 1
	}
	w := &RotateWriter{path: path, maxBytes: maxBytes, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotateWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("打开日志文件 %s 失败: %w", w.path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("读取日志文件 %s 大小失败: %w", w.path, err)
	}
	w.f, w.size = f, info.Size()
	return nil
}

// Write 实现 io.Writer；写入前超过大小上限则先轮转。
func (w *RotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, fmt.Errorf("日志文件已关闭")
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate 关闭当前文件并整体移位：.2→.3、.1→.2、name→.1，然后重新打开。
func (w *RotateWriter) rotate() error {
	w.f.Close()
	for i := w.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	_ = os.Rename(w.path, w.path+".1")
	return w.open()
}

// Close 关闭当前日志文件。
func (w *RotateWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
