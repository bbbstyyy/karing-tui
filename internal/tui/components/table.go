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
	l.Selectable = nil
	l.SetTableSelectable(columns, rows, keys, nil)
}

// SetTableSelectable is SetTable plus per-row selectability: rows marked false
// (group headings) are skipped by navigation and rendered as plain text.
func (l *SimpleList) SetTableSelectable(columns []Column, rows [][]string, keys []string, selectable []bool) {
	items := make([]string, len(rows))
	for i, row := range rows {
		items[i] = strings.Join(row, " · ")
	}
	l.Columns, l.Rows = columns, rows
	l.Selectable = selectable
	l.tableVersion++
	l.SetItems(items, keys)
}

type tableColumn struct{ index, width int }

// tableColumnsFor 返回给定宽度下的列布局。
// 布局只依赖 Columns 与 width：SetTable* 递增 tableVersion 使缓存失效，
// 同一宽度下表头与全部可见行共享同一份布局，不再每行重算（C12 改法 1；
// 每帧 31 次「排序/裁剪 + 分配」收敛为 1 次）。
func (l *SimpleList) tableColumnsFor(width int) []tableColumn {
	if l.tcols != nil && l.tcolsWidth == width && l.tcolsVersion == l.tableVersion {
		return l.tcols
	}
	cols := l.computeTableColumns(width)
	l.tcolsWidth, l.tcolsVersion, l.tcols = width, l.tableVersion, cols
	return cols
}

func (l *SimpleList) computeTableColumns(width int) []tableColumn {
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

// tableRow 按 cols 布局渲染一行；cols 由调用方经 tableColumnsFor 取得。
func (l *SimpleList) tableRow(values []string, cols []tableColumn) string {
	var cells []string
	for _, c := range cols {
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
