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
	"github.com/bbbstyyy/karing-tui/internal/core"
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

// nodeNamesOf 把节点切片拼成便于断言的顺序串。
func nodeNamesOf(nodes []*config.Node) string {
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	return strings.Join(names, ",")
}

// TestProfilesFilterUsesPreSortedOrderAndDoesNotResort 钉住 C7 的核心不变量：
// 搜索路径只做 O(N) 过滤，展示顺序完全来自装载时排好一次的 p.sortedNodes。
//
// 直接断言「顺序和排序键一致」是无效的——把已排好的列表再排一遍还是原样。所以
// 先把预排序结果**人为置成逆序**，再走真实按键路径：只要过滤环节（或按键路径上的
// 任何环节）仍然自行排序，逆序就会立刻被"纠正"回名称序。赋值 sortBy 同理无效，
// 排序谓词本身没变。
func TestProfilesFilterUsesPreSortedOrderAndDoesNotResort(t *testing.T) {
	app := pageFixture(t)
	p := NewProfiles(app)
	p.SetSize(80, 21)
	p.mode = profilesNodes
	p.nodes = []*config.Node{ // 装载顺序刻意不是名称序
		{ID: 3, Name: "charlie", Server: "s3.example.invalid", Protocol: "vmess", Enabled: true, LatencyMS: -1},
		{ID: 1, Name: "alpha", Server: "s1.example.invalid", Protocol: "vmess", Enabled: true, LatencyMS: -1},
		{ID: 2, Name: "bravo", Server: "s2.example.invalid", Protocol: "trojan", Enabled: true, LatencyMS: -1},
	}
	p.resortNodes()
	p.applyNodeFilter()
	if got := nodeNamesOf(p.filtered); got != "alpha,bravo,charlie" {
		t.Fatalf("装载后未按名称排序: %s", got)
	}

	slices.Reverse(p.sortedNodes)
	reversed := nodeViewNamesOf(p.sortedNodes)

	// 真实按键路径：进入搜索态并逐字输入。
	p.Update(chars("/"))
	p.Update(chars("a"))
	if got := nodeViewNamesOf(p.sortedNodes); got != reversed {
		t.Fatalf("搜索路径重排了预排序结果: %s", got)
	}
	if got := nodeNamesOf(p.filtered); got != "charlie,bravo,alpha" {
		t.Fatalf("过滤结果被重新排序: %s", got)
	}
	// 反向核对：上面的断言不能因为「过滤压根没跑」而假通过。
	// （Esc 退出再重新进入搜索态：搜索框内容会被重设为 p.search，凑不成 "ch"。）
	p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	p.Update(chars("/"))
	p.Update(chars("c"))
	p.Update(chars("h"))
	if got := nodeNamesOf(p.filtered); got != "charlie" {
		t.Fatalf("搜索没有生效，前一条断言失去意义: %s", got)
	}
}

// searchReference 是**与实现无关**的搜索语义参照：对 Name 与 Server 各自单独做
// ToLower 再判断是否包含关键词。C8 只是把这两次 ToLower 提前到装载期，语义必须
// 逐项保持相等，因此这份参照不引用被测代码的任何派生字段。
func searchReference(n *config.Node, lowerQuery string) bool {
	return strings.Contains(strings.ToLower(n.Name), lowerQuery) ||
		strings.Contains(strings.ToLower(n.Server), lowerQuery)
}

// matchedNamesOf 把一次过滤的结果取成便于比较的排序名称集。
func matchedNamesOf(nodes []*config.Node) []string {
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	slices.Sort(names)
	return names
}

