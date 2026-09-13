// Package components 提供 TUI 公共组件：列表、表单、确认弹窗。
package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
)

// SimpleList 是极简可导航列表，页面按需包装数据源。
type SimpleList struct {
	Title  string
	Items  []string
	Cursor int

	// Height 为视窗高度（可见行数）；0 表示不限高、整表渲染（默认行为）。
	// 分类库这类上千条的列表必须设置，否则一次渲染会撑破终端。
	Height int
	// offset 是视窗首行下标，随 Cursor 移动惰性调整。
	offset int
}

// Update 处理上下键移动；返回 false 表示按键未被消费。
func (l *SimpleList) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false, nil
	}
	switch key.String() {
	case "up", "k":
		if l.Cursor > 0 {
			l.Cursor--
		}
		return true, nil
	case "down", "j":
		if l.Cursor < len(l.Items)-1 {
			l.Cursor++
		}
		return true, nil
	case "pgup":
		l.Cursor -= l.page()
		if l.Cursor < 0 {
			l.Cursor = 0
		}
		return true, nil
	case "pgdown":
		l.Cursor += l.page()
		if l.Cursor > len(l.Items)-1 {
			l.Cursor = len(l.Items) - 1
		}
		if l.Cursor < 0 {
			l.Cursor = 0
		}
		return true, nil
	case "g", "home":
		l.Cursor = 0
		return true, nil
	case "G", "end":
		l.Cursor = len(l.Items) - 1
		if l.Cursor < 0 {
			l.Cursor = 0
		}
		return true, nil
	}
	return false, nil
}

// page 返回翻页步长。
func (l *SimpleList) page() int {
	if l.Height > 1 {
		return l.Height - 1
	}
	return 10
}

// Selected 返回当前选中项；空列表返回 ("", false)。
func (l *SimpleList) Selected() (string, bool) {
	if l.Cursor < 0 || l.Cursor >= len(l.Items) {
		return "", false
	}
	return l.Items[l.Cursor], true
}

