package pages

import (
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// resetRoutingGroups 清空夹具库的默认方案，让用例从确定的多层场景开始。
func resetRoutingGroups(t *testing.T, p *Rules) {
	t.Helper()
	groups, err := p.app.DB.ListRoutingGroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if err := p.app.Rout.DeleteGroup(g.ID); err != nil {
			t.Fatalf("清理分流组 %q: %v", g.Name, err)
		}
	}
}

// seedRulesFixture 在夹具库里造出 6.5/7.7 要求的「多层 + 多组」场景。
// final 的目标用夹具库自带的 Manual 代理组（EnsureDefaultGroups 创建）。
func seedRulesFixture(t *testing.T, p *Rules) []*config.RoutingGroup {
	t.Helper()
	resetRoutingGroups(t, p)
	seed := []struct {
		name   string
		kind   string
		target string
		rule   config.Rule
	}{
		{"手工规则 A", config.KindCustom, "Auto", config.Rule{Type: "domain_suffix", Value: "a.com", Enabled: true}},
		{"手工规则 B", config.KindCustom, "DIRECT", config.Rule{Type: "domain_suffix", Value: "b.com", Enabled: true}},
		{"苹果服务", config.KindGeosite, "DIRECT", config.Rule{Type: "rule_set", Value: "geosite:apple", Enabled: true}},
		{"Google", config.KindGeoIP, "Auto", config.Rule{Type: "rule_set", Value: "geoip:google", Enabled: true}},
		{"国内直连", config.KindACL, "DIRECT", config.Rule{Type: "rule_set", Value: "acl:ChinaIp", Enabled: true}},
	}
	var groups []*config.RoutingGroup
	for _, s := range seed {
		g, err := p.app.Rout.CreateGroupIn(s.name, s.target, s.kind, []config.Rule{s.rule})
		if err != nil {
			t.Fatalf("建组 %q: %v", s.name, err)
		}
		groups = append(groups, g)
	}
	final, err := p.app.Rout.CreateGroupIn("Final", "Manual", config.KindFinal,
		[]config.Rule{{Type: "final", Enabled: true}})
	if err != nil {
		t.Fatalf("建 final 组: %v", err)
	}
	groups = append(groups, final)
	return groups
}

// TestRulesLayeredRendering 6.5：Rules 页按层分组渲染——层标题按层序出现、final 恒置底、
// 空层也保留标题，层标题带「组数 / 启用数」。
func TestRulesLayeredRendering(t *testing.T) {
	app := pageFixture(t)
	p := NewRules(app)
	p.SetSize(100, 30)
	p.reload()
	seedRulesFixture(t, p)
	p.reload()

	view := ansi.Strip(p.View())
	for _, want := range []string{
		"自定义分流组 · 2 组 / 启用 2",
		"GeoSite · 1 组 / 启用 1",
		"GeoIP · 1 组 / 启用 1",
		"ACL · 1 组 / 启用 1",
		"final · 1 组 / 启用 1",
		"兜底 · 不可选「不处理」",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("按层渲染缺少 %q\n%s", want, view)
		}
	}
	// 层序：custom 在 geosite 之前，final 在最后
	indexOf := func(s string) int { return strings.Index(view, s) }
	if indexOf("自定义分流组") > indexOf("GeoSite") ||
		indexOf("GeoSite") > indexOf("GeoIP") ||
		indexOf("GeoIP") > indexOf("ACL") ||
		indexOf("ACL") > indexOf("final") {
		t.Errorf("层序不正确:\n%s", view)
	}
	// 层内序号从 1 开始
	if !strings.Contains(view, "1. 手工规则 A") || !strings.Contains(view, "2. 手工规则 B") {
		t.Errorf("层内序号缺失:\n%s", view)
	}
}

