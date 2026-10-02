package core

import (
	"fmt"
	"io"
	"sync"
)

// NewLogWriter keeps process output flowing even when the disk log is full or
// unavailable. Returning a disk error to os/exec would stop its pipe reader and
// can terminate the child with SIGPIPE. The bounded memory log remains usable;
// one warning per outage makes the loss of persistent logging visible.
// The caller retains ownership of disk and must close it after process exit.
func NewLogWriter(disk io.Writer, buffer *LogBuf) io.Writer {
	return &logWriter{disk: disk, buffer: buffer}
}

type logWriter struct {
	mu      sync.Mutex
	disk    io.Writer
	buffer  *LogBuf
	failing bool
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buffer != nil {
		_, _ = w.buffer.Write(p)
	}
	if w.disk == nil {
		return len(p), nil
	}
	n, err := w.disk.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		if !w.failing && w.buffer != nil {
			w.buffer.AppendLine(fmt.Sprintf("[karing] disk log unavailable; continuing with bounded memory logs: %v", err))
		}
		w.failing = true
	} else if w.failing {
		w.failing = false
		if w.buffer != nil {
			w.buffer.AppendLine("[karing] disk logging recovered")
		}
	}
	return len(p), nil
}