// TestProfilesSearchTextMatchesReferenceSemantics 钉住 C8 的「行为零变化」：
// 同一组节点 + 同一批关键词，过滤结果必须与逐字段 ToLower 的参照实现逐项相等。
//
// 关键词表刻意覆盖三件事：
//   - "HK" / "hong" / "EXAMPLE"：大小写不敏感；
//   - "hk1.example" / "e.com"：按**服务器地址**也能搜到；
//   - "a hk1"：这是**跨字段误匹配**的探针。"relay A" 的名称与 "hk1.example.com"
//     的地址一旦被拼成一个字符串（无论空格还是 \x00 分隔），这个词就会凭空命中；
//     逐字段比对则不会。当前实现不需要分隔符，这条探针用来挡住今后「拼成一个
//     searchText 省一次 Contains」的改动。
//
// 反向核对放在末尾：至少一个关键词必须真的命中过，否则整轮断言可能因「过滤压根
// 没跑」而全部假通过。
func TestProfilesSearchTextMatchesReferenceSemantics(t *testing.T) {
	app := pageFixture(t)
	nodes := []*config.Node{
		{ID: 1, Name: "relay A", Server: "hk1.example.com", Protocol: "vmess", Enabled: true, LatencyMS: -1},
		{ID: 2, Name: "Hong Kong 01", Server: "10.0.0.1", Protocol: "trojan", Enabled: true, LatencyMS: -1},
		{ID: 3, Name: "东京 03", Server: "jp3.Example.COM", Protocol: "vless", Enabled: true, LatencyMS: -1},
		{ID: 4, Name: "UPPER", Server: "lower.host", Protocol: "http", Enabled: false, LatencyMS: -1},
	}
	p := NewProfiles(app)
	p.SetSize(80, 21)
	p.mode = profilesNodes
	p.nodes = nodes
	p.resortNodes()
	p.applyNodeFilter()

	queries := []string{
		"", "hk", "HK", "hong kong", "hong", "01",
		"example", "EXAMPLE", "hk1.example", "e.com",
		"relay", "东京", "upper", "lower.host",
		"a hk1", "kong 01 10.0", "不存在",
	}
	matchedAny := false
	for _, query := range queries {
		p.search = query
		p.applyNodeFilter()

		var want []*config.Node
		for _, n := range nodes {
			if searchReference(n, strings.ToLower(query)) {
				want = append(want, n)
			}
		}
		if len(want) > 0 {
			matchedAny = true
		}
		got, want2 := matchedNamesOf(p.filtered), matchedNamesOf(want)
		if !slices.Equal(got, want2) {
			t.Fatalf("关键词 %q：过滤结果 %v，参照实现 %v", query, got, want2)
		}
	}
	if !matchedAny {
		t.Fatal("没有任何关键词命中，前面对照失去意义")
	}
}

// TestProfilesFilterReadsCachedSearchText 钉住 C8 的实现要点：过滤循环读的是装载期
// 算好的 nodeView.lowerName / lowerServer，而不是当场对 Name/Server 做 ToLower。
//
// 手法是把某条视图的小写副本换成一个人为哨兵值（Name/Server 里都不含它），再用哨兵值
// 搜索：命中即证明过滤读的是缓存字段。这正是「C8 真的落地了」的直接证据——上面那条
// 行为对照测试在旧实现上同样会通过，无法区分两者。（实测：把过滤循环临时改回
// 当场 ToLower，本测试报 "过滤没有使用装载期缓存的 searchText"，对照测试仍通过。）
//
// 反向核对在末尾：哨兵必须不出现在任何 Name/Server 里，否则命中可能来自现算。
func TestProfilesFilterReadsCachedSearchText(t *testing.T) {
	app := pageFixture(t)
	p := NewProfiles(app)
	p.SetSize(80, 21)
	p.mode = profilesNodes
	p.nodes = []*config.Node{
		{ID: 1, Name: "alpha", Server: "a.example.invalid", Protocol: "http", Enabled: true, LatencyMS: -1},
		{ID: 2, Name: "bravo", Server: "b.example.invalid", Protocol: "http", Enabled: true, LatencyMS: -1},
	}
	p.resortNodes()
	p.applyNodeFilter()

	const canary = "zz-canary-zz"
	patched := false
	for i := range p.sortedNodes {
		if p.sortedNodes[i].Name == "bravo" {
			p.sortedNodes[i].lowerName = canary
			patched = true
		}
	}
	if !patched {
		t.Fatal("没找到用于打哨兵的视图")
	}

	p.search = canary
	p.applyNodeFilter()
	if got := nodeNamesOf(p.filtered); got != "bravo" {
		t.Fatalf("过滤没有使用装载期缓存的 searchText: %s", got)
	}
	for _, n := range p.nodes {
		if strings.Contains(n.Name, canary) || strings.Contains(n.Server, canary) {
			t.Fatal("哨兵出现在 Name/Server 里，断言失去意义")
		}
	}
}