// TestRulesLayeredRenderingFitsTerminal 7.7 渲染回归：默认方案（cn 预置 27 组 + Final）
// 在 80×24 与 100×30 下按层渲染都不得溢出宽度/高度，底栏必须留在最后一行
// （27 组 + 5 个层标题让行数远超视窗，最容易把固定底栏挤出去）。
func TestRulesLayeredRenderingFitsTerminal(t *testing.T) {
	app := pageFixture(t)
	p := NewRules(app)
	p.reload()

	groups, err := app.DB.ListRoutingGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 27+1 {
		t.Fatalf("默认方案分流组数 = %d, 期望 27 + 1 个兜底组", len(groups))
	}
	assertRulesFramesFit(t, p)
}

// TestRulesLayerRenderingFitsWithEmojiNames emoji 组名 + 5 个层标题：
// 覆盖 5.11 的按码点保守计宽，确认底栏仍不被挤出。
func TestRulesLayerRenderingFitsWithEmojiNames(t *testing.T) {
	app := pageFixture(t)
	p := NewRules(app)
	p.SetSize(100, 30)
	p.reload()
	resetRoutingGroups(t, p)
	emoji := []struct{ name, kind string }{
		{"🛑 广告拦截", config.KindCustom},
		{"🍎 苹果服务", config.KindGeosite},
		{"🌏 Google", config.KindGeoIP},
		{"🎯 国内直连", config.KindACL},
		{"🌏 国外穿墙", config.KindCustom},
	}
	for _, e := range emoji {
		if _, err := app.Rout.CreateGroupIn(e.name, "DIRECT", e.kind,
			[]config.Rule{{Type: "rule_set", Value: "geosite:cn", Enabled: true}}); err != nil {
			t.Fatalf("建组 %q: %v", e.name, err)
		}
	}
	p.reload()
	assertRulesFramesFit(t, p)
}

// assertRulesFramesFit 按 5.11 的四尺寸口径检查 Rules 页帧：行数严格等于终端高度、
// 不溢出宽度、最后一行是固定底栏。
func assertRulesFramesFit(t *testing.T, p *Rules) {
	t.Helper()
	for _, size := range [][2]int{{80, 24}, {100, 30}} {
		p.SetSize(size[0], size[1])
		view := p.View()
		lines := strings.Split(view, "\n")
		if len(lines) != size[1] {
			t.Errorf("%dx%d: 行数 = %d, 期望 %d", size[0], size[1], len(lines), size[1])
		}
		for i, line := range lines {
			if ansi.StringWidth(line) > size[0] {
				t.Errorf("%dx%d: 第 %d 行溢出宽度 %q", size[0], size[1], i, line)
			}
		}
		if last := ansi.Strip(lines[len(lines)-1]); !strings.Contains(last, "帮助") {
			t.Errorf("%dx%d: 底栏被挤出: %q", size[0], size[1], last)
		}
	}
}

// TestRulesCursorSkipsLayerHeadings 层标题不可选中：上下移动与 Enter 都不会把它当成组。
func TestRulesCursorSkipsLayerHeadings(t *testing.T) {
	app := pageFixture(t)
	p := NewRules(app)
	p.SetSize(100, 30)
	p.reload()
	seedRulesFixture(t, p)
	p.reload()

	// 逐行向下走一遍，光标始终落在可选中行上
	for i := 0; i < 40; i++ {
		if !p.list.IsSelectable(p.list.Cursor) {
			t.Fatalf("光标落在不可选行: %d", p.list.Cursor)
		}
		if _, ok := p.selectedGroup(); !ok {
			t.Fatalf("光标 %d 处取不到分流组", p.list.Cursor)
		}
		p.Update(chars("j"))
	}
	// 回到首行
	p.Update(tea.KeyMsg{Type: tea.KeyHome})
	if !p.list.IsSelectable(p.list.Cursor) {
		t.Fatal("Home 后光标落在不可选行")
	}
	// Enter 打开组内规则而不是 panic
	p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if p.mode != rulesGroupRl || p.cur == nil {
		t.Fatalf("Enter 未进入组内规则: mode=%v cur=%v", p.mode, p.cur)
	}
}

