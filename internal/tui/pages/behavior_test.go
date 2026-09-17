package pages

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/catalog"
	"github.com/bbbstyyy/karing-tui/internal/clashapi"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func pageFixture(t *testing.T) *application.App {
	t.Helper()
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	app, err := application.New(paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	return app
}

func chars(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
func acceptConfirm(t *testing.T, p Page) {
	t.Helper()
	_, cmd := p.Update(chars("y"))
	if cmd == nil {
		t.Fatal("missing confirmation result")
	}
	p.Update(cmd())
}

func TestFormErrorsStayVisibleWhileEditing(t *testing.T) {
	app := pageFixture(t)
	p := NewProfiles(app)
	p.SetSize(80, 21)
	p.openSubForm("add-sub")
	p.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if p.mode != profilesForm || p.form.Error == "" || p.form.CurrentKey() != "name" || !strings.Contains(p.View(), "订阅名称") {
		t.Fatal("subscription form hid its validation error")
	}
	g := NewGroups(app)
	g.SetSize(80, 21)
	g.openForm("add-group")
	g.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if g.mode != groupsForm || g.form.Error == "" || g.form.CurrentKey() != "name" || !strings.Contains(g.View(), "代理组名称") {
		t.Fatal("group form hid its validation error")
	}
	d := NewDNS(app)
	d.SetSize(80, 21)
	d.reload()
	d.openForm("add-server")
	d.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if d.mode != dnsForm || d.form.Error == "" || d.form.CurrentKey() != "tag" || !strings.Contains(d.View(), "错误") {
		t.Fatal("DNS form hid its validation error")
	}
	d.openOptions()
	d.form.SetValueByKey("fakeip", "true")
	d.form.SetValueByKey("range", "bad-range")
	d.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if d.mode != dnsOptions || d.err == nil || !strings.Contains(d.View(), "网段非法") {
		t.Fatal("DNS options hid its validation error")
	}
}

func TestRoutingTargetsPreserveNamesAndDefaultGroupCanSave(t *testing.T) {
	app := pageFixture(t)
	if _, err := app.Proxy.CreateGroup("MiXeD组", "select", "", 0, nil); err != nil {
		t.Fatal(err)
	}
	r := NewRules(app)
	r.SetSize(80, 21)
	r.reload()
	for _, target := range []string{"Auto", "Manual", "MiXeD组", "direct", "bLoCk"} {
		r.openForm("add-rg")
		r.form.SetValueByKey("name", "测试 "+target)
		r.form.SetValueByKey("target", target)
		r.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
		if r.err != nil {
			t.Fatalf("saving target %s: %v", target, r.err)
		}
		want := target
		if strings.EqualFold(target, "DIRECT") || strings.EqualFold(target, "BLOCK") {
			want = strings.ToUpper(target)
		}
		found := false
		for _, g := range r.groups {
			if g.Name == "测试 "+target {
				found = true
				if g.Target != want {
					t.Fatalf("target rewritten to %q", g.Target)
				}
			}
		}
		if !found {
			t.Fatal("routing group not saved")
		}
	}
	for _, g := range r.groups {
		if g.Name == "🌏 Google" {
			r.openFormEditGroup(g)
			r.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
			if r.err != nil {
				t.Fatalf("default Google group edit: %v", r.err)
			}
			return
		}
	}
	t.Fatal("fixture lacked default Google group")
}

func TestLongSubscriptionURLAndMixedBatchImport(t *testing.T) {
	app := pageFixture(t)
	p := NewProfiles(app)
	p.SetSize(80, 21)
	url := "https://example.invalid/" + strings.Repeat("a", 661-len("https://example.invalid/"))
	p.openSubForm("add-sub")
	p.form.SetValueByKey("name", "long")
	p.form.SetValueByKey("url", url)
	p.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if p.err != nil {
		t.Fatal(p.err)
	}
	subs, _ := app.DB.ListSubscriptions()
	if len(subs) != 1 || subs[0].URL != url {
		t.Fatal("URL did not survive form-to-database round trip")
	}
	var links []string
	for i := range 10 {
		links = append(links, fmt.Sprintf("trojan://fixture-password@node.example.invalid:443#node-%02d", i))
	}
	links = append(links, "not-a-link")
	p.openImportForm()
	p.form.SetValueByKey("links", strings.Join(links, "\n"))
	p.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	nodes, err := app.DB.ListNodes(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 10 || len(p.results) != 11 || p.err == nil {
		t.Fatalf("partial import not accounted for: nodes=%d results=%d err=%v", len(nodes), len(p.results), p.err)
	}
	if p.results[10].State != "失败" {
		t.Fatal("invalid link was silently skipped")
	}
}

func TestBatchUpdateSkipsDisabledReportsEveryResultAndRetriesOnlyFailures(t *testing.T) {
	app := pageFixture(t)
	var mu sync.Mutex
	counts := map[string]int{}
	var recoverBad atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		mu.Unlock()
		if r.URL.Path == "/bad" && !recoverBad.Load() {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "trojan://fixture-password@node.example.invalid:443#fixture")
	}))
	defer server.Close()
	var disabledID int64
	for _, name := range []string{"good", "bad", "disabled"} {
		sub, err := app.Subs.Add(context.Background(), name, server.URL+"/"+name, "")
		if err != nil {
			t.Fatal(err)
		}
		if name == "disabled" {
			disabledID = sub.ID
			if err := app.Subs.SetEnabled(sub.ID, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	p := NewProfiles(app)
	p.reload()
	_, cmd := p.Update(chars("U"))
	if cmd == nil {
		t.Fatal("batch did not start")
	}
	if _, duplicate := p.Update(chars("U")); duplicate != nil {
		t.Fatal("duplicate update was allowed")
	}
	p.Update(cmd())
	if p.busy || p.err == nil || !strings.Contains(p.status, "成功 1 · 失败 1 · 跳过 1") {
		t.Fatalf("wrong batch result: %s, %v", p.status, p.err)
	}
	p.Update(chars("r"))
	if p.err == nil {
		t.Fatal("refresh erased an unacknowledged failure")
	}
	p.SetSize(80, 21)
	p.Update(ActivateMsg{})
	view := p.View()
	for _, count := range []string{"成功 1", "失败 1", "跳过 1"} {
		if !strings.Contains(view, count) {
			t.Fatalf("partial update summary hidden after returning: %s", view)
		}
	}
	disabled, _ := app.DB.GetSubscription(disabledID)
	if !disabled.LastUpdated.IsZero() {
		t.Fatal("disabled subscription was changed")
	}
	recoverBad.Store(true)
	_, cmd = p.Update(chars("f"))
	if cmd == nil {
		t.Fatal("failed subscription cannot be retried")
	}
	p.Update(cmd())
	mu.Lock()
	defer mu.Unlock()
	if counts["/good"] != 1 || counts["/bad"] != 2 || counts["/disabled"] != 0 {
		t.Fatalf("wrong retry/download boundary: %v", counts)
	}
	if p.busy || p.err != nil || !strings.Contains(p.status, "成功 1 · 失败 0") {
		t.Fatalf("retry stayed busy/failed: %s %v", p.status, p.err)
	}
}

func TestStaleTaskCannotClearNewTask(t *testing.T) {
	app := pageFixture(t)
	p := NewProfiles(app)
	old := p.task("old", func() tea.Msg { return actionDoneMsg{Action: "old"} })()
	if duplicate := p.task("duplicate", func() tea.Msg { t.Fatal("duplicate task ran"); return nil }); duplicate != nil {
		t.Fatal("duplicate task was scheduled")
	}
	p.Update(old)
	newCmd := p.task("new", func() tea.Msg { return actionDoneMsg{Action: "new"} })
	p.busy = true
	p.Update(old)
	if !p.busy || !p.taskActive {
		t.Fatal("stale result cleared a new operation")
	}
	p.Update(newCmd())
	if p.busy || p.taskActive {
		t.Fatal("matching result did not release busy state")
	}
}

func TestOldDashboardSpeedTestCannotClearNewTask(t *testing.T) {
	d := NewDashboard(pageFixture(t))
	d.testBusy, d.testID = true, 2
	d.Update(testDoneMsg{id: 1, err: fmt.Errorf("old failure")})
	if !d.testBusy || d.lastErr != nil {
		t.Fatal("old speed test completion changed the current task")
	}
	active, label := d.TaskStatus()
	if !active || !strings.Contains(label, "测速") {
		t.Fatal("background speed test is missing from global feedback")
	}
}

func TestDynamicGroupSelectionAndReturnPosition(t *testing.T) {
	app := pageFixture(t)
	sub, err := app.Subs.Add(context.Background(), "synthetic", "http://example.invalid", "")
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*config.Node
	for i := range 1000 {
		nodes = append(nodes, &config.Node{Name: fmt.Sprintf("node-%04d 中文👩‍💻é", i), Protocol: "trojan", Server: "example.invalid", Port: 443, Enabled: true, Metadata: map[string]any{"password": "fixture"}})
	}
	if err := app.DB.ReplaceSubscriptionNodes(sub.ID, nodes, time.Now()); err != nil {
		t.Fatal(err)
	}
	g := NewGroups(app)
	g.SetSize(80, 21)
	g.reload()
	var id int64
	for _, group := range g.groups {
		if group.Name == "Manual" {
			id = group.ID
		}
	}
	g.list.SelectKey(strconv.FormatInt(id, 10))
	g.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if !g.detailActive || !strings.Contains(g.View(), "Manual") {
		t.Fatal("full group details are not reachable")
	}
	g.Update(tea.KeyMsg{Type: tea.KeyEsc})
	g.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(g.members) != 1000 {
		t.Fatalf("dynamic group has %d choices", len(g.members))
	}
	g.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if !strings.Contains(ansi.Strip(g.View()), "> node-0999") {
		t.Fatal("selected last member is invisible")
	}
	g.Update(tea.KeyMsg{Type: tea.KeyEnter})
	before, _ := app.DB.GetProxyGroup(id)
	if before.Selected != "" {
		t.Fatal("browsing changed the selected proxy")
	}
	g.Update(tea.KeyMsg{Type: tea.KeyEsc})
	chosen, _ := g.curMember()
	g.Update(chars(" "))
	after, _ := app.DB.GetProxyGroup(id)
	if after.Selected != chosen.key || !strings.Contains(g.status, "应用后生效") {
		t.Fatal("persistent selection lacks correct pending-apply feedback")
	}
	if err := app.Proxy.SetGroupSelected(id, "all"); err == nil {
		t.Fatal("dynamic placeholder is still selectable")
	}
	g.Update(tea.KeyMsg{Type: tea.KeyEsc})
	current, ok := g.selectedGroup()
	if !ok || current.ID != id {
		t.Fatal("return did not restore the original group")
	}
}

func TestDNSMoveEditDeleteAndReferenceErrorKeepIdentity(t *testing.T) {
	app := pageFixture(t)
	a, err := app.DNS.AddRule("domain", "a.example", "local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.DNS.AddRule("domain", "b.example", "local"); err != nil {
		t.Fatal(err)
	}
	d := NewDNS(app)
	d.SetSize(80, 21)
	d.reload()
	d.Update(chars("R"))
	d.list.SelectKey(fmt.Sprintf("rule:%d", a.ID))
	for _, key := range []string{"J", "J", "K", "K", "K", "J"} {
		d.Update(chars(key))
		rules, i, ok := d.selectedRule()
		if !ok || rules[i].ID != a.ID {
			t.Fatalf("%s changed the selected rule", key)
		}
	}
	d.Update(chars("e"))
	d.form.SetValueByKey("value", "a.changed")
	d.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	rules, i, ok := d.selectedRule()
	if !ok || rules[i].ID != a.ID || rules[i].Value != "a.changed" {
		t.Fatal("edit after move targeted another rule")
	}
	d.Update(chars("D"))
	_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyEnter})
	d.Update(cmd())
	rules, i, ok = d.selectedRule()
	if !ok || rules[i].ID != a.ID {
		t.Fatal("default delete cancellation changed selection")
	}
	d.Update(chars("d"))
	acceptConfirm(t, d)
	remaining, _ := app.DNS.ListRules()
	for _, r := range remaining {
		if r.ID == a.ID {
			t.Fatal("explicit delete did not remove the same rule")
		}
	}
	d.Update(tea.KeyMsg{Type: tea.KeyEsc})
	servers, _ := app.DNS.ListServers()
	for _, server := range servers {
		if server.Tag == "remote" {
			d.list.SelectKey(fmt.Sprintf("server:%d", server.ID))
		}
	}
	d.Update(chars("d"))
	acceptConfirm(t, d)
	if d.err == nil || !strings.Contains(d.View(), "final") {
		t.Fatal("reference protection error was erased")
	}
	d.Update(ActivateMsg{})
	if d.err == nil || !strings.Contains(d.View(), "final") {
		t.Fatal("activation erased delete failure")
	}
}

// C3：DNS 页的 View() 与选择器只能读内存缓存，渲染路径不得再查数据库。
//
// 手法是直接把库关掉：如果 View / resize 还会走 ListServers() / ListRules()，
// 就会拿到 "database is closed" 并改写 d.err，渲染结果随之变化。
func TestDNSViewDoesNotTouchDatabase(t *testing.T) {
	app := pageFixture(t)
	if _, err := app.DNS.AddRule("domain", "example.com", "remote"); err != nil {
		t.Fatal(err)
	}
	d := NewDNS(app)
	d.SetSize(100, 24)
	d.reload()
	// 切到「DNS 规则」子视图（此切换本身会 reload 一次，属 Update 阶段）。
	d.Update(chars("]"))
	before := d.View()
	if !strings.Contains(before, "example.com") || !strings.Contains(before, "remote") {
		t.Fatal("reload 没有把数据读进缓存")
	}

	if err := app.DB.Close(); err != nil {
		t.Fatal(err)
	}
	if got := d.View(); got != before {
		t.Fatal("View 访问了数据库：关闭后渲染结果发生变化")
	}
	// resize 与光标移动同样不得触发查库（只重建内存列表）。
	d.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	if d.err != nil {
		t.Fatalf("resize 触发了数据库访问：%v", d.err)
	}
	d.Update(tea.KeyMsg{Type: tea.KeyDown})
	if d.err != nil {
		t.Fatalf("光标移动触发了数据库访问：%v", d.err)
	}
	if len(d.servers) == 0 || len(d.rules) == 0 {
		t.Fatal("缓存被意外清空")
	}
}

// C5：规则表单里，值字段的连续输入不得重跑 configureRuleValue。
//
// 判定手法：先让 rule_set 的选项里带上一个只存在于数据库的自定义规则集，
// 然后关掉数据库再连打 20 个字符。若按键路径仍重建选项，那次 DB 查询会失败，
// 该条目就会从选项里消失。
func TestRuleFormKeyPressDoesNotReloadReferenceChoices(t *testing.T) {
	app := pageFixture(t)
	if err := app.DB.CreateRuleSet(&config.RuleSet{Name: "自定义集", Tag: "my-set", SourceType: "remote", Format: "srs", URL: "https://example.invalid/x.srs", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	r := NewRules(app)
	r.SetSize(120, 30)
	r.cur = &config.RoutingGroup{ID: 1, Name: "基准组", Target: "DIRECT"}
	r.openForm("add-rule")
	r.form.SetValueByKey("type", "rule_set")
	configureRuleValue(r.app, &r.form)
	if !hasOption(r.form.Field("value"), "my-set") {
		t.Fatal("rule_set 选项没有带上数据库里的自定义规则集")
	}

	// 反向核对：类型真的变化时必须重配（否则上面的断言会因为「永不重配」而假通过）。
	r.form.SetValueByKey("type", "domain")
	syncRuleValueOnTypeChange(r.app, &r.form, "rule_set")
	if r.form.Field("value").Choice {
		t.Fatal("类型切到 domain 后值字段仍按 rule_set 配置")
	}
	r.form.SetValueByKey("type", "rule_set")
	syncRuleValueOnTypeChange(r.app, &r.form, "domain")
	if !hasOption(r.form.Field("value"), "my-set") {
		t.Fatal("类型切回 rule_set 后没有重建选项")
	}

	if err := app.DB.Close(); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		r.Update(chars("x"))
	}
	if !hasOption(r.form.Field("value"), "my-set") {
		t.Fatal("按键路径重建了选项列表（触发了数据库查询）")
	}
}

func hasOption(field *components.FormField, value string) bool {
	if field == nil {
		return false
	}
	for _, opt := range field.Options {
		if opt.Value == value {
			return true
		}
	}
	return false
}

// C4：Profiles 的表格行必须在状态变化时准备好，渲染路径只做渲染。
//
// 手法：只改数据、不重建——View 的输出必须保持不变。若 View 里仍有
// table()/SetTable()/SetItems()，行就会被重新建模，"ghost" 会立刻出现。
func TestProfilesViewRendersPreparedRows(t *testing.T) {
	app := pageFixture(t)
	for _, name := range []string{"alpha", "beta"} {
		if err := app.Proxy.SaveManual(&config.Node{Name: name, Protocol: "http", Server: "example.invalid", Port: 80}); err != nil {
			t.Fatal(err)
		}
	}
	p := NewProfiles(app)
	p.SetSize(100, 24)
	p.mode = profilesNodes
	p.reloadNodes()
	before := p.View()
	if !strings.Contains(before, "alpha") {
		t.Fatal("reloadNodes 没有准备好表格行")
	}

	// 就地改名而非增删：不改变头部计数与当前预览，只有重建行才会体现出来。
	p.filtered[1].Name = "ghost"
	if got := p.View(); got != before {
		t.Fatal("View 在渲染路径里重建了表格行")
	}
	rows := len(p.list.Rows)

	// 光标移动只改视口，不得触发重建。
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	if len(p.list.Rows) != rows {
		t.Fatal("光标移动触发了行重建")
	}

	// 显式重建后才反映新数据。
	p.rebuildNodeTable()
	if !strings.Contains(p.View(), "ghost") {
		t.Fatal("rebuildNodeTable 没有生效")
	}
}

func TestNodeSearchIsIncrementalAndEscapeRestoresFilterAndPosition(t *testing.T) {
	app := pageFixture(t)
	p := NewProfiles(app)
	p.SetSize(80, 21)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := app.Proxy.SaveManual(&config.Node{Name: name, Protocol: "http", Server: "example.invalid", Port: 80}); err != nil {
			t.Fatal(err)
		}
	}
	p.reload()
	p.Update(chars("n"))
	p.list.Cursor = 1
	before := p.list.SelectedKey()
	p.Update(chars("/"))
	p.Update(chars("gamma"))
	if len(p.filtered) != 1 || !strings.Contains(p.View(), "gamma") {
		t.Fatal("search hid the results instead of filtering them")
	}
	p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if p.search != "" || len(p.filtered) != 3 || p.list.SelectedKey() != before {
		t.Fatal("search cancel did not restore original selection")
	}
}

func TestDNSOptionsReturnToTheSameSubviewAndObject(t *testing.T) {
	app := pageFixture(t)
	if _, err := app.DNS.AddRule("domain", "options.example", "local"); err != nil {
		t.Fatal(err)
	}
	d := NewDNS(app)
	d.SetSize(80, 21)
	d.reload()
	for _, mode := range []dnsMode{dnsServers, dnsRules} {
		d.mode = mode
		d.reload()
		d.Update(tea.KeyMsg{Type: tea.KeyEnd})
		before := d.list.SelectedKey()
		d.Update(chars("F"))
		d.Update(tea.WindowSizeMsg{Width: 120, Height: 27})
		d.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if d.mode != mode || d.list.SelectedKey() != before {
			t.Fatal("options cancellation changed the subview or selection")
		}
		d.Update(chars("F"))
		d.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
		if d.err != nil || d.mode != mode || d.list.SelectedKey() != before {
			t.Fatalf("options save changed the subview or selection: %v", d.err)
		}
	}
}

func TestMemberPickerSearchEscapeRestoresFilterAndPosition(t *testing.T) {
	app := pageFixture(t)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := app.Proxy.SaveManual(&config.Node{Name: name, Protocol: "http", Server: "example.invalid", Port: 80}); err != nil {
			t.Fatal(err)
		}
	}
	g := NewGroups(app)
	g.SetSize(80, 21)
	g.reload()
	g.Update(tea.KeyMsg{Type: tea.KeyEnter})
	g.Update(chars("m"))
	g.Update(tea.KeyMsg{Type: tea.KeyEnd})
	before := g.pickList.SelectedKey()
	g.Update(chars("/"))
	g.Update(chars("beta"))
	if len(g.pickList.Items) != 1 || !strings.Contains(g.View(), "beta") {
		t.Fatal("member search did not retain the filtered list")
	}
	g.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if g.pickQuery != "" || g.pickList.SelectedKey() != before {
		t.Fatal("member search cancellation did not restore position")
	}
}

