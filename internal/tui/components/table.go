package components

import "strings"

// Column keeps essential columns visible. Optional columns disappear in
// descending Priority order; the first column receives the remaining space.
type Column struct {
	Title    string
	Width    int
	Priority int
	Right    bool
}

func (l *SimpleList) SetTable(columns []Column, rows [][]string, keys []string) {
	items := make([]string, len(rows))
	for i, row := range rows {
		items[i] = strings.Join(row, " · ")
	}
	l.Columns, l.Rows = columns, rows
	l.SetItems(items, keys)
}

type tableColumn struct{ index, width int }

func (l *SimpleList) tableColumns(width int) []tableColumn {
	var cols []tableColumn
	for i, c := range l.Columns {
		cols = append(cols, tableColumn{i, max(4, c.Width)})
	}
	needed := func() int {
		n := max(0, len(cols)-1)
		for _, c := range cols {
			n += c.width
		}
		return n
	}
	for len(cols) > 1 && needed() > width {
		remove, priority := -1, 0
		for i, c := range cols {
			if i > 0 && l.Columns[c.index].Priority > priority {
				remove, priority = i, l.Columns[c.index].Priority
			}
		}
		if remove < 0 {
			break
		}
		cols = append(cols[:remove], cols[remove+1:]...)
	}
	if len(cols) > 0 {
		cols[0].width = max(1, cols[0].width+width-needed())
	}
	return cols
}

func (l *SimpleList) tableRow(values []string, width int) string {
	var cells []string
	for _, c := range l.tableColumns(width) {
		value := ""
		if c.index < len(values) {
			value = values[c.index]
		}
		value = Clip(value, c.width)
		if l.Columns[c.index].Right {
			value = strings.Repeat(" ", max(0, c.width-DisplayWidth(value))) + value
		} else {
			value = Pad(value, c.width)
		}
		cells = append(cells, value)
	}
	return strings.Join(cells, " ")
}
