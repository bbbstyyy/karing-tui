package pages

import (
	"fmt"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
)

// benchApp 构造一个真实但为空的应用实例（临时目录 + 完整迁移），
// 供需要 DB / 管理器语义的页面基准使用。构造成本在计时之外。
func benchApp(b *testing.B) *application.App {
	b.Helper()
	b.Setenv("KARING_HOME", b.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		b.Fatal(err)
	}
	app, err := application.New(paths)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = app.Close() })
	return app
}

// benchNodes 生成固定规模的节点数据。刻意不用大 fixture：规模由参数决定，
// 曲线随 N 的走势比绝对值更能说明复杂度（见 CHECKLIST-v4 §5「基准稳定性」）。
func benchNodes(n int) []*config.Node {
	nodes := make([]*config.Node, 0, n)
	tested := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	for i := range n {
		latency := int64(-1)
		if i%5 != 0 {
			latency = int64((i * 37) % 500)
		}
		nodes = append(nodes, &config.Node{
			ID:             int64(i + 1),
			SubscriptionID: 1,
			Name:           fmt.Sprintf("node-%05d", i),
			Protocol:       []string{"vmess", "vless", "trojan"}[i%3],
			Server:         fmt.Sprintf("10.%d.%d.%d", (i/65536)%256, (i/256)%256, i%256),
			Port:           10000 + i%5000,
			Enabled:        i%7 != 0,
			LatencyMS:      latency,
			LastTested:     tested,
		})
	}
	return nodes
}

func benchProfilesNodes(b *testing.B, nodes int) *Profiles {
	b.Helper()
	p := NewProfiles(benchApp(b))
	p.SetSize(140, 40)
	p.mode = profilesNodes
	p.nodes = benchNodes(nodes)
	p.search = "node"
	p.applyNodeFilter()
	if len(p.filtered) == 0 {
		b.Fatal("过滤后无节点，基准失去意义")
	}
	return p
}

// BenchmarkProfilesFilter 度量节点过滤 + 建列表项的成本。
//
// 当前实现每次过滤都会重新排序并重建全部 Items（O(N log N) + O(N)），
// C7（预排序、过滤保持 O(N)）与 C8（lowercase 预计算）应让 ns/op 与 allocs/op 同时下降。
func BenchmarkProfilesFilter(b *testing.B) {
	for _, nodes := range []int{100, 1000, 5000, 10000} {
		b.Run(fmt.Sprintf("N%d", nodes), func(b *testing.B) {
			p := benchProfilesNodes(b, nodes)
			b.ReportAllocs()
			for b.Loop() {
				p.applyNodeFilter()
			}
		})
	}
}

// BenchmarkProfilesView5000 度量节点页单帧渲染成本（含当前在 View 内重建的表格行）。
//
// C4 把建行移出 View、C12 收敛 viewport 之后，理想复杂度是 O(可见行)，
// allocs/op 应不随总行数线性增长。
func BenchmarkProfilesView5000(b *testing.B) {
	p := benchProfilesNodes(b, 5000)
	b.ReportAllocs()
	for b.Loop() {
		_ = p.View()
	}
}

// BenchmarkGroupsSearch5000 度量成员勾选列表的搜索过滤成本（C10 目标）。
func BenchmarkGroupsSearch5000(b *testing.B) {
	g := NewGroups(benchApp(b))
	g.SetSize(140, 40)
	g.cands = make([]pickCandidate, 0, 5001)
	g.cands = append(g.cands, pickCandidate{key: "all", label: "全部节点（动态包含所有启用节点）"})
	for i := range 5000 {
		g.cands = append(g.cands, pickCandidate{
			key:   fmt.Sprintf("node:%d", i+1),
			label: fmt.Sprintf("node-%05d (vmess:443) [订阅 A]", i),
		})
	}
	g.pickQuery = "node-01"
	g.refreshPickItems()
	b.ReportAllocs()
	for b.Loop() {
		g.refreshPickItems()
	}
}

// BenchmarkLogsFilter1000 度量日志过滤一次的成本。
//
// LogBuf 上限 1000 行，因此这是满缓冲下的最坏情况。当前实现每帧调用两次
// （Update 导航 + View 渲染），每次都做全量拷贝 + 逐行 ANSI strip + ToLower。
// C11 引入版本号缓存后，未变化时该次调用应退化为命中缓存。
func BenchmarkLogsFilter1000(b *testing.B) {
	app := benchApp(b)
	l := NewLogs(app)
	l.SetSize(140, 40)
	for i := range 1000 {
		app.AppLog.AppendLine(fmt.Sprintf("\x1b[32mINFO\x1b[0m node-%04d handshake done in %dms", i, i%200))
	}
	l.query = "node-01"
	l.level = "info"
	if hits := l.lines(); len(hits) == 0 {
		b.Fatal("过滤后无日志，基准失去意义")
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = l.lines()
	}
}