// nodeViewNamesOf 把预排序视图拼成便于断言的顺序串。
func nodeViewNamesOf(views []nodeView) string {
	names := make([]string, 0, len(views))
	for _, v := range views {
		names = append(names, v.Name)
	}
	return strings.Join(names, ",")
}

// TestProfilesRenameRebuildsSearchText 覆盖 C8 改法第 4 条：searchText 是装载期派生值，
// 改名 / 改服务器之后必须重建，否则搜索命中的是旧名字。
//
// 走真实编辑路径（SaveManual 落库 → reloadNodes，也就是 submitNodeForm 保存后的那一步），
// 而不是直接改结构体字段——只有前者能证明「改 Name/Server 的入口确实覆盖到了」。
func TestProfilesRenameRebuildsSearchText(t *testing.T) {
	app := pageFixture(t)
	node := &config.Node{Name: "旧名字", Protocol: "http", Server: "old.example.com", Port: 80}
	if err := app.Proxy.SaveManual(node); err != nil {
		t.Fatal(err)
	}
	p := NewProfiles(app)
	p.SetSize(80, 21)
	p.mode = profilesNodes
	p.reloadNodes()

	p.search = "旧名字"
	p.applyNodeFilter()
	if len(p.filtered) != 1 {
		t.Fatalf("按原名搜索应命中 1 条，实际 %d 条", len(p.filtered))
	}

	node.Name, node.Server = "新名字", "new.example.com"
	if err := app.Proxy.SaveManual(node); err != nil {
		t.Fatal(err)
	}
	p.reloadNodes()

	p.search = "新名字"
	p.applyNodeFilter()
	if len(p.filtered) != 1 {
		t.Fatalf("改名后按新名搜索应命中 1 条，实际 %d 条", len(p.filtered))
	}
	p.search = "旧名字"
	p.applyNodeFilter()
	if len(p.filtered) != 0 {
		t.Fatal("改名后仍能按旧名搜到，searchText 未重建")
	}
	p.search = "new.example"
	p.applyNodeFilter()
	if len(p.filtered) != 1 {
		t.Fatalf("改服务器后按新地址搜索应命中 1 条，实际 %d 条", len(p.filtered))
	}
}

