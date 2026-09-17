package pages

import (
	"fmt"
	"strings"
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

// benchPerm 返回 [0,n) 的确定性洗牌结果。
//
// 刻意不用 math/rand：夹具必须跨机器、跨 Go 版本可复现，否则 benchstat 的对比会被
// 夹具本身的抖动污染。
func benchPerm(n int) []int {
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	state := uint64(0x9E3779B97F4A7C15)
	for i := n - 1; i > 0; i-- {
		state = state*6364136223846793005 + 1442695040888963407
		j := int((state >> 33) % uint64(i+1))
		perm[i], perm[j] = perm[j], perm[i]
	}
	return perm
}

// benchNodes 生成固定规模的节点数据。刻意不用大 fixture：规模由参数决定，
// 曲线随 N 的走势比绝对值更能说明复杂度（见 CHECKLIST-v4 §5「基准稳定性」）。
//
// scrambled=true 时名称按确定性洗牌排列。这不是为了"更随机"，而是为了让 C7 的
// 成本可测：旧实现的排序是插入排序，在**已按名称排好**的输入上退化为 O(N) 最好
// 情况（每个元素只比一次），而真实订阅返回的节点名不会恰好有序。
func benchNodes(n int, scrambled bool) []*config.Node {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	if scrambled {
		order = benchPerm(n)
	}
	nodes := make([]*config.Node, 0, n)
	tested := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	for i, k := range order {
		latency := int64(-1)
		if i%5 != 0 {
			latency = int64((i * 37) % 500)
		}
		nodes = append(nodes, &config.Node{
			ID:             int64(i + 1),
			SubscriptionID: 1,
			Name:           fmt.Sprintf("node-%05d", k),
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

// benchNodesMixed 与 benchNodes 规模、排列完全一致，只把名称与服务器改成**含大写**的形态。
//
// 存在的理由不是"更像真实数据"，而是 C8 的目标成本恰好被全小写夹具藏住了：
// strings.ToLower 在「没有大写字母」的 ASCII 串上**直接返回原串**（零分配、单趟扫描），
// 于是每按键 N×2 次 ToLower 在基准上看起来完全免费。真实订阅的节点名（"Hong Kong 01"）
// 与主机名普遍含大写，那才是 C8 要消掉的成本，所以这条基准必须单独一份夹具。
//
// 变换只加共享前缀/后缀与整体大写：保持原有相对顺序，让 scrambled=false 的版本
// 仍然是名称序（与 benchNodes 的语义对齐）。
func benchNodesMixed(n int, scrambled bool) []*config.Node {
	nodes := benchNodes(n, scrambled)
	for _, node := range nodes {
		node.Name = strings.ToUpper(node.Name)
		node.Server = "Relay." + node.Server + ".Example.ORG"
	}
	return nodes
}

// benchProfilesNodesFrom 让夹具来源与搜索词可替换，其余装配完全一致。
//
// 搜索词是独立参数而不是写死 "node"，是为了能构造「命中 0 条」的场景：
// 命中为空时 rebuildNodeTable 建不出任何行、分配近乎为 0，于是这条基准量的
// 就只剩过滤循环本身的扫描成本——这正是 C8 要消掉的那部分，不会被建行的
// 分配淹没。
func benchProfilesNodesFrom(b *testing.B, nodes int, scrambled bool, build func(int, bool) []*config.Node, query string) *Profiles {
	b.Helper()
	p := NewProfiles(benchApp(b))
	p.SetSize(140, 40)
	p.mode = profilesNodes
	p.nodes = build(nodes, scrambled)
	p.search = query
	p.resortNodes() // 装载路径的预排序（C7）：放在计时之外，基准只量按键成本
	p.applyNodeFilter()
	return p
}

func benchProfilesNodes(b *testing.B, nodes int, scrambled bool) *Profiles {
	b.Helper()
	p := benchProfilesNodesFrom(b, nodes, scrambled, benchNodes, "node")
	if len(p.filtered) == 0 {
		b.Fatal("过滤后无节点，基准失去意义")
	}
	return p
}

func benchProfilesNodesMixed(b *testing.B, nodes int, scrambled bool) *Profiles {
	b.Helper()
	p := benchProfilesNodesFrom(b, nodes, scrambled, benchNodesMixed, "node")
	if len(p.filtered) == 0 {
		b.Fatal("过滤后无节点，基准失去意义")
	}
	return p
}

// BenchmarkProfilesFilter 度量节点过滤 + 建列表项的成本。
//
// 夹具名称保持名称序，以便与 C-BENCH0 的基线（docs/bench-baseline.txt）逐项对比；
// 非有序输入下的按键成本见 BenchmarkProfilesFilterScrambled。
func BenchmarkProfilesFilter(b *testing.B) {
	for _, nodes := range []int{100, 1000, 5000, 10000} {
		b.Run(fmt.Sprintf("N%d", nodes), func(b *testing.B) {
			p := benchProfilesNodes(b, nodes, false)
			b.ReportAllocs()
			for b.Loop() {
				p.applyNodeFilter()
			}
		})
	}
}

// BenchmarkProfilesFilterScrambled 度量「搜索按键」在**名称无序**节点上的成本。
//
// 真实订阅返回的节点名不会是名称序，而旧实现的插入排序在无序输入上是 O(N²)
// 最坏情况，并且每个按键都重跑一次。C7 之后按键路径只剩 O(N) 过滤 + 建行，
// ns/op 应随 N 近似线性。
func BenchmarkProfilesFilterScrambled(b *testing.B) {
	for _, nodes := range []int{100, 1000, 5000, 10000} {
		b.Run(fmt.Sprintf("N%d", nodes), func(b *testing.B) {
			p := benchProfilesNodes(b, nodes, true)
			b.ReportAllocs()
			for b.Loop() {
				p.applyNodeFilter()
			}
		})
	}
}

// BenchmarkProfilesFilterMixedCase 度量「搜索按键」在**含大写**节点上的完整成本
// （过滤 + 建行），C8 前后可直接对比。
//
// 夹具必须含大写：ToLower 在无大写的 ASCII 串上直接返回原串，零分配，
// 用原夹具根本量不到 C8 要消掉的那笔分配（见 benchNodesMixed）。
func BenchmarkProfilesFilterMixedCase(b *testing.B) {
	for _, nodes := range []int{100, 1000, 5000, 10000} {
		b.Run(fmt.Sprintf("N%d", nodes), func(b *testing.B) {
			p := benchProfilesNodesMixed(b, nodes, true)
			b.ReportAllocs()
			for b.Loop() {
				p.applyNodeFilter()
			}
		})
	}
}

// BenchmarkProfilesFilterScanMixedCase 把「过滤循环本身」从建行里剥出来单独量：
// 搜索词命中 0 条，于是 rebuildNodeTable 没有任何行可建，剩余成本全是
// 「扫描 N 个节点 + 每节点对 Name/Server 各做一次 ToLower」。
//
// C8 之前约 N×2 allocs/op，之后应为 0——这是本项最干净的判据。
func BenchmarkProfilesFilterScanMixedCase(b *testing.B) {
	for _, nodes := range []int{100, 1000, 5000, 10000} {
		b.Run(fmt.Sprintf("N%d", nodes), func(b *testing.B) {
			p := benchProfilesNodesFrom(b, nodes, true, benchNodesMixed, "zzzz-no-such-node")
			if len(p.filtered) != 0 {
				b.Fatalf("夹具应当命中 0 条，实际 %d 条", len(p.filtered))
			}
			b.ReportAllocs()
			for b.Loop() {
				p.applyNodeFilter()
			}
		})
	}
}

// BenchmarkProfilesResortScrambled 度量「重排一次」本身的成本（装载、切换排序键、
// 测速回写触发，C7 把这三处收敛到唯一入口 resortNodes）。
//
// 该成本不再落在按键路径上，但它决定了每次 reload 的固定开销：C7 之前它是
// 插入排序（无序输入 O(N²)），之后换成稳定排序 O(N log N)。
func BenchmarkProfilesResortScrambled(b *testing.B) {
	for _, nodes := range []int{100, 1000, 5000, 10000} {
		b.Run(fmt.Sprintf("N%d", nodes), func(b *testing.B) {
			p := benchProfilesNodes(b, nodes, true)
			b.ReportAllocs()
			for b.Loop() {
				p.resortNodes()
			}
		})
	}
}

// BenchmarkProfilesView5000 度量节点页单帧渲染成本（含当前在 View 内重建的表格行）。
//
// C4 把建行移出 View、C12 收敛 viewport 之后，理想复杂度是 O(可见行)，
// allocs/op 应不随总行数线性增长。
func BenchmarkProfilesView5000(b *testing.B) {
	p := benchProfilesNodes(b, 5000, false)
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
