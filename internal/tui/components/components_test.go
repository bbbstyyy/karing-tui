package components

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func keyRunes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func assertSize(t *testing.T, s string, width, height int) {
	t.Helper()
	lines := strings.Split(s, "\n")
	if len(lines) > height {
		t.Fatalf("rendered %d rows, budget %d", len(lines), height)
	}
	for _, line := range lines {
		if got := ansi.StringWidth(line); got > width {
			t.Fatalf("rendered %d cells, budget %d: %q", got, width, line)
		}
	}
}

func TestListKeepsThousandthAndFirstRowVisibleAfterResize(t *testing.T) {
	l := SimpleList{}
	for i := range 1000 {
		l.Items = append(l.Items, fmt.Sprintf("node-%04d 中文 👩‍💻 é %s", i, strings.Repeat("名称", 30)))
		l.Keys = append(l.Keys, fmt.Sprint(i))
	}
	for _, size := range [][2]int{{160, 36}, {80, 15}, {60, 9}, {120, 21}, {80, 15}} {
		l.Width, l.Height = size[0], size[1]
		l.Update(tea.KeyMsg{Type: tea.KeyEnd})
		view := l.View("empty")
		assertSize(t, view, size[0], size[1])
		if !strings.Contains(ansi.Strip(view), "> node-0999") {
			t.Fatal("last selected row is not visible")
		}
		l.Update(tea.KeyMsg{Type: tea.KeyHome})
		view = l.View("empty")
		if !strings.Contains(ansi.Strip(view), "> node-0000") {
			t.Fatal("Home did not reveal the first row")
		}
		assertSize(t, view, size[0], size[1])
	}
	l.Cursor = 900
	l.SetItems([]string{"kept", "other"}, []string{"900", "800"})
	if l.SelectedKey() != "900" {
		t.Fatal("refresh changed object identity")
	}
}

func TestFormPreservesLongURLAndTenPastedLinks(t *testing.T) {
	url := "https://example.invalid/sub?token=" + strings.Repeat("x", 661-len("https://example.invalid/sub?token="))
	f := NewForm("订阅", []string{"URL"}, []string{"url"}, []string{"订阅地址"})
	f.Width, f.Height = 80, 21
	f.Handle(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(url), Paste: true})
	if got := f.ValueByKey("url"); got != url {
		t.Fatalf("URL truncated: got %d, want %d", len(got), len(url))
	}
	if action, _ := f.Handle(tea.KeyMsg{Type: tea.KeyCtrlS}); action != "save" {
		t.Fatal("valid long URL cannot be saved")
	}
	links := make([]string, 10)
	for i := range links {
		links[i] = fmt.Sprintf("trojan://fixture-password@node.example.invalid:443#node-%02d", i)
	}
	content := strings.Join(links, "\n")
	f = NewForm("导入", []string{"分享链接"}, []string{"links"}, []string{""})
	f.Width, f.Height = 80, 21
	f.View()
	f.Handle(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(content), Paste: true})
	if got := f.ValueByKey("links"); got != content {
		t.Fatalf("multiline paste lost content: %q", got)
	}
	assertSize(t, f.View(), 80, 21)
}

func TestFormSaveCancelMaskAndExplicitLimit(t *testing.T) {
	f := NewForm("编辑", []string{"名称", "密钥"}, []string{"name", "clash_api_secret"}, []string{"名称", "API 凭据"})
	f.Width, f.Height = 80, 21
	f.SetValueByKey("clash_api_secret", "fixture-secret")
	f.Begin()
	f.Handle(keyRunes("q1?"))
	if action, _ := f.Handle(tea.KeyMsg{Type: tea.KeyEnter}); action != "" {
		t.Fatal("Enter in a field submitted")
	}
	if strings.Contains(f.View(), "fixture-secret") {
		t.Fatal("secret is visible by default")
	}
	f.Handle(tea.KeyMsg{Type: tea.KeyCtrlR})
	if !strings.Contains(f.View(), "fixture-secret") {
		t.Fatal("explicit reveal is unavailable")
	}
	f.Handle(tea.KeyMsg{Type: tea.KeyEsc})
	if action, _ := f.Handle(tea.KeyMsg{Type: tea.KeyEnter}); action != "" {
		t.Fatal("default discard button must keep editing")
	}
	if !f.Dirty() {
		t.Fatal("cancel confirmation lost edits")
	}
	f.Handle(tea.KeyMsg{Type: tea.KeyEsc})
	if action, _ := f.Handle(keyRunes("y")); action != "cancel" {
		t.Fatal("explicit discard did not cancel")
	}
	f.Reset()
	f.SetValueByKey("name", strings.Repeat("名", 1025))
	if action, _ := f.Handle(tea.KeyMsg{Type: tea.KeyCtrlS}); action == "save" {
		t.Fatal("oversize name was saved")
	}
	if len([]rune(f.ValueByKey("name"))) != 1025 || !strings.Contains(f.View(), "输入已保留") {
		t.Fatal("limit failure must retain input and explain it")
	}
}

func TestConfirmOwnsInputAndDefaultsToCancel(t *testing.T) {
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeyEsc}, keyRunes("q")} {
		c := NewConfirm("delete", "删除对象？")
		consumed, cmd := c.Update(key)
		if !consumed || cmd == nil || cmd().(ConfirmMsg).Confirmed {
			t.Fatalf("%s did not cancel", key.String())
		}
	}
	c := NewConfirm("delete", strings.Repeat("长名称与实际影响。", 100))
	if consumed, cmd := c.Update(keyRunes("1")); !consumed || cmd != nil || !c.Active {
		t.Fatal("confirmation leaked a global key")
	}
	c.Update(tea.KeyMsg{Type: tea.KeyTab})
	_, cmd := c.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !cmd().(ConfirmMsg).Confirmed {
		t.Fatal("focused confirm did not execute")
	}
	c.Width, c.Height = 60, 15
	assertSize(t, c.View(), 60, 15)
}

