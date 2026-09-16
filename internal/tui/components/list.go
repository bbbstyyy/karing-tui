// Package components provides the shared TUI controls and layout primitives.
package components

import (
	"fmt"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/tui/keys"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
	tea "github.com/charmbracelet/bubbletea"
)

// SimpleList keeps the selected row inside its viewport. Height includes the
// position indicator; callers allocate space for their own headers and footer.
type SimpleList struct {
	Title   string
	Items   []string
	Keys    []string
	Cursor  int
	Height  int
	Width   int
	Columns []Column
	Rows    [][]string
	// Selectable marks which rows can be selected; nil means all rows can.
	// Group headings and other decorations are set to false: navigation skips
	// them and they render as plain (unstyled-by-column) text.
	Selectable []bool
	offset     int
}

func (l *SimpleList) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false, nil
	}
	switch keys.Navigation(key.String()) {
	case keys.Up:
		l.MoveCursor(-1)
	case keys.Down:
		l.MoveCursor(1)
	case keys.PageUp:
		l.MoveCursor(-l.page())
	case keys.PageDown:
		l.MoveCursor(l.page())
	case keys.First:
		l.JumpTo(1)
	case keys.Last:
		l.JumpTo(-1)
	default:
		return false, nil
	}
	return true, nil
}

// IsSelectable reports whether row i can be selected (nil Selectable = all).
func (l *SimpleList) IsSelectable(i int) bool {
	if i < 0 || i >= len(l.Items) {
		return false
	}
	if len(l.Selectable) == 0 || i >= len(l.Selectable) {
		return true
	}
	return l.Selectable[i]
}

// MoveCursor moves the cursor by delta and lands on the nearest selectable row
// in the direction of travel, so headings never receive the cursor.
func (l *SimpleList) MoveCursor(delta int) {
	if len(l.Items) == 0 {
		l.Cursor = 0
		return
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	target := max(0, min(l.Cursor+delta, len(l.Items)-1))
	if i := l.seek(target, step); i >= 0 {
		l.Cursor = i
		return
	}
	if i := l.seek(target, -step); i >= 0 {
		l.Cursor = i
		return
	}
	l.Cursor = target
}

// JumpTo selects the first (step > 0) or last (step < 0) selectable row.
func (l *SimpleList) JumpTo(step int) {
	if len(l.Items) == 0 {
		return
	}
	from := 0
	if step < 0 {
		from = len(l.Items) - 1
	}
	if i := l.seek(from, step); i >= 0 {
		l.Cursor = i
	}
}

func (l *SimpleList) seek(from, step int) int {
	for i := from; i >= 0 && i < len(l.Items); i += step {
		if l.IsSelectable(i) {
			return i
		}
	}
	return -1
}

func (l *SimpleList) page() int {
	if len(l.Columns) > 0 {
		return max(1, l.Height-2)
	}
	return max(1, l.Height-1)
}

func (l *SimpleList) Selected() (string, bool) {
	if l.Cursor < 0 || l.Cursor >= len(l.Items) {
		return "", false
	}
	return l.Items[l.Cursor], true
}

func (l *SimpleList) SelectedKey() string {
	if l.Cursor < 0 || l.Cursor >= len(l.Keys) {
		return ""
	}
	return l.Keys[l.Cursor]
}

func (l *SimpleList) SelectKey(key string) {
	for i, k := range l.Keys {
		if k != key {
			continue
		}
		// 命中不可选行（层标题等）时，落到其后最近的可选行
		if l.IsSelectable(i) {
			l.Cursor = i
			return
		}
		if j := l.seek(i, 1); j >= 0 {
			l.Cursor = j
			return
		}
		if j := l.seek(i, -1); j >= 0 {
			l.Cursor = j
			return
		}
		break
	}
	l.Cursor = max(0, min(l.Cursor, len(l.Items)-1))
}

// SetItems restores selection by stable identity, or chooses the adjacent row
// if the selected object disappeared. Offset is intentionally retained.
func (l *SimpleList) SetItems(items, keys []string) {
	selected := l.SelectedKey()
	l.Items, l.Keys = items, keys
	l.SelectKey(selected)
}

func (l *SimpleList) View(empty string) string {
	height := l.Height
	if height <= 0 {
		height = 10
	}
	width := l.Width
	if width <= 0 {
		width = 78
	}
	if len(l.Items) == 0 {
		return Fit(styles.Dim.Render(empty), width, height)
	}
	l.Cursor = max(0, min(l.Cursor, len(l.Items)-1))
	if !l.IsSelectable(l.Cursor) {
		l.MoveCursor(1)
	}
	rows := max(1, height-1)
	var header string
	if len(l.Columns) > 0 && height >= 3 {
		titles := make([]string, len(l.Columns))
		for i, c := range l.Columns {
			titles[i] = c.Title
		}
		header = styles.Dim.Render("  " + l.tableRow(titles, max(1, width-2)))
		rows--
	}
	if l.Cursor < l.offset {
		l.offset = l.Cursor
	}
	if l.Cursor >= l.offset+rows {
		l.offset = l.Cursor - rows + 1
	}
	l.offset = max(0, min(l.offset, len(l.Items)-rows))
	end := min(len(l.Items), l.offset+rows)
	lines := make([]string, 0, height)
	if header != "" {
		lines = append(lines, header)
	}
	for i := l.offset; i < end; i++ {
		if !l.IsSelectable(i) {
			// 分组标题等装饰行：整行铺满、不套用列宽、不显示光标
			lines = append(lines, Clip(l.Items[i], width))
			continue
		}
		prefix := "  "
		if i == l.Cursor {
			prefix = "> "
		}
		item := l.Items[i]
		if len(l.Columns) > 0 && i < len(l.Rows) {
			item = l.tableRow(l.Rows[i], max(1, width-2))
		}
		line := Clip(prefix+item, width)
		if i == l.Cursor {
			line = styles.Selected.Render(line)
		}
		lines = append(lines, line)
	}
	if height > 1 {
		for len(lines) < height-1 {
			lines = append(lines, "")
		}
		lines = append(lines, styles.Dim.Render(fmt.Sprintf("%d/%d · PgUp/PgDn 翻页 · Home/End 首尾", l.Cursor+1, len(l.Items))))
	}
	return Fit(strings.Join(lines, "\n"), width, height)
}
