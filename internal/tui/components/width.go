// Package components provides the shared TUI controls and layout primitives.
package components

import (
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// Cell width accounting deliberately counts one code point at a time instead of
// using grapheme clustering. Clustering reports a ZWJ emoji sequence (👩‍💻) or a
// regional-indicator flag (🇭🇰) as one two-cell grapheme, while terminals that do
// not cluster — tmux up to 3.2, plain xterm, and the Linux acceptance host — draw
// the same sequence with four cells. Under-counting makes a line wider than the
// terminal: the pane wraps it, every row costs an extra line and the fixed
// footer is pushed off screen (verified on the Linux host with emoji node names).
// Over-counting on a clustering terminal only leaves a small right margin, so
// counting per code point is the safe default everywhere.

// cellCondition pins the ambiguous-width rule so a screen renders identically on
// every host: go-runewidth's default condition reads the process locale and turns
// ambiguous characters ("…", "·", "↑") into two cells under LANG=zh_CN, which
// would make the layout depend on the environment. Measured against tmux 3.2a
// (cursor columns): 👩‍💻=4, 🇭🇰=2, ❤️️=1, ⭐️=2, 中文=4 — this condition matches all
// of them and only over-counts ambiguous characters, the conservative direction:
// over-counting leaves a margin, while under-counting wraps the line and pushes
// the fixed footer off screen. StrictEmojiNeutral keeps ❤️ at one cell.
var cellCondition = runewidth.Condition{EastAsianWidth: true, StrictEmojiNeutral: true}

// RuneCellWidth reports the cells a single rune occupies. Joiners and variation
// selectors take no cells; unprintable runes are treated as zero width.
func RuneCellWidth(r rune) int {
	switch r {
	case 0x200B, 0x200C, 0x200D, 0xFE0E, 0xFE0F:
		return 0
	}
	if width := cellCondition.RuneWidth(r); width > 0 {
		return width
	}
	return 0
}

// CellWidth measures terminal cells for a possibly styled string.
func CellWidth(s string) int {
	total := 0
	for _, r := range stripANSI(s) {
		total += RuneCellWidth(r)
	}
	return total
}

// DisplayWidth is the cell measurement used by every layout decision.
func DisplayWidth(s string) int { return CellWidth(s) }

// escapeLen returns the byte length of the ANSI sequence starting at s[0], or 0.
func escapeLen(s string) int {
	if len(s) == 0 || s[0] != 0x1b {
		return 0
	}
	if len(s) == 1 {
		return 1
	}
	switch s[1] {
	case '[': // CSI: parameters then a final byte 0x40-0x7e
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return len(s)
	case ']': // OSC: terminated by BEL or ST
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return len(s)
	default: // two-byte escape
		return 2
	}
}

// stripANSI removes escape sequences so only printable runes are measured.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); {
		if n := escapeLen(s[i:]); n > 0 {
			i += n
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// Clip truncates to at most width cells using the same accounting as DisplayWidth,
// keeping escape sequences intact and closing any open style before the ellipsis.
func Clip(s string, width int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if width <= 0 {
		return ""
	}
	if CellWidth(s) <= width {
		return s
	}
	ellipsis := RuneCellWidth('…')
	limit := width - ellipsis // reserve the cells the ellipsis itself needs
	var out strings.Builder
	used := 0
	styled := false
	for i := 0; i < len(s); {
		if n := escapeLen(s[i:]); n > 0 {
			out.WriteString(s[i : i+n])
			styled = true
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		cell := RuneCellWidth(r)
		if used+cell > limit {
			break
		}
		out.WriteString(s[i : i+size])
		used += cell
		i += size
	}
	if styled {
		out.WriteString("\x1b[0m")
	}
	out.WriteString("…")
	return out.String()
}

// Wrap hard-wraps plain text at width cells. Styled input falls back to the
// escape-aware wrapper so styles are not broken across lines.
func Wrap(s string, width int) string {
	width = max(1, width)
	if strings.Contains(s, "\x1b") {
		return wrapStyled(s, width)
	}
	var out strings.Builder
	line := 0
	for _, r := range s {
		if r == '\n' {
			out.WriteRune(r)
			line = 0
			continue
		}
		cell := RuneCellWidth(r)
		if line+cell > width {
			out.WriteByte('\n')
			line = 0
		}
		out.WriteRune(r)
		line += cell
	}
	return out.String()
}

// wrapStyled wraps escape-aware text by walking escapes with the text so a line
// break never separates an escape sequence from the runes it styles.
func wrapStyled(s string, width int) string {
	var out strings.Builder
	line := 0
	for i := 0; i < len(s); {
		if n := escapeLen(s[i:]); n > 0 {
			out.WriteString(s[i : i+n])
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == '\n' {
			out.WriteRune(r)
			line = 0
			i += size
			continue
		}
		cell := RuneCellWidth(r)
		if line+cell > width {
			out.WriteByte('\n')
			line = 0
		}
		out.WriteString(s[i : i+size])
		line += cell
		i += size
	}
	return out.String()
}