func TestLongFormKeepsFocusedFieldAndErrorVisible(t *testing.T) {
	var labels, keys, hints []string
	for i := range 30 {
		labels = append(labels, fmt.Sprintf("字段%02d", i))
		keys = append(keys, fmt.Sprint(i))
		hints = append(hints, "填写后仍可见的说明")
	}
	f := NewForm("长表单", labels, keys, hints)
	f.Width, f.Height = 80, 21
	f.FocusKey("29")
	f.Error = "输入错误，请修正"
	view := ansi.Strip(f.View())
	if !strings.Contains(view, "> 字段29") || !strings.Contains(view, "输入错误") || !strings.Contains(view, "[保存]") {
		t.Fatal("focus, error or explicit save button was cropped")
	}
	assertSize(t, view, 80, 21)
}

func TestMultiReferenceSelectionCancelAndHiddenFieldRetention(t *testing.T) {
	f := NewForm("引用", []string{"规则集", "高级值"}, []string{"value", "extra"}, []string{"", ""})
	f.Width, f.Height = 80, 21
	f.SetChoices("value", []Option{{"first", "第一条"}, {"second", "第二条"}, {"legacy", "旧标识 · 第三条"}})
	f.Field("value").Multi = true
	f.Field("extra").Advanced = true
	f.SetValueByKey("value", "first,legacy")
	f.SetValueByKey("extra", "保留的高级配置")
	f.Begin()
	f.Handle(tea.KeyMsg{Type: tea.KeyEnter})
	f.Handle(tea.KeyMsg{Type: tea.KeyDown})
	f.Handle(tea.KeyMsg{Type: tea.KeySpace})
	f.Handle(tea.KeyMsg{Type: tea.KeyEsc})
	if f.ValueByKey("value") != "first,legacy" || f.Dirty() {
		t.Fatal("cancelled reference selection changed the form")
	}
	f.Handle(tea.KeyMsg{Type: tea.KeyEnter})
	f.Handle(tea.KeyMsg{Type: tea.KeyDown})
	f.Handle(tea.KeyMsg{Type: tea.KeySpace})
	f.Handle(tea.KeyMsg{Type: tea.KeyEnter})
	if f.ValueByKey("value") != "first,legacy,second" {
		t.Fatalf("selection lost an existing reference: %s", f.ValueByKey("value"))
	}
	f.Handle(tea.KeyMsg{Type: tea.KeyCtrlG})
	f.Handle(tea.KeyMsg{Type: tea.KeyCtrlG})
	if action, _ := f.Handle(tea.KeyMsg{Type: tea.KeyCtrlS}); action != "save" || f.ValueByKey("extra") != "保留的高级配置" {
		t.Fatal("collapsing advanced fields discarded their values")
	}
}

// TestListSkipsUnselectableRows 分组标题这类装饰行不可选中：上下移动、翻页、Home/End
// 与按 key 恢复选中都不会把光标停在标题上，标题也不套用表格列宽。
func TestListSkipsUnselectableRows(t *testing.T) {
	l := SimpleList{}
	l.SetTableSelectable(
		[]Column{{Title: "名称", Width: 10}, {Title: "状态", Width: 6}},
		[][]string{
			{"[层 A]"},
			{"a1", "启用"},
			{"[层 B]"},
			{"b1", "停用"},
			{"b2", "启用"},
		},
		[]string{"", "a1", "", "b1", "b2"},
		[]bool{false, true, false, true, true},
	)
	l.Height, l.Width = 10, 40

	if l.IsSelectable(0) || !l.IsSelectable(1) {
		t.Fatal("Selectable 映射错误：标题行应不可选、组行应可选")
	}
	// 初始光标不应停在标题行
	if !l.IsSelectable(l.Cursor) {
		t.Fatalf("初始光标落在不可选行: %d", l.Cursor)
	}
	// 向下跳过层 B 标题
	l.Update(tea.KeyMsg{Type: tea.KeyDown})
	if key := l.SelectedKey(); key != "b1" {
		t.Fatalf("向下移动后选中 = %q, 期望 b1（跳过标题）", key)
	}
	// 向上回到 a1
	l.Update(tea.KeyMsg{Type: tea.KeyUp})
	if key := l.SelectedKey(); key != "a1" {
		t.Fatalf("向上移动后选中 = %q, 期望 a1", key)
	}
	// Home / End 落到首尾可选行
	l.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if key := l.SelectedKey(); key != "b2" {
		t.Fatalf("End 后选中 = %q, 期望 b2", key)
	}
	l.Update(tea.KeyMsg{Type: tea.KeyHome})
	if key := l.SelectedKey(); key != "a1" {
		t.Fatalf("Home 后选中 = %q, 期望 a1", key)
	}
	// 按 key 恢复到标题行时退到其后最近的可选行
	l.SelectKey("")
	if key := l.SelectedKey(); key != "a1" {
		t.Fatalf("SelectKey(标题) 后选中 = %q, 期望 a1", key)
	}
	// 标题整行渲染（不按 10 列截断），且高度不超界
	view := l.View("空")
	if !strings.Contains(ansi.Strip(view), "[层 A]") || !strings.Contains(ansi.Strip(view), "[层 B]") {
		t.Fatalf("层标题未完整渲染:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if ansi.StringWidth(line) > l.Width {
			t.Fatalf("列表行溢出宽度: %q", line)
		}
	}
}