// TestRulesMoveWithinLayerAndAcrossLayers J/K 层内移动、m 显式跨层移动（含后果提示）。
func TestRulesMoveWithinLayerAndAcrossLayers(t *testing.T) {
	app := pageFixture(t)
	p := NewRules(app)
	p.SetSize(100, 30)
	p.reload()
	seedRulesFixture(t, p)
	p.reload()

	order := func() string {
		groups, err := app.DB.ListRoutingGroups()
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, g := range groups {
			names = append(names, g.Name)
		}
		return strings.Join(names, ",")
	}
	// 初始：custom 两组按创建顺序
	if got := order(); !strings.HasPrefix(got, "手工规则 A,手工规则 B") {
		t.Fatalf("初始顺序 = %q", got)
	}

	// 光标在首行（手工规则 A），J 下移
	p.Update(chars("J"))
	if got := order(); !strings.HasPrefix(got, "手工规则 B,手工规则 A") {
		t.Fatalf("层内下移后顺序 = %q", got)
	}
	// K 上移还原
	p.Update(chars("K"))
	if got := order(); !strings.HasPrefix(got, "手工规则 A,手工规则 B") {
		t.Fatalf("层内上移后顺序 = %q", got)
	}

	// m 打开层选择器
	p.Update(chars("m"))
	if p.mode != rulesMove {
		t.Fatalf("m 未打开层选择器: mode=%v", p.mode)
	}
	moveView := ansi.Strip(p.View())
	if !strings.Contains(moveView, "移动到其他层") || !strings.Contains(moveView, "层序即优先级") {
		t.Errorf("层选择器缺少层序提示:\n%s", moveView)
	}
	// 选到 ACL 层（层序列 custom,geosite,geoip,acl,final → index 3）
	p.moveList.Cursor = 3
	p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if p.mode != rulesConfirm {
		t.Fatalf("Enter 应弹出后果确认: mode=%v", p.mode)
	}
	confirmView := ansi.Strip(p.confirm.View())
	if !strings.Contains(confirmView, "优先级归属会随之改变") {
		t.Errorf("确认文案应说明优先级影响:\n%s", confirmView)
	}
	// 确认默认停在「取消」，按 y 直接确认（确认消息要回灌才生效）
	acceptConfirm(t, p)
	if p.mode != rulesGroups {
		t.Fatalf("确认后应回到分流组列表: mode=%v", p.mode)
	}
	groups, _ := app.DB.ListRoutingGroups()
	aclOrder := []string{}
	for _, g := range groups {
		if g.Layer() == config.KindACL {
			aclOrder = append(aclOrder, g.Name)
		}
	}
	if len(aclOrder) != 2 || aclOrder[len(aclOrder)-1] != "手工规则 A" {
		t.Errorf("跨层移动后 ACL 层 = %v, 期望 手工规则 A 追加到层尾", aclOrder)
	}
	// 跨层后 custom 层只剩 B
	if got := order(); strings.Contains(got, "手工规则 A,手工规则 B") {
		t.Errorf("跨层后顺序未变: %q", got)
	}
}

// TestRulesMoveIntoFinalLayerRejected 普通组不能靠跨层移动变成 final 组，错误要落在状态栏。
func TestRulesMoveIntoFinalLayerRejected(t *testing.T) {
	app := pageFixture(t)
	p := NewRules(app)
	p.SetSize(100, 30)
	p.reload()
	seedRulesFixture(t, p)
	p.reload()

	p.list.Cursor = 0
	p.Update(tea.KeyMsg{Type: tea.KeyHome}) // 落到第一个可选中的组
	p.Update(chars("m"))
	if p.mode != rulesMove {
		t.Fatal("m 未打开层选择器")
	}
	// 找到 final 层的行
	finalRow := -1
	for i, kind := range p.moveKinds {
		if kind == config.KindFinal {
			finalRow = i
		}
	}
	if finalRow < 0 {
		t.Fatal("层选择器缺少 final 层")
	}
	p.moveList.Cursor = finalRow
	p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	acceptConfirm(t, p)
	if p.err == nil {
		t.Fatal("把普通规则组移入 final 层应被拒绝")
	}
	if !strings.Contains(p.err.Error(), "final") {
		t.Errorf("错误信息应说明 final 层要求: %v", p.err)
	}
}