func TestLogsKeepAbsoluteAnchorAsNewLinesArrive(t *testing.T) {
	app := pageFixture(t)
	l := NewLogs(app)
	l.SetSize(80, 10)
	// C2：周期刷新由激活启动，不再是 Init。
	if _, cmd := l.Update(ActivateMsg{}); cmd == nil {
		t.Fatal("activation did not start the refresh chain")
	}
	for i := range 100 {
		app.AppLog.AppendLine(fmt.Sprintf("fixture-line-%03d", i))
	}
	l.Update(tea.KeyMsg{Type: tea.KeyUp})
	before := strings.Join(strings.Split(l.View(), "\n")[1:8], "\n")
	for i := 100; i < 110; i++ {
		app.AppLog.AppendLine(fmt.Sprintf("fixture-line-%03d", i))
	}
	l.Update(tickMsg{Generation: l.tickGeneration})
	after := strings.Join(strings.Split(l.View(), "\n")[1:8], "\n")
	if before != after {
		t.Fatal("new logs moved the reader's anchor")
	}
	l.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if !strings.Contains(l.View(), "fixture-line-109") {
		t.Fatal("End did not resume following")
	}
	// 切走之后，切走前那一代在途 tick 到达时必须被丢弃，不得续出新链。
	stale := l.tickGeneration
	l.Update(DeactivateMsg{})
	if _, cmd := l.Update(tickMsg{Generation: stale}); cmd != nil {
		t.Fatal("stale tick renewed the refresh chain")
	}
	if l.active {
		t.Fatal("deactivation left the page active")
	}
	if _, cmd := l.Update(ActivateMsg{}); cmd == nil || !l.active {
		t.Fatal("re-activation did not restart the refresh chain")
	}
}

