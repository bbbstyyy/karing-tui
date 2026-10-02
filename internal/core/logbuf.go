package core

import (
	"bytes"
	"sync"

	"github.com/bbbstyyy/karing-tui/internal/redact"
)

// Keep the ordinary recent-log payload near the previous 16 KiB-per-line bound
// without changing a log record's contents. Oversized individual records are
// preserved whole; later records rotate them out instead of truncating them.
const retainedLogBytesPerLine = 16 << 10

// LogBuf is a thread-safe recent-log buffer bounded by both record count and a
// soft total-byte target. A single record may exceed that target because records
// are never shortened.
type LogBuf struct {
	mu            sync.Mutex
	lines         []string
	max           int
	maxBytes      int
	retainedBytes int
	drop          int
	version       uint64
}

func NewLogBuf(max int) *LogBuf {
	if max <= 0 {
		max = 500
	}
	return &LogBuf{max: max, maxBytes: max * retainedLogBytesPerLine}
}

// Write avoids allocating a split slice proportional to the incoming payload.
func (b *LogBuf) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			i = len(p)
		}
		if i > 0 {
			b.AppendLine(string(p[:i]))
		}
		if i == len(p) {
			break
		}
		p = p[i+1:]
	}
	return n, nil
}

func (b *LogBuf) AppendLine(line string) {
	line = redact.Text(line)
	b.mu.Lock()
	defer b.mu.Unlock()

	lineBytes := len(line)
	// Rotate whole records only. This keeps ordinary long-running memory bounded
	// while ensuring a long diagnostic record is never silently shortened.
	for len(b.lines) > 0 &&
		(len(b.lines) >= b.max || b.retainedBytes+lineBytes > b.maxBytes) {
		b.retainedBytes -= len(b.lines[0])
		b.lines[0] = ""
		b.lines = b.lines[1:]
		b.drop++
	}
	b.lines = append(b.lines, line)
	b.retainedBytes += lineBytes
	b.version++
}

// Snapshot returns stable absolute line identities and a write counter under
// the same lock; its returned slice is independent of future writes.
func (b *LogBuf) Snapshot() (first int, version uint64, lines []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.drop, b.version, append([]string(nil), b.lines...)
}

func (b *LogBuf) Version() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.version
}

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

func (b *LogBuf) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.drop
}
