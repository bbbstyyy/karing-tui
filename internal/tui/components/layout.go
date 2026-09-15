package components

import (
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/tui/keys"
)

// Clip, Pad, DisplayWidth and Wrap live in width.go: every layout decision uses
// the conservative per-code-point cell accounting defined there.

// Pad clips to width and fills the remaining cells with spaces.
func Pad(s string, width int) string {
	s = Clip(s, width)
	return s + strings.Repeat(" ", max(0, width-DisplayWidth(s)))
}

// Fit is a final width guard and blank-line filler. Scrollable controls budget
// their own content first; it never shifts the whole page to its last lines.
func Fit(s string, width, height int) string {
	if height <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for i := range lines {
		lines[i] = Clip(lines[i], width)
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// TextView supports long details, help and errors using the same bounded area.
type TextView struct{ Offset int }

func (v *TextView) Move(key string, height int) bool {
	switch keys.Navigation(key) {
	case keys.Up:
		v.Offset--
	case keys.Down:
		v.Offset++
	case keys.PageUp:
		v.Offset -= max(1, height-1)
	case keys.PageDown:
		v.Offset += max(1, height-1)
	case keys.First:
		v.Offset = 0
	case keys.Last:
		v.Offset = int(^uint(0) >> 1)
	default:
		return false
	}
	v.Offset = max(0, v.Offset)
	return true
}

func (v *TextView) View(s string, width, height int) string {
	lines := strings.Split(Wrap(s, width), "\n")
	v.Offset = max(0, min(v.Offset, len(lines)-height))
	return Fit(strings.Join(lines[v.Offset:min(len(lines), v.Offset+max(0, height))], "\n"), width, height)
}
