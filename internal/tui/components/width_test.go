package components

import "testing"

// Terminals that do not cluster grapheme sequences (tmux <= 3.2, plain xterm)
// draw a ZWJ sequence or a regional-indicator flag with one cell per code point.
// Over-counting only leaves a margin; under-counting wraps the line and breaks
// the fixed footer, so the layout must not follow grapheme clustering here.
func TestCellWidthCountsCodePointsNotGraphemes(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"ascii", "Node-0000", 9},
		{"cjk and combining", "中文e\u0301", 5},
		{"zwj sequence", "👩\u200d💻", 4},
		{"flag pair", "🇭🇰", 2},
		{"variation selector", "❤\ufe0f", 1},
		{"ambiguous ellipsis", "…", 2},
		{"ambiguous middle dot", "·", 2},
		{"styled text is measured without escapes", "\x1b[2mabc\x1b[0m", 3},
		{"newline is not counted here", "a\nb", 2},
	}
	for _, test := range cases {
		if got := DisplayWidth(test.text); got != test.want {
			t.Errorf("%s: DisplayWidth(%q) = %d, want %d", test.name, test.text, got, test.want)
		}
	}
	// Measured against tmux 3.2a on the Linux acceptance host: it reports
	// cursor columns 4 for a ZWJ emoji pair, 2 for a flag, 1 for "❤️".
	if got := DisplayWidth("  Node-0000 中文👩\u200d💻e\u0301 长名称"); got != 28 {
		t.Errorf("row width = %d, want 28", got)
	}
}

func TestClipKeepsWidthAndStyles(t *testing.T) {
	long := "  Node-0000 中文👩\u200d💻e\u0301 长名称 未测 启用 http 127.0.0.1"
	for _, width := range []int{8, 16, 24, 32} {
		clipped := Clip(long, width)
		if got := DisplayWidth(clipped); got > width {
			t.Errorf("Clip(%d) = %d cells: %q", width, got, clipped)
		}
	}
	if got := Clip("short", 10); got != "short" {
		t.Errorf("Clip left a fitting string alone: %q", got)
	}
	styled := Clip("\x1b[31mred text that is far too long for the width\x1b[0m", 12)
	if got := DisplayWidth(styled); got > 12 {
		t.Errorf("styled clip width = %d", got)
	}
	if !containsEscape(styled, "\x1b[0m") {
		t.Errorf("styled clip must close the open style: %q", styled)
	}
	if got := Clip("line\nbreak", 20); got != "line break" {
		t.Errorf("Clip must flatten newlines: %q", got)
	}
}

func TestWrapNeverExceedsWidth(t *testing.T) {
	text := "Enter 查看节点详情 · 中文名称👩\u200d💻 很长的一段提示文本 · ? 帮助"
	for _, width := range []int{10, 20, 33, 64} {
		for _, line := range splitLines(Wrap(text, width)) {
			if got := DisplayWidth(line); got > width {
				t.Errorf("Wrap(%d) produced %d cells: %q", width, got, line)
			}
		}
	}
	styled := Wrap("\x1b[2m中文提示👩\u200d💻很长\x1b[0m", 8)
	for _, line := range splitLines(styled) {
		if got := DisplayWidth(line); got > 8 {
			t.Errorf("styled wrap produced %d cells: %q", got, line)
		}
	}
	if got := Wrap("a\nb", 10); got != "a\nb" {
		t.Errorf("Wrap changed existing line breaks: %q", got)
	}
}

func TestPadFillsUsingCellWidth(t *testing.T) {
	padded := Pad("👩\u200d💻", 6)
	if got := DisplayWidth(padded); got != 6 {
		t.Errorf("Pad width = %d, want 6 (%q)", got, padded)
	}
}

func containsEscape(value, escape string) bool {
	return len(value) >= len(escape) && indexOf(value, escape) >= 0
}

func indexOf(value, sub string) int {
	for i := 0; i+len(sub) <= len(value); i++ {
		if value[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func splitLines(value string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(value); i++ {
		if value[i] == '\n' {
			lines = append(lines, value[start:i])
			start = i + 1
		}
	}
	return append(lines, value[start:])
}