// TestRulesEnabledOnlyFilter 7.6：仅显示启用过滤，停用组不再淹没列表，层标题仍显示总数。
func TestRulesEnabledOnlyFilter(t *testing.T) {
	app := pageFixture(t)
	p := NewRules(app)
	p.SetSize(100, 30)
	p.reload()
	groups := seedRulesFixture(t, p)
	// 停用第二个手工组与 geoip 组
	for _, g := range groups {
		if g.Name == "手工规则 B" || g.Name == "Google" {
			if err := app.Rout.SetEnabled(g.ID, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	p.reload()
	view := ansi.Strip(p.View())
	if !strings.Contains(view, "手工规则 B") {
		t.Fatal("默认应显示停用组")
	}

	p.Update(chars("f"))
	view = ansi.Strip(p.View())
	if strings.Contains(view, "手工规则 B") {
		t.Error("过滤后不应显示停用的手工规则 B")
	}
	if !strings.Contains(view, "自定义分流组 · 2 组 / 启用 1") {
		t.Errorf("层标题应同时给出组数与启用数:\n%s", view)
	}
	p.Update(chars("f"))
	if !strings.Contains(ansi.Strip(p.View()), "手工规则 B") {
		t.Error("再次按 f 应恢复显示全部")
	}
}

// TestRulesApplyPresetEntry 7.4/7.6：Rules 页的预置入口（P）走确认 → 后台任务，
// 逐条结果按任务回执展示；对已装方案重复导入时全部跳过（merge 语义）。
func TestRulesApplyPresetEntry(t *testing.T) {
	app := pageFixture(t)
	p := NewRules(app)
	p.SetSize(100, 30)
	p.reload() // 夹具库已装入默认方案（27 组 + 兜底组）

	p.Update(chars("P"))
	if p.mode != rulesConfirm {
		t.Fatalf("P 应弹出导入确认: mode=%v", p.mode)
	}
	prompt := ansi.Strip(p.confirm.View())
	for _, want := range []string{"地区预置", "custom"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("确认文案缺少 %q:\n%s", want, prompt)
		}
	}
	// 确认消息与任务命令都要在测试里执行/回灌（与真实事件循环一致）
	_, confirmCmd := p.Update(chars("y"))
	if confirmCmd == nil {
		t.Fatal("确认未产生命令")
	}
	confirmMsg := confirmCmd()
	if _, ok := confirmMsg.(components.ConfirmMsg); !ok {
		t.Fatalf("确认命令应返回 ConfirmMsg, 实得 %T", confirmMsg)
	}
	_, taskCmd := p.Update(confirmMsg)
	if taskCmd == nil {
		t.Fatal("确认后应启动导入任务")
	}
	if _, label := p.TaskStatus(); !strings.Contains(label, "地区预置导入") {
		t.Fatalf("任务应处于进行中: %q", label)
	}
	done := taskCmd()
	if _, ok := done.(actionDoneMsg); !ok {
		t.Fatalf("任务命令应返回 actionDoneMsg, 实得 %T", done)
	}
	p.Update(done)
	if !strings.Contains(p.status, "新增 0") || !strings.Contains(p.status, "跳过") {
		t.Errorf("对已装方案重复导入应全部跳过: %q", p.status)
	}
	groups, _ := app.DB.ListRoutingGroups()
	if len(groups) != 28 {
		t.Errorf("导入后分流组数 = %d, 期望 28", len(groups))
	}
}

// TestSettingsRoutingInfoReadOnly 设置页的分流说明项只读：Enter 打开详情而不是空表单。
func TestSettingsRoutingInfoReadOnly(t *testing.T) {
	app := pageFixture(t)
	s := NewSettings(app)
	s.SetSize(100, 30)
	s.section = 2 // 分流行为
	s.reloadSettings()
	s.list.SelectKey("routing_final")
	if settingEditable("routing_final") {
		t.Fatal("routing_final 应为只读说明项")
	}
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.editing {
		t.Fatal("只读说明项不应进入编辑表单")
	}
	if !s.detailActive || !strings.Contains(s.detailText, "Manual") {
		t.Fatalf("Enter 应打开说明详情: %q", s.detailText)
	}
}