func TestDashboardRejectsOldInstanceAndResetsRateBaseline(t *testing.T) {
	app := pageFixture(t)
	// StartCore/RestartCore 会先生成配置；C15 起没有可用节点时生成会 fail-closed。
	if err := app.DB.CreateNode(&config.Node{
		Name: "seed-01", Protocol: "shadowsocks", Server: "127.0.0.1", Port: 8388,
		Enabled: true, Metadata: map[string]any{"method": "aes-128-gcm", "password": "pw"},
	}); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$1" in
 version) echo 'sing-box version 1.14.0'; exit 0 ;;
 check) exit 0 ;;
 run) trap 'exit 0' TERM; while :; do sleep 1; done ;;
esac
exit 1
`
	if err := os.WriteFile(app.Paths.CoreBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := app.StartCore(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := NewDashboard(app)
	d.fetchID = 1
	instance := app.Core.Status().StartedAt
	t0 := time.Now()
	sample := func(n int64, at time.Time) {
		d.Update(snapMsg{id: 1, startedAt: instance, sampledAt: at, conns: clashapi.Connections{UploadTotal: n, DownloadTotal: n}})
	}
	sample(1000, t0)
	sample(3000, t0.Add(time.Second))
	if d.upSpeed != 2000 || d.downSpeed != 2000 {
		t.Fatal("normal rates are incorrect")
	}
	sample(100, t0.Add(2*time.Second))
	if d.upSpeed != 0 || d.downSpeed != 0 {
		t.Fatal("counter rollback produced a rate spike or negative rate")
	}
	d.Update(snapMsg{id: 1, startedAt: instance, err: fmt.Errorf("offline")})
	sample(5000, t0.Add(3*time.Second))
	if d.upSpeed != 0 || d.downSpeed != 0 {
		t.Fatal("reconnect did not rebuild the baseline")
	}
	if err := app.RestartCore(context.Background()); err != nil {
		t.Fatal(err)
	}
	sample(999999, t0.Add(4*time.Second))
	if d.upTotal == 999999 {
		t.Fatal("snapshot from old core was accepted")
	}
	// C2：周期刷新由激活启动；切走之后，切走前那一代在途 tick 必须被丢弃。
	if _, cmd := d.Update(ActivateMsg{}); cmd == nil {
		t.Fatal("activation did not start the refresh chain")
	}
	stale := d.tickGeneration
	if _, cmd := d.Update(DeactivateMsg{}); cmd != nil {
		t.Fatal("deactivation produced a command")
	}
	if _, cmd := d.Update(tickMsg{Generation: stale}); cmd != nil {
		t.Fatal("stale tick renewed the refresh chain")
	}
	if d.active {
		t.Fatal("deactivation left the page active")
	}
}

// C6：geosite / geoip 的候选项来自**编译期固定**的内嵌分类库，转换一次后必须命中缓存；
// 而 rule_set 与它共用同一个函数，其中数据库条目是动态的，不能被一起缓存。
//
// 判定手法用分配数而不是切片地址：缓存命中时该路径零分配，而把 1953 条分类码重新
// 拼成 []components.Option 必然分配。地址稳定不作为验收标准。
func TestRuleReferenceChoicesStaticCache(t *testing.T) {
	app := pageFixture(t)
	r := NewRules(app)
	r.SetSize(120, 30)
	r.cur = &config.RoutingGroup{ID: 1, Name: "基准组", Target: "DIRECT"}
	r.openForm("add-rule")

	want := geositeOptionExpectation(t)

	r.form.SetValueByKey("type", "geosite")
	configureRuleValue(r.app, &r.form)
	field := r.form.Field("value")
	if field == nil || !field.Choice || !field.Multi {
		t.Fatal("geosite 类型没有把值字段配成多选")
	}
	if !slices.Equal(field.Options, want) {
		t.Fatalf("geosite 选项与分类库不一致：得到 %d 条，期望 %d 条", len(field.Options), len(want))
	}
	// 反向核对：这条期望来自分类库本身，不是空的（否则 0 == 0 会假通过）。
	if len(want) != catalog.Count("geosite") || !hasOption(field, "cn") {
		t.Fatalf("geosite 期望 %d 条且含 cn，实际 %d 条", catalog.Count("geosite"), len(want))
	}

	// 再配一次（等价于重开表单 / 从别的类型切回 geosite）：静态转换不得重跑。
	var short bool
	if n := testing.AllocsPerRun(5, func() {
		optionsSink = referenceChoices(app, "geosite", "")
		if len(optionsSink) != len(want) {
			short = true
		}
	}); n != 0 || short {
		t.Fatalf("geosite 静态选项在缓存命中路径上仍有分配：%.1f allocs/op（期望 0）", n)
	}

	// 两种取值形态不能互相串味：geosite 用分类码（cn），rule_set 用完整引用（geosite:cn）。
	r.form.SetValueByKey("type", "rule_set")
	configureRuleValue(r.app, &r.form)
	if !hasOption(r.form.Field("value"), "geosite:cn") {
		t.Fatal("rule_set 选项缺少分类库引用（完整引用形态）")
	}
	if hasOption(r.form.Field("value"), "cn") {
		t.Fatal("rule_set 选项混入了分类码形态的取值")
	}
	r.form.SetValueByKey("type", "geosite")
	configureRuleValue(r.app, &r.form)
	if !slices.Equal(r.form.Field("value").Options, want) {
		t.Fatal("geosite 的缓存被 rule_set 形态污染")
	}

	// rule_set 的动态部分不能被缓存：新建的规则集必须立刻出现在选项里。
	if err := app.DB.CreateRuleSet(&config.RuleSet{Name: "新建集", Tag: "fresh-set", SourceType: "remote", Format: "srs", URL: "https://example.invalid/y.srs", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	r.form.SetValueByKey("type", "rule_set")
	configureRuleValue(r.app, &r.form)
	if !hasOption(r.form.Field("value"), "fresh-set") {
		t.Fatal("rule_set 选项没有反映新建的规则集（动态部分被误缓存）")
	}
	// 反向核对：数据库里禁用的规则集不出现（防止断言被「全量列出」蒙对）。
	if err := app.DB.CreateRuleSet(&config.RuleSet{Name: "禁用集", Tag: "off-set", SourceType: "remote", Format: "srs", URL: "https://example.invalid/z.srs", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	configureRuleValue(r.app, &r.form)
	if hasOption(r.form.Field("value"), "off-set") {
		t.Fatal("rule_set 选项带上了已禁用的规则集")
	}

	// 表单交互（打开选项面板 + 搜索过滤）必须把 Options 当作只读。
	r.form.SetValueByKey("type", "geosite")
	configureRuleValue(r.app, &r.form)
	r.form.FocusKey("value")
	r.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(r.View(), "Space 勾选") {
		t.Fatal("没有打开多选选项面板，只读性断言会假通过")
	}
	for _, ch := range "cn" {
		r.Update(chars(string(ch)))
	}
	if !slices.Equal(r.form.Field("value").Options, want) {
		t.Fatal("表单交互改写了共享的静态选项")
	}
	// 共享缓存本身也必须原样：换一个表单再配一次应得到同一份内容。
	if got := referenceChoices(app, "geosite", ""); !slices.Equal(got, want) {
		t.Fatal("共享静态缓存被前一次表单交互改写")
	}
}

// geositeOptionExpectation 按分类库现状构造期望选项（取值用分类码）。
func geositeOptionExpectation(t *testing.T) []components.Option {
	t.Helper()
	refs := catalog.Search("geosite", "", 0)
	want := make([]components.Option, 0, len(refs))
	for _, ref := range refs {
		want = append(want, components.Option{Value: ref.Code, Label: ref.String()})
	}
	return want
}
