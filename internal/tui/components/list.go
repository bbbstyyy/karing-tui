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
	offset  int
}

func (l *SimpleList) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false, nil
	}
	switch keys.Navigation(key.String()) {
	case keys.Up:
		l.Cursor--
	case keys.Down:
		l.Cursor++
	case keys.PageUp:
		l.Cursor -= l.page()
	case keys.PageDown:
		l.Cursor += l.page()
	case keys.First:
		l.Cursor = 0
	case keys.Last:
		l.Cursor = len(l.Items) - 1
	default:
		return false, nil
	}
	l.Cursor = max(0, min(l.Cursor, len(l.Items)-1))
	return true, nil
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
		if k == key {
			l.Cursor = i
			return
		}
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