// View 渲染列表；空列表显示 empty 提示。
// Height > 0 时只渲染光标所在的视窗，并在上下方提示被折叠的条数。
func (l *SimpleList) View(empty string) string {
	if len(l.Items) == 0 {
		return styles.Dim.Render(empty)
	}
	start, end := 0, len(l.Items)
	if l.Height > 0 && len(l.Items) > l.Height {
		// 光标移出视窗时才滚动，保持浏览位置稳定
		if l.Cursor < l.offset {
			l.offset = l.Cursor
		}
		if l.Cursor >= l.offset+l.Height {
			l.offset = l.Cursor - l.Height + 1
		}
		if max := len(l.Items) - l.Height; l.offset > max {
			l.offset = max
		}
		if l.offset < 0 {
			l.offset = 0
		}
		start, end = l.offset, l.offset+l.Height
	} else {
		l.offset = 0
	}

	var b strings.Builder
	if start > 0 {
		b.WriteString(styles.Dim.Render(fmt.Sprintf("  ↑ 上方还有 %d 条", start)) + "\n")
	}
	for i := start; i < end; i++ {
		if i == l.Cursor {
			b.WriteString(styles.Selected.Render("> "+l.Items[i]) + "\n")
		} else {
			b.WriteString("  " + l.Items[i] + "\n")
		}
	}
	if rest := len(l.Items) - end; rest > 0 {
		b.WriteString(styles.Dim.Render(fmt.Sprintf("  ↓ 下方还有 %d 条", rest)) + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// FormField 表单字段。
type FormField struct {
	Label string
	Key   string
	input textinput.Model
}

// Value 返回字段当前值。
func (f *FormField) Value() string { return f.input.Value() }

// SetValue 设置字段值。
func (f *FormField) SetValue(v string) { f.input.SetValue(v) }

// Form 纵向表单：多字段文本录入，Tab/Shift+Tab 切换，Enter 提交，Esc 取消。
type Form struct {
	Title   string
	Fields  []FormField
	focus   int
	Editing bool // 表单是否处于激活状态（拦截全局键位）
}

// NewForm 创建表单；placeholder 为字段占位提示，与 fields 一一对应。
func NewForm(title string, labels, keys, placeholders []string) Form {
	f := Form{Title: title}
	for i, label := range labels {
		input := textinput.New()
		input.Placeholder = placeholders[i]
		input.CharLimit = 512
		if i == 0 {
			input.Focus()
		}
		f.Fields = append(f.Fields, FormField{Label: label, Key: keys[i], input: input})
	}
	return f
}

// ValueByKey 按 Key 取字段值。
func (f *Form) ValueByKey(key string) string {
	for i := range f.Fields {
		if f.Fields[i].Key == key {
			return f.Fields[i].Value()
		}
	}
	return ""
}

// SetValueByKey 按 Key 设字段值（编辑已有记录时用）。
func (f *Form) SetValueByKey(key, value string) {
	for i := range f.Fields {
		if f.Fields[i].Key == key {
			f.Fields[i].SetValue(value)
		}
	}
}

// Reset 清空全部字段并把焦点移到第一个字段。
func (f *Form) Reset() {
	for i := range f.Fields {
		f.Fields[i].SetValue("")
		f.Fields[i].input.Blur()
	}
	f.focus = 0
	if len(f.Fields) > 0 {
		f.Fields[0].input.Focus()
	}
}

// Update 处理表单按键。
func (f *Form) Update(msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "tab", "down":
		f.Fields[f.focus].input.Blur()
		f.focus = (f.focus + 1) % len(f.Fields)
		f.Fields[f.focus].input.Focus()
		return nil
	case "shift+tab", "up":
		f.Fields[f.focus].input.Blur()
		f.focus = (f.focus - 1 + len(f.Fields)) % len(f.Fields)
		f.Fields[f.focus].input.Focus()
		return nil
	}
	var cmd tea.Cmd
	f.Fields[f.focus].input, cmd = f.Fields[f.focus].input.Update(msg)
	return cmd
}

// View 渲染表单。
func (f *Form) View() string {
	var b strings.Builder
	b.WriteString(styles.Title.Render(f.Title) + "\n\n")
	for i := range f.Fields {
		cursor := "  "
		if i == f.focus {
			cursor = "> "
		}
		b.WriteString(cursor + styles.Dim.Render(f.Fields[i].Label+": ") + f.Fields[i].input.View() + "\n")
	}
	b.WriteString("\n" + styles.Dim.Render("Tab/Shift+Tab 切换字段 · Enter 提交 · Esc 取消"))
	return b.String()
}

// ConfirmMsg 确认弹窗结果消息。Confirmed 为用户选择。
type ConfirmMsg struct {
	ID        string // 区分不同确认场景
	Confirmed bool
}

// Confirm 确认弹窗。
type Confirm struct {
	ID     string
	Prompt string
	Active bool
}

// NewConfirm 创建确认弹窗。
func NewConfirm(id, prompt string) Confirm {
	return Confirm{ID: id, Prompt: prompt, Active: true}
}

// Update 处理弹窗按键；返回结果消息。
func (c *Confirm) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || !c.Active {
		return false, nil
	}
	switch key.String() {
	case "y", "Y", "enter":
		c.Active = false
		id := c.ID
		return true, func() tea.Msg { return ConfirmMsg{ID: id, Confirmed: true} }
	case "n", "N", "esc", "q":
		c.Active = false
		id := c.ID
		return true, func() tea.Msg { return ConfirmMsg{ID: id, Confirmed: false} }
	}
	return false, nil
}

// View 渲染弹窗。
func (c *Confirm) View() string {
	return styles.HelpOverlay.Render(
		styles.Title.Render("确认") + "\n\n" + c.Prompt + "\n\n" +
			styles.Dim.Render("y 确认 · n/Esc 取消"),
	)
}
