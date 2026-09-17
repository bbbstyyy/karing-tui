package pages

import (
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/tui/components"
)

// C12 的两个核心性质：
//  1. View() 不再重建表格——连续两次渲染之间列表状态必须逐字节不变
//     （此前 Groups/DNS/Rules 的 View 每帧 SetTable，561 行
//     「改写渲染状态的 View」与重复建行都源于此）；
//  2. 数据经 reload 之类的 Update 路径变化后，不经过 View 表格即为新内容，
//     且列布局缓存（components 的 tableColumnsFor）在 SetTable / 宽度变化时
//     正确失效。

func snapshotList(l *components.SimpleList) string {
	var b strings.Builder
	for i, item := range l.Items {
		b.WriteString(item)
		b.WriteByte('\x1f')
		var key string
		if i < len(l.Keys) {
			key = l.Keys[i]
		}
		b.WriteString(key)
		b.WriteByte('\x1f')
	}
	for _, row := range l.Rows {
		for _, cell := range row {
			b.WriteString(cell)
			b.WriteByte('\x1e')
		}
		b.WriteByte('\x1d')
	}
	return b.String()
}

func assertViewPure(t *testing.T, name string, render func() string, l *components.SimpleList) {
	t.Helper()
	render() // 预热（首帧可能带布局副作用）
	before := snapshotList(l)
	first := render()
	second := render()
	if snapshotList(l) != before {
		t.Errorf("%s: View() 改写了列表状态（应只在 Update 路径重建表格）", name)
	}
	if first != second {
		t.Errorf("%s: 连续两次渲染输出不一致", name)
	}
}

func TestDNSViewDoesNotRebuildTable(t *testing.T) {
	app := pageFixture(t)
	d := NewDNS(app)
	d.SetSize(100, 24)
	d.Update(ActivateMsg{})
	assertViewPure(t, "DNS", d.View, &d.list)
}

func TestGroupsViewDoesNotRebuildTable(t *testing.T) {
	app := pageFixture(t)
	g := NewGroups(app)
	g.SetSize(100, 24)
	g.Update(ActivateMsg{})
	assertViewPure(t, "Groups", g.View, &g.list)

	// 进入详情模式：同一性质在成员表上成立。
	g.handleKey(chars("enter"))
	if g.mode != groupsDetail {
		t.Fatal("enter 应进入详情模式")
	}
	assertViewPure(t, "GroupsDetail", g.View, &g.list)
}

func TestRulesViewDoesNotRebuildTable(t *testing.T) {
	app := pageFixture(t)
	r := NewRules(app)
	r.SetSize(100, 24)
	r.Update(ActivateMsg{})
	assertViewPure(t, "Rules", r.View, &r.list)

	// 规则子表同理。
	r.handleKey(chars("enter"))
	if r.mode != rulesGroupRl {
		t.Fatal("enter 应进入规则列表模式")
	}
	assertViewPure(t, "RulesGroupRl", r.View, &r.list)
}

// TestTableRebuiltOnDataChangeWithoutView 钉住事件驱动的另一半：
// 数据变了、没渲染，表格也必须是新的——否则「移出 View」会把陈旧行带回来。
func TestTableRebuiltOnDataChangeWithoutView(t *testing.T) {
	app := pageFixture(t)
	d := NewDNS(app)
	d.SetSize(100, 24)
	d.Update(ActivateMsg{})
	rowsBefore := len(d.list.Rows)

	if _, err := app.DNS.AddServer("new-tag", "udp", "8.8.8.8:53", "", ""); err != nil {
		t.Fatal(err)
	}
	d.Update(ActivateMsg{}) // ActivateMsg → reload → table，全程无 View
	if len(d.list.Rows) != rowsBefore+1 {
		t.Fatalf("数据变化后未重建表格：rows = %d, want %d", len(d.list.Rows), rowsBefore+1)
	}
	found := false
	for _, row := range d.list.Rows {
		if strings.Contains(row[0], "new-tag") {
			found = true
		}
	}
	if !found {
		t.Error("新服务器应出现在表格行中")
	}
}

// TestTableColumnsCacheInvalidation 直测 components 的列布局缓存：
// 同宽度复用，SetTable / 宽度变化即失效。
func TestTableColumnsCacheInvalidation(t *testing.T) {
	var l components.SimpleList
	l.Height = 10
	viewAt := func(w int) string {
		l.Width = w
		return l.View("")
	}
	l.SetTable(
		[]components.Column{
			{Title: "A", Width: 10, Priority: 0},
			{Title: "B", Width: 10, Priority: 1},
		},
		[][]string{{"a1", "b1"}, {"a2", "b2"}},
		[]string{"1", "2"},
	)
	wide := viewAt(60)
	if !strings.Contains(wide, "B") {
		t.Error("宽视图应含第二列标题")
	}
	if viewAt(60) != wide {
		t.Error("同宽度重复渲染应逐字节一致（列缓存生效）")
	}

	// 宽度变小：渲染随之变化（缓存按宽度失效）。
	if narrow := viewAt(24); narrow == wide {
		t.Error("宽度变化后渲染应随之变化（缓存按宽度失效）")
	}

	// 同宽度换表：SetTable 必须使缓存失效。
	l.SetTable(
		[]components.Column{{Title: "X", Width: 10, Priority: 0}},
		[][]string{{"x1"}, {"x2"}},
		[]string{"1", "2"},
	)
	next := viewAt(60)
	if !strings.Contains(next, "X") || strings.Contains(next, "A ") {
		t.Error("SetTable 后列布局缓存应失效并渲染新列")
	}
}
