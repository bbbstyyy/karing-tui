package core

import (
	"fmt"
	"io"
	"os"
	"sync"
)

const (
	DefaultLogMaxBytes int64 = 5 << 20
	DefaultLogKeep           = 3
)

// RotateWriter bounds both the active file and its retained backups. A failed
// rotation is reported and retried by the next Write; it never leaves a closed
// descriptor masquerading as an open log file.
type RotateWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
	closed   bool
}

func NewRotateWriter(path string, maxBytes int64, keep int) (*RotateWriter, error) {
	if keep < 1 {
		keep = 1
	}
	if maxBytes <= 0 {
		maxBytes = DefaultLogMaxBytes
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

func (w *RotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, fmt.Errorf("日志文件已关闭: %w", os.ErrClosed)
	}
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	written := 0
	for len(p) > 0 {
		if w.size > 0 && int64(len(p)) > w.maxBytes-w.size {
			if err := w.rotate(); err != nil {
				return written, err
			}
		}
		// A single large Write must not bypass the per-file size limit.
		chunk := int(min(int64(len(p)), w.maxBytes-w.size))
		n, err := w.f.Write(p[:chunk])
		w.size += int64(n)
		written += n
		if err != nil {
			return written, err
		}
		if n != chunk {
			return written, io.ErrShortWrite
		}
		p = p[n:]
	}
	return written, nil
}

func (w *RotateWriter) rotate() error {
	if w.f != nil {
		err := w.f.Close()
		w.f = nil
		if err != nil {
			return err
		}
	}
	for i := w.keep - 1; i >= 1; i-- {
		source := fmt.Sprintf("%s.%d", w.path, i)
		if _, err := os.Lstat(source); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := os.Rename(source, fmt.Sprintf("%s.%d", w.path, i+1)); err != nil {
			return fmt.Errorf("轮转日志失败: %w", err)
		}
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		return fmt.Errorf("轮转日志失败: %w", err)
	}
	return w.open()
}

func (w *RotateWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
