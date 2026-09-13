package core

import (
	"strings"
	"sync"
)

// LogBuf 是线程安全的定长日志环形缓冲，供 TUI 实时查看最近日志。
type LogBuf struct {
	mu    sync.Mutex
	lines []string
	max   int
	drop  int // 因缓冲满而丢弃的总行数
}

// NewLogBuf 创建容量为 max 行的日志缓冲。
func NewLogBuf(max int) *LogBuf {
	if max <= 0 {
		max = 500
	}
	return &LogBuf{max: max}
}

// Write 实现 io.Writer，按行拆分写入（用于挂接进程输出）。
func (b *LogBuf) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			b.AppendLine(line)
		}
	}
	return len(p), nil
}

// AppendLine 追加一行日志。
func (b *LogBuf) AppendLine(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) >= b.max {
		b.lines = b.lines[1:]
		b.drop++
	}
	b.lines = append(b.lines, line)
}

// Tail 返回最近 n 行日志；n <= 0 表示全部。
func (b *LogBuf) Tail(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > len(b.lines) {
		n = len(b.lines)
	}
	out := make([]string, n)
	copy(out, b.lines[len(b.lines)-n:])
	return out
}

// Dropped 返回被丢弃的行数（提示用户查看完整日志文件）。
func (b *LogBuf) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.drop
}
