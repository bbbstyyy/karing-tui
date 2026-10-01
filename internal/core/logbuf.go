package core

import (
	"bytes"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bbbstyyy/karing-tui/internal/redact"
)

// MaxLogLineBytes bounds retained memory as well as the number of lines.
// Oversized records are still written in full to the rotating disk log.
const MaxLogLineBytes = 16 << 10

// LogBuf is a thread-safe, bounded recent-log buffer.
type LogBuf struct {
	mu      sync.Mutex
	lines   []string
	max     int
	drop    int
	version uint64
}

func NewLogBuf(max int) *LogBuf {
	if max <= 0 {
		max = 500
	}
	return &LogBuf{max: max}
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
	// Redact before truncation so a cut credential cannot escape redaction.
	line = redact.Text(line)
	if len(line) > MaxLogLineBytes {
		const suffix = " [truncated]"
		end := MaxLogLineBytes - len(suffix)
		for end > 0 && !utf8.RuneStart(line[end]) {
			end--
		}
		line = line[:end] + suffix
	}
	// Do not retain a large backing string through a short substring.
	line = strings.Clone(line)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) >= b.max {
		b.lines[0] = ""
		b.lines = b.lines[1:]
		b.drop++
	}
	b.lines = append(b.lines, line)
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