// BenchmarkRuleFormKeyPress 度量规则表单「单次按键」的成本。
//
// 当前 rules.go 在每次 KeyMsg 后无条件调用 configureRuleValue，而它对
// rule_set / geosite / geoip 会重建整个 Options（含一次 DB 查询与一次分类库
// 全量物化）。C5 让它在 type 未变化时跳过，allocs/op 应大幅下降。
func BenchmarkRuleFormKeyPress(b *testing.B) {
	r := NewRules(benchApp(b))
	r.SetSize(140, 40)
	r.cur = &config.RoutingGroup{ID: 1, Name: "基准组", Target: "DIRECT"}
	r.openForm("add-rule")
	r.form.SetValueByKey("type", "rule_set")
	configureRuleValue(r.app, &r.form)
	r.form.FocusKey("value")
	key := chars("x")
	b.ReportAllocs()
	for b.Loop() {
		_, _ = r.Update(key)
	}
}

// BenchmarkRuleFormOpenGeosite 度量「表单打开 / 类型切换」时重建值字段选项的成本。
//
// geosite 的候选项来自编译期固定的内嵌清单，重复打开表单却在反复把同一份静态
// 数据转成 []components.Option。C6 缓存该转换后，热身后 allocs/op 应降到 0。
func BenchmarkRuleFormOpenGeosite(b *testing.B) {
	r := NewRules(benchApp(b))
	r.SetSize(140, 40)
	r.cur = &config.RoutingGroup{ID: 1, Name: "基准组", Target: "DIRECT"}
	r.openForm("add-rule")
	r.form.SetValueByKey("type", "geosite")
	configureRuleValue(r.app, &r.form)
	if len(r.form.Field("value").Options) < 1000 {
		b.Fatal("geosite 选项为空，基准失去意义")
	}
	b.ReportAllocs()
	for b.Loop() {
		configureRuleValue(r.app, &r.form)
	}
}

// BenchmarkReferenceChoicesGeosite 度量静态分类库 → 表单选项的转换本身。
func BenchmarkReferenceChoicesGeosite(b *testing.B) {
	app := benchApp(b)
	b.ReportAllocs()
	for b.Loop() {
		optionsSink = referenceChoices(app, "geosite", "")
	}
}

// BenchmarkReferenceChoicesRuleSet 度量混合路径：数据库条目是动态的（每次都要查），
// 分类库那一半是静态的（应命中缓存）。C6 只能消掉后者，allocs/op 应明显下降但不为 0。
func BenchmarkReferenceChoicesRuleSet(b *testing.B) {
	app := benchApp(b)
	if err := app.DB.CreateRuleSet(&config.RuleSet{Name: "基准集", Tag: "bench-set", SourceType: "remote", Format: "srs", URL: "https://example.invalid/x.srs", Enabled: true}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		optionsSink = referenceChoices(app, "rule_set", "")
	}
}

// optionsSink 阻止编译器把基准里被丢弃的返回值优化掉。
var optionsSink []components.Option

// BenchmarkCatalogView 度量内置分类库浏览「每次按键」的成本（reloadCatalog + 渲染）。
//
// 查询词 "a" 会让 geosite 命中接近全量（≈1953 条），当前实现会为全部命中项
// 逐条查缓存状态、格式化成列表项。C9 限制物化条数后 allocs/op 应显著下降。
func BenchmarkCatalogView(b *testing.B) {
	r := NewRules(benchApp(b))
	r.SetSize(140, 40)
	r.openCatalog(rulesGroups)
	r.catQuery = "a"
	r.reloadCatalog()
	b.ReportAllocs()
	for b.Loop() {
		r.reloadCatalog()
		_ = r.View()
	}
}

// BenchmarkDNSView 度量 DNS 服务器页单帧渲染成本。
//
// 当前 View 会查两次 SQLite（table + selectionDetails），且同一次渲染里
// ListServers 被调用多次。C3 改为内存缓存后，ns/op 应下降到与行数无关的量级。
func BenchmarkDNSView(b *testing.B) {
	app := benchApp(b)
	for i := range 200 {
		if _, err := app.DNS.AddServer(fmt.Sprintf("srv%03d", i), "udp", "8.8.8.8", "", ""); err != nil {
			b.Fatal(err)
		}
	}
	for i := range 200 {
		if _, err := app.DNS.AddRule("domain", fmt.Sprintf("example%03d.com", i), "srv000"); err != nil {
			b.Fatal(err)
		}
	}
	d := NewDNS(app)
	d.SetSize(140, 40)
	d.mode = dnsServers
	d.reload()
	if len(d.list.Items) == 0 {
		b.Fatal("DNS 列表为空，基准失去意义")
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = d.View()
	}
}
