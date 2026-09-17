package core

import (
	"github.com/bbbstyyy/karing-tui/internal/redact"
	"strings"
	"sync"
)

// LogBuf 是线程安全的定长日志环形缓冲，供 TUI 实时查看最近日志。
type LogBuf struct {
	mu      sync.Mutex
	lines   []string
	max     int
	drop    int    // 因缓冲满而丢弃的总行数（即 Snapshot 的绝对行号基准）
	version uint64 // 单调递增写入计数：每次 AppendLine 自增（含回绕挤掉的写入）
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
	line = redact.Text(line)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) >= b.max {
		b.lines = b.lines[1:]
		b.drop++
	}
	b.lines = append(b.lines, line)
	b.version++
}

// Snapshot returns absolute line identities, content, and a monotonically
// increasing write counter under the same lock. Anchors remain stable when new
// lines arrive or the ring buffer wraps.
//
// version 在每次 AppendLine 后自增（回绕那次写入同样自增，drop 另计）：
// 调用方（logs 页）用它做增量缓存的失效键——C11 之前 drop 只在回绕时变化，
// 无法表达「有新行写入」，非回绕的追加会被误判为「无变化」。
func (b *LogBuf) Snapshot() (first int, version uint64, lines []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.drop, b.version, append([]string(nil), b.lines...)
}

// Version 返回当前写入计数。供调用方做增量缓存的廉价预检（C11）：只读
// 一个整数、不拷贝内容；内容本身以 Snapshot 为准（两次加锁之间可能又有
// 写入，以 Snapshot 返回的 version 为权威值）。
func (b *LogBuf) Version() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.version
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