// TestProfilesSortByLatencyReordersOnlyOnExplicitTriggers 覆盖 C7 改法第 4 条：
// 延迟会被测速改写，所以「测速回写 + 重新装载」必须计入重排触发点；切换排序键同理。
func TestProfilesSortByLatencyReordersOnlyOnExplicitTriggers(t *testing.T) {
	app := pageFixture(t)
	nodes := []*config.Node{
		{Name: "charlie", Protocol: "http", Server: "c.example.invalid", Port: 80},
		{Name: "alpha", Protocol: "http", Server: "a.example.invalid", Port: 80},
		{Name: "bravo", Protocol: "http", Server: "b.example.invalid", Port: 80},
	}
	for _, n := range nodes {
		if err := app.Proxy.SaveManual(n); err != nil {
			t.Fatal(err)
		}
	}
	p := NewProfiles(app)
	p.SetSize(80, 21)
	p.mode = profilesNodes
	p.sortBy = "latency"
	p.reloadNodes()
	// 都未测速：按 latencyLess 的规则退化为名称升序。
	if got := nodeNamesOf(p.filtered); got != "alpha,bravo,charlie" {
		t.Fatalf("未测速节点未按名称排序: %s", got)
	}

	// 测速回写（落库）后由 reloadNodes 重新装载 → 顺序必须反映新延迟。
	if err := app.DB.UpdateNodeLatency(nodes[1].ID, 300, time.Now()); err != nil { // alpha 变慢
		t.Fatal(err)
	}
	if err := app.DB.UpdateNodeLatency(nodes[2].ID, 20, time.Now()); err != nil { // bravo 变快
		t.Fatal(err)
	}
	p.reloadNodes()
	if got := nodeNamesOf(p.filtered); got != "bravo,alpha,charlie" {
		t.Fatalf("测速回写后未按延迟重排: %s", got)
	}

	// 切换排序键（真实按键）→ 立即重排回名称序。
	p.Update(chars("o"))
	if p.sortBy != "name" {
		t.Fatalf("排序键未切换: %q", p.sortBy)
	}
	if got := nodeNamesOf(p.filtered); got != "alpha,bravo,charlie" {
		t.Fatalf("切换排序键后未重排: %s", got)
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

// openMemberPicker 从代理组列表进入第一个组的详情，再打开成员勾选模式。
func openMemberPicker(t *testing.T, app *application.App) *Groups {
	t.Helper()
	g := NewGroups(app)
	g.SetSize(80, 21)
	g.reload()
	g.Update(tea.KeyMsg{Type: tea.KeyEnter}) // 进入组详情
	g.Update(chars("m"))                     // 打开成员勾选
	if g.mode != groupsPick {
		t.Fatal("未能进入成员勾选模式")
	}
	return g
}

// pickItemsReference 是勾选列表过滤的独立参照实现：只读候选的 label，不复用
// refreshPickItems 里的任何派生字段（C10 之前它就是生产实现的那两行）。
func pickItemsReference(cands []pickCandidate, query string, set map[string]bool) []string {
	lower := strings.ToLower(query)
	var items []string
	for _, c := range cands {
		if !strings.Contains(strings.ToLower(c.label), lower) {
			continue
		}
		mark := "[ ] "
		if set[c.key] {
			mark = "[x] "
		}
		items = append(items, mark+c.label)
	}
	return items
}

// TestMemberPickerSearchMatchesReferenceSemantics 用独立参照实现钉住勾选列表的过滤语义：
// 「Label 不区分大小写地包含查询词」。C10 只是把 ToLower 的结果挪到候选列表建立时，
// 语义必须逐项一致，勾选状态也不得影响结果集合。
//
// 参照实现直接写原始表达式，因此它在本项改动前后都通过——它证明的是「行为没变」；
// 「改动真的落地」由 TestMemberPickerSearchReadsCachedSearchKey 负责。
func TestMemberPickerSearchMatchesReferenceSemantics(t *testing.T) {
	app := pageFixture(t)
	for _, n := range []*config.Node{
		{Name: "Hong Kong 01", Protocol: "http", Server: "hk1.example.com", Port: 443},
		{Name: "东京 03", Protocol: "http", Server: "jp3.example.com", Port: 8080},
		{Name: "UPPER", Protocol: "http", Server: "lo.example.com", Port: 80},
	} {
		if err := app.Proxy.SaveManual(n); err != nil {
			t.Fatal(err)
		}
	}
	g := openMemberPicker(t, app)
	last := g.cands[len(g.cands)-1].key
	g.pickSet[last] = true // 有勾选项，且勾选不得改变搜索结果集合

	// 勾选列表的 label 是「名称 (协议:端口) [来源]」加组前缀，不含 server，
	// 因此这里只放会真正出现在 label 里的词。
	queries := []string{"", "hong", "HONG", "01", "东京", "http", "upper", "[组]", "节点", "不存在"}
	matchedAny := false
	for _, q := range queries {
		g.pickQuery = q
		g.refreshPickItems()
		want := pickItemsReference(g.cands, q, g.pickSet)
		if len(want) > 0 {
			matchedAny = true
		}
		if !slices.Equal(g.pickList.Items, want) {
			t.Fatalf("关键词 %q：勾选列表 %v，参照实现 %v", q, g.pickList.Items, want)
		}
	}
	if !matchedAny {
		t.Fatal("没有任何关键词命中，前面对照失去意义")
	}
}

// TestMemberPickerSearchReadsCachedSearchKey 钉住 C10 的实现要点：过滤循环读的是
// 候选列表建立时算好的 pickCandidate.search，而不是每次按键当场对 label 做 ToLower。
//
// 手法与 C8 的 TestProfilesFilterReadsCachedSearchText 一致：把某个候选的 search
// 换成 label 里不含的哨兵值，再用哨兵搜索——命中即证明读的是缓存字段。上面那条
// 对照测试在旧实现上同样通过，无法区分两者。（实测：把过滤循环临时改回当场
// ToLower，本测试报「未命中」而对照测试仍通过。）
//
// 末尾反向核对：哨兵不得出现在任何 label 里，否则命中可能来自现算。
func TestMemberPickerSearchReadsCachedSearchKey(t *testing.T) {
	app := pageFixture(t)
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if err := app.Proxy.SaveManual(&config.Node{Name: name, Protocol: "http", Server: "example.invalid", Port: 80}); err != nil {
			t.Fatal(err)
		}
	}
	g := openMemberPicker(t, app)

	const canary = "zz-canary-zz"
	patched := false
	for i := range g.cands {
		if strings.Contains(g.cands[i].label, "bravo") {
			g.cands[i].search = canary
			patched = true
		}
	}
	if !patched {
		t.Fatal("没找到用于打哨兵的候选")
	}

	g.pickQuery = canary
	g.refreshPickItems()
	if len(g.pickList.Items) != 1 || !strings.Contains(g.pickList.Items[0], "bravo") {
		t.Fatalf("过滤没有使用候选项上缓存的小写搜索键: %v", g.pickList.Items)
	}
	for _, c := range g.cands {
		if strings.Contains(c.label, canary) {
			t.Fatal("哨兵出现在 label 里，断言失去意义")
		}
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

// C11 验收：按住 ↓ 移动 20 次，日志过滤只执行 1 次（缓冲无新写入时），
// 且 View 不触发重算（不再 Snapshot / Strip）。recalcs 是被测路径上的
// 重算计数器；「base == 0 则夹具失效」与「连续按键渲染内容必须变化」
// 是两条反向核对，防止断言因永不触发而假通过。
func TestLogsNavigationReusesFilterCache(t *testing.T) {
	app := pageFixture(t)
	l := NewLogs(app)
	l.SetSize(80, 10)
	l.Update(ActivateMsg{})
	for i := range 100 {
		app.AppLog.AppendLine(fmt.Sprintf("fixture-%03d", i))
	}
	// 首次导航：缓冲在 Activate 后有新写入，必须重算一次。
	l.Update(tea.KeyMsg{Type: tea.KeyUp})
	base := l.recalcs
	if base == 0 {
		t.Fatal("首次导航应触发一次重算，夹具失效")
	}
	if l.positions[0].following {
		t.Fatal("KeyUp 后应处于回看状态，导航未生效")
	}
	for range 20 {
		l.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if got := l.recalcs - base; got != 0 {
		t.Fatalf("20 次导航触发了 %d 次重算，期望 0（缓存未变化）", got)
	}
	v1 := l.View()
	l.Update(tea.KeyMsg{Type: tea.KeyUp})
	v2 := l.View()
	if v1 == v2 {
		t.Fatal("连续两次按键渲染内容相同，导航未生效，重算断言失去意义")
	}
	before := l.recalcs
	_ = l.View()
	_ = l.View()
	if l.recalcs != before {
		t.Fatalf("View 触发了 %d 次重算；View 应只读缓存", l.recalcs-before)
	}
}

// C11 兼容性：过滤结果与独立重写的朴素参照实现逐项一致（不复用被测代码
// 的 refresh/plainByID，仅共用 ansi.Strip 这个库原语）。夹具覆盖 ANSI 控制序列、
// 大写（触发 ToLower 分配）、无命中的反例。
func TestLogsFilterMatchesReferenceImplementation(t *testing.T) {
	app := pageFixture(t)
	l := NewLogs(app)
	l.SetSize(80, 10)
	fixture := []string{
		"\x1b[32mINFO\x1b[0m Node-001 handshake done",
		"\x1b[31mERROR\x1b[0m node-002 timeout",
		"plain warn text NODE-003",
		"\x1b[2J\x1b[HDEBUG\x1b[0m node-004 started",
		"info level-mixed Info node-005",
		"—— 这一行不匹配任何用例 ————",
	}
	for _, line := range fixture {
		app.AppLog.AppendLine(line)
	}
	l.Update(ActivateMsg{})
	sawHit, sawMiss := false, false
	for _, tc := range []struct{ name, query, level string }{
		{"无过滤", "", ""},
		{"查询命中大小写混合", "node-00", ""},
		{"查询大写", "NODE", ""},
		{"仅级别", "", "error"},
		{"级别加查询", "node", "info"},
		{"查询无命中", "zzz-none", ""},
	} {
		l.query, l.level = tc.query, tc.level
		l.Update(tickMsg{Generation: l.tickGeneration})
		first, _, all := app.AppLog.Snapshot()
		var want []logLine
		q := strings.ToLower(tc.query)
		for i, line := range all {
			plain := strings.ToLower(ansi.Strip(line))
			if tc.query != "" && !strings.Contains(plain, q) {
				continue
			}
			if tc.level != "" && !strings.Contains(plain, tc.level) {
				continue
			}
			want = append(want, logLine{id: first + i, text: line})
		}
		got := l.cachedHits
		if len(got) != len(want) {
			t.Fatalf("%s: 结果数 %d != 参照 %d", tc.name, len(got), len(want))
		}
		for j := range want {
			if got[j] != want[j] {
				t.Errorf("%s: 第 %d 项 got %+v, want %+v", tc.name, j, got[j], want[j])
			}
		}
		if tc.name == "查询命中大小写混合" && len(got) > 0 {
			sawHit = true
		}
		if tc.name == "查询无命中" && len(got) == 0 {
			sawMiss = true
		}
	}
	if !sawHit || !sawMiss {
		t.Fatalf("夹具失效：命中/无命中反例缺失（hit=%v miss=%v）", sawHit, sawMiss)
	}
}

// C11：锚点被轮转挤出缓冲时，钳制与「更早日志已轮转」提示必须发生在
// Update 侧（refresh），View 只读状态——旧行为在 View 里改写锚点，
// 同一状态两次渲染字节不同；双渲染字节一致即证明副作用已移出 View。
func TestLogsRotationClampMovesOutOfView(t *testing.T) {
	app := pageFixture(t)
	app.AppLog = core.NewLogBuf(10) // 小缓冲便于触发回绕
	l := NewLogs(app)
	l.SetSize(80, 10)
	l.Update(ActivateMsg{})
	for i := range 25 {
		app.AppLog.AppendLine(fmt.Sprintf("L%02d", i))
	}
	l.Update(tea.KeyMsg{Type: tea.KeyUp}) // 固定锚点：hits ids 15..24，anchor=17
	if l.positions[0].following {
		t.Fatal("KeyUp 后应处于回看状态")
	}
	for i := 25; i < 30; i++ {
		app.AppLog.AppendLine(fmt.Sprintf("L%02d", i))
	}
	l.Update(tickMsg{Generation: l.tickGeneration})
	// 回绕后 ids 20..29：anchor 17 < hits[0].id 20，应钳到 20 并记录状态。
	if got := l.positions[0].anchor; got != 20 {
		t.Fatalf("锚点应钳到最旧行 20, got %d", got)
	}
	v1 := l.View()
	if !strings.Contains(v1, "更早日志已轮转") {
		t.Fatal("锚点被轮转挤出后应显示提示")
	}
	v2 := l.View()
	if v1 != v2 {
		t.Fatal("同一状态两次渲染字节不同——View 里仍有状态改写")
	}
	before := l.recalcs
	_ = l.View()
	if l.recalcs != before {
		t.Fatalf("View 触发了重算（Snapshot/Strip 应只在 Update 侧）")
	}
	l.Update(tea.KeyMsg{Type: tea.KeyEnd})
	v3 := l.View()
	if strings.Contains(v3, "更早日志已轮转") {
		t.Fatal("跟随中不应显示轮转提示")
	}
	if !strings.Contains(v3, "L29") {
		t.Fatal("End 后应跟随显示最新日志")
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

// --- C9 · 分类库浏览的物化上限与标注缓存 ---

// TestCatalogListMaterializesAtMostLimit 分类库列表必须「物化受限、总数准确」：
// 单字符搜索 geosite 命中上千条，此前会为每一条查缓存状态、建列表项与表格行。
func TestCatalogListMaterializesAtMostLimit(t *testing.T) {
	app := pageFixture(t)
	r := NewRules(app)
	r.SetSize(140, 40)
	r.openCatalog(rulesGroups)
	r.catQuery = "a"
	r.reloadCatalog()

	full := catalog.Search(catalog.KindGeosite, "a", 0) // 独立参照：旧行为（全量物化）
	// 反向核对：夹具必须让上限真正生效，否则下面的断言全是空转
	if len(full) <= catalogHitLimit {
		t.Fatalf("夹具失效：geosite 查询 %q 只命中 %d 条（需 > 上限 %d）", "a", len(full), catalogHitLimit)
	}
	if len(r.catHits) != catalogHitLimit {
		t.Errorf("物化 %d 条，期望恰好上限 %d 条", len(r.catHits), catalogHitLimit)
	}
	if r.catTotal != len(full) {
		t.Errorf("catTotal = %d，期望与全量命中 %d 一致", r.catTotal, len(full))
	}
	for i, hit := range r.catHits {
		if hit.ref != full[i] { // 截断只能取前缀，不得改变顺序或内容
			t.Fatalf("第 %d 条 = %v，期望 %v", i, hit.ref, full[i])
		}
	}
	if len(r.list.Items) != len(r.catHits) || len(r.list.Keys) != len(r.catHits) {
		t.Errorf("列表项 %d 条 / 键 %d 个，期望与命中 %d 条一致", len(r.list.Items), len(r.list.Keys), len(r.catHits))
	}
	// View 每帧经 table() 重建表格行，同样不得超出上限
	r.table()
	if len(r.list.Rows) != len(r.catHits) {
		t.Errorf("表格行 %d，期望与命中 %d 条一致", len(r.list.Rows), len(r.catHits))
	}
	// 标题要给出真实总数并说明被截断，否则用户会以为总共就这么多条
	view := ansi.Strip(r.View())
	if !strings.Contains(view, strconv.Itoa(len(full))) {
		t.Errorf("标题未给出真实命中总数 %d：\n%s", len(full), view)
	}
	if !strings.Contains(view, "仅列前") {
		t.Errorf("标题未提示列表被截断：\n%s", view)
	}
	// 光标索引仍与命中下标一一对应（截断后不能有偏移）
	r.list.Cursor = len(r.catHits) - 1
	if ref, ok := r.selectedCatalogRef(); !ok || ref != full[catalogHitLimit-1] {
		t.Errorf("末尾选中 = %v（ok=%v），期望 %v", ref, ok, full[catalogHitLimit-1])
	}
}

// TestCatalogMarksFollowRuleChanges 引用 / 缓存标记必须准确，且引用集合的缓存要随
// 规则变更失效——在同一个分类库会话里新增引用后标记立刻跟上，不必切页重进。
//
// 夹具说明：首次启动会导入地区预置 cn，因此 geosite:cn 天然处于「已引用」；
// 本节另给 cn 造一个缓存文件，于是同一个命中项同时钉住两个方向的正例与反例。
func TestCatalogMarksFollowRuleChanges(t *testing.T) {
	app := pageFixture(t)
	dir := app.Rules.CacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cn := catalog.Ref{Kind: catalog.KindGeosite, Code: "cn"}
	if err := os.WriteFile(app.Rules.CatalogCachePath(cn), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	grp := &config.RoutingGroup{Name: "基准组", Target: "DIRECT", Enabled: true}
	if err := app.DB.CreateRoutingGroup(grp); err != nil {
		t.Fatal(err)
	}
	r := NewRules(app)
	r.SetSize(140, 40)
	r.cur = grp
	r.mode = rulesGroupRl
	r.openCatalog(rulesGroupRl)
	r.catQuery = "cn"
	r.reloadCatalog()

	find := func(ref catalog.Ref) (catalogHit, bool) {
		for _, hit := range r.catHits {
			if hit.ref == ref {
				return hit, true
			}
		}
		return catalogHit{}, false
	}
	hit, ok := find(cn)
	if !ok {
		t.Fatalf("查询 %q 未命中 cn，夹具失效：%v", r.catQuery, r.catHits)
	}
	if !hit.cached {
		t.Error("cn 有缓存文件，应标为已缓存")
	}
	if !hit.inUse {
		t.Error("地区预置 cn 引用了 geosite:cn，应标为已引用")
	}

	// 反例方向：同一次搜索结果里挑一个既无缓存、也未被引用的命中。
	// 找不到就说明夹具失效（否则下面的断言会因为「全都已引用」而假通过）。
	target := catalog.Ref{}
	for _, h := range r.catHits {
		if h.ref != cn && !h.cached && !h.inUse {
			target = h.ref
			break
		}
	}
	if target.Code == "" {
		t.Fatalf("查询 %q 的结果里没有既未缓存也未引用的条目，夹具失效：%v", r.catQuery, r.catHits)
	}

	// 走页面真实写入路径新增引用：UpdateGroup + MarkConfigDirty，随后 reloadCatalog。
	// 若引用集合缓存只在「进入分类库时」算一次，这里的标记不会更新。
	r.addCatalogRule(target)
	got, ok := find(target)
	if !ok {
		t.Fatalf("新增引用后 %s 从列表消失：%v", target, r.catHits)
	}
	if !got.inUse {
		t.Error("新增引用后标记未跟上（引用集合缓存未随配置变更失效）")
	}
	if got.cached {
		t.Errorf("%s 没有缓存文件，却标为已缓存", target)
	}
	// 表格「引用」列消费同一份已物化标注
	r.table()
	idx := slices.IndexFunc(r.catHits, func(h catalogHit) bool { return h.ref == target })
	if idx < 0 || idx >= len(r.list.Rows) || r.list.Rows[idx][1] != "已引用" {
		t.Errorf("表格「引用」列 = %v，期望 已引用", r.list.Rows[idx])
	}
}

// TestCatalogViewReadsCacheIndexNotPerHitStat 判决性测试：缓存标记必须来自
// 「列一次目录」的索引，而不是逐条 CatalogCached（os.Stat）。
//
// 手法：把缓存目录设成**不可列但可 stat**（0111）。目录里确实放着 cn 的缓存文件，
// 因此 CatalogCached(cn) 仍为 true（按名 stat 只需目录的搜索权限），而列目录失败。
// 标记若来自索引，cn 必须显示「未缓存」；若回退成逐条 os.Stat 就会显示「已缓存」。
// 反向核对就在同一处：断言时刻 CatalogCached(cn) 必须为 true，否则本测试无法区分两者。
//
// 本测试不主张「目录不可列 ⇒ 未缓存」是产品语义（真实用户的缓存目录永远可列），
// 只用来钉住标记的取数来源。
func TestCatalogViewReadsCacheIndexNotPerHitStat(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 会绕过目录权限检查，本探针失效")
	}
	app := pageFixture(t)
	dir := app.Rules.CacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cn := catalog.Ref{Kind: catalog.KindGeosite, Code: "cn"}
	if err := os.WriteFile(app.Rules.CatalogCachePath(cn), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// TempDir 的清理需要写权限，本回调在它之前执行（cleanup 后进先出）
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := os.Chmod(dir, 0o111); err != nil {
		t.Fatal(err)
	}
	if !app.Rules.CatalogCached(cn) {
		t.Fatal("目录 0111 下按名 stat 应仍成功——否则本测试区分不出两种取数方式")
	}

	r := NewRules(app)
	r.SetSize(140, 40)
	r.openCatalog(rulesGroups)
	r.catQuery = "cn"
	r.reloadCatalog()
	for _, hit := range r.catHits {
		if hit.ref == cn {
			if hit.cached {
				t.Error("缓存标记不是来自列目录索引（仍存在逐条 os.Stat）")
			}
			return
		}
	}
	t.Fatalf("查询 %q 未命中 cn，夹具失效：%v", r.catQuery, r.catHits)
}

// TestCatalogViewListsCacheDirOncePerReload 每次按键只允许列一次缓存目录：
// 调用次数与命中总数无关（此前是「每个命中一次 os.Stat」）。
func TestCatalogViewListsCacheDirOncePerReload(t *testing.T) {
	app := pageFixture(t)
	calls := 0
	app.Rules.ReadDir = func(dir string) ([]os.DirEntry, error) {
		calls++
		return os.ReadDir(dir)
	}
	r := NewRules(app)
	r.SetSize(140, 40)
	r.openCatalog(rulesGroups) // 内部也会重载一次
	r.catQuery = "a"

	calls = 0
	for range 20 {
		r.reloadCatalog()
	}
	// 反向核对：这次搜索确实命中上千条，否则「1 次」没有说服力
	if r.catTotal <= catalogHitLimit {
		t.Fatalf("夹具失效：查询 %q 只命中 %d 条", r.catQuery, r.catTotal)
	}
	if calls != 20 {
		t.Errorf("20 次重载共列目录 %d 次，期望每次重载恰好 1 次（而非每个命中一次）", calls)
	}
}
