package storage

import (
	"database/sql"
	"net/url"
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// legacyV7DB 造一个 v8 之前的老库（含旧版「4 组 · 5 个规则集」默认方案），
// 供迁移测试使用。返回的 *sql.DB 直接操作原始文件，未经 storage.Open。
func legacyV7DB(t *testing.T, paths *platform.Paths) *sql.DB {
	t.Helper()
	dsn := (&url.URL{Scheme: "file", Path: paths.DB}).String() +
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_time_format=sqlite"
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("打开老库: %v", err)
	}
	// 依次应用 v1..v7（v8 是本次要验证的迁移，留给 storage.Open）
	for i := 0; i < 7; i++ {
		for _, stmt := range migrations[i] {
			if _, err := raw.Exec(stmt); err != nil {
				raw.Close()
				t.Fatalf("应用迁移 v%d: %v", i+1, err)
			}
		}
	}
	if _, err := raw.Exec(`PRAGMA user_version = 7`); err != nil {
		raw.Close()
		t.Fatalf("写入 user_version: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO proxy_groups (name, type, test_url, interval_s) VALUES ('Auto','urltest','',1), ('Manual','select','',0)`,
	); err != nil {
		raw.Close()
		t.Fatalf("预置代理组: %v", err)
	}
	// 旧方案：4 组，position 全部为 0（靠 id 决胜），最后一组含活动 final 规则
	legacy := []struct {
		name   string
		target string
		rule   string
	}{
		{"中国大陆", "DIRECT", "geosite:cn"},
		{"Telegram", "Auto", "geoip:telegram"},
		{"AI", "Auto", "geosite:category-ai-!cn"},
		{"Final", "Auto", "final"},
	}
	for _, g := range legacy {
		res, err := raw.Exec(
			`INSERT INTO routing_groups (name, target, position, enabled) VALUES (?, ?, 0, 1)`,
			g.name, g.target)
		if err != nil {
			raw.Close()
			t.Fatalf("写入老分流组 %q: %v", g.name, err)
		}
		id, _ := res.LastInsertId()
		value := g.rule
		typ := "rule_set"
		if g.rule == "final" {
			typ, value = "final", ""
		}
		if _, err := raw.Exec(
			`INSERT INTO rules (routing_group_id, rule_type, value, mode, invert, enabled, position)
			 VALUES (?, ?, ?, '', 0, 1, 0)`, id, typ, value); err != nil {
			raw.Close()
			t.Fatalf("写入老规则 %q: %v", g.rule, err)
		}
	}
	return raw
}

// legacyGroupsByOldOrder 按**迁移前的排序键** (position, id) 读出分组，用于生成
// 「老版本会产出什么 route」的对照快照。Kind 全空 = 老模型没有层概念。
func legacyGroupsByOldOrder(t *testing.T, raw *sql.DB) []*config.RoutingGroup {
	t.Helper()
	rows, err := raw.Query(`SELECT id, name, target, enabled FROM routing_groups ORDER BY position, id`)
	if err != nil {
		t.Fatalf("读取老分组: %v", err)
	}
	defer rows.Close()
	var out []*config.RoutingGroup
	for rows.Next() {
		var g config.RoutingGroup
		var enabled int
		if err := rows.Scan(&g.ID, &g.Name, &g.Target, &enabled); err != nil {
			t.Fatalf("扫描老分组: %v", err)
		}
		g.Enabled = enabled != 0
		out = append(out, &g)
	}
	for _, g := range out {
		rules, err := raw.Query(
			`SELECT id, rule_type, value, mode, invert, enabled, position FROM rules
			 WHERE routing_group_id=? ORDER BY position, id`, g.ID)
		if err != nil {
			t.Fatalf("读取老规则: %v", err)
		}
		for rules.Next() {
			var r config.Rule
			var invert, enabled int
			if err := rules.Scan(&r.ID, &r.Type, &r.Value, &r.Mode, &invert, &enabled, &r.Position); err != nil {
				rules.Close()
				t.Fatalf("扫描老规则: %v", err)
			}
			r.Invert = invert != 0
			r.Enabled = enabled != 0
			g.Rules = append(g.Rules, r)
		}
		rules.Close()
	}
	return out
}

func legacySnapshot(groups []*config.RoutingGroup) config.Snapshot {
	set := config.DefaultSettings()
	return config.Snapshot{
		Settings: set,
		// C15 起「没有任何可用节点」会 fail-closed，而本测试只关心层序迁移前后
		// 生成的 route 是否逐条一致，因此需要给出一个可用节点 + 可解析的默认组。
		Nodes: []*config.Node{{
			ID: 1, Name: "n1", Protocol: "trojan", Server: "1.1.1.1", Port: 443,
			Enabled: true, Metadata: map[string]any{"password": "p"},
		}},
		ProxyGroups: []*config.ProxyGroup{{ID: 1, Name: "Auto", Type: "urltest",
			Members: []config.ProxyGroupMember{{Type: "all"}}}},
		RoutingGroups: groups,
	}
}

// TestMigrationBackfillsRoutingLayers 老库（4 组）升级后：final 组落到 final 层、
// 其余落 custom 层，层内 position 重排为连续值，残留的 final 规则被清理。
func TestMigrationBackfillsRoutingLayers(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	raw := legacyV7DB(t, paths)
	raw.Close()

	db, err := Open(paths)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	defer db.Close()

	var version int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读取 user_version: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("迁移后 schema 版本 = %d, 期望 %d", version, len(migrations))
	}

	groups, err := db.ListRoutingGroups()
	if err != nil {
		t.Fatalf("ListRoutingGroups: %v", err)
	}
	if len(groups) != 4 {
		t.Fatalf("分组数 = %d, 期望 4", len(groups))
	}
	// 顺序：custom 层 3 个（按迁移前 (position,id) = insertion 顺序）→ final 层
	wantOrder := []string{"中国大陆", "Telegram", "AI", "Final"}
	for i, g := range groups {
		if g.Name != wantOrder[i] {
			t.Errorf("第 %d 个分组 = %q, 期望 %q", i, g.Name, wantOrder[i])
		}
	}
	for i, g := range groups[:3] {
		if g.Kind != config.KindCustom {
			t.Errorf("%q.Kind = %q, 期望 custom", g.Name, g.Kind)
		}
		if g.Position != i {
			t.Errorf("%q.Position = %d, 期望 %d（层内连续）", g.Name, g.Position, i)
		}
	}
	final := groups[3]
	if final.Kind != config.KindFinal || final.Position != 0 {
		t.Errorf("Final 组 = kind %q position %d, 期望 final/0", final.Kind, final.Position)
	}
	if len(final.Rules) != 1 || final.Rules[0].Type != "final" {
		t.Errorf("Final 组规则 = %+v, 期望唯一一条 final", final.Rules)
	}
	// kind_rank 冗余列与 KindRank 一致，且确实参与排序
	for _, g := range groups {
		var rank int
		if err := db.db.QueryRow(`SELECT kind_rank FROM routing_groups WHERE id=?`, g.ID).Scan(&rank); err != nil {
			t.Fatalf("读取 kind_rank: %v", err)
		}
		if rank != config.KindRank(g.Kind) {
			t.Errorf("%q.kind_rank = %d, 期望 %d", g.Name, rank, config.KindRank(g.Kind))
		}
	}
}

// TestMigrationKeepsRouteIdentical 本 Phase 最关键的回归门：分层迁移前后，生成器
// 产出的 route 必须**逐条一致**（golden 对比）。若不一致，说明层序实现引入了非预期
// 的优先级变化，必须先修模型而不是调数据。
func TestMigrationKeepsRouteIdentical(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	raw := legacyV7DB(t, paths)
	defer raw.Close()

	before, err := config.Generate(legacySnapshot(legacyGroupsByOldOrder(t, raw)))
	if err != nil {
		t.Fatalf("生成迁移前配置: %v", err)
	}
	raw.Close()

	db, err := Open(paths)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	defer db.Close()
	groups, err := db.ListRoutingGroups()
	if err != nil {
		t.Fatalf("ListRoutingGroups: %v", err)
	}
	after, err := config.Generate(legacySnapshot(groups))
	if err != nil {
		t.Fatalf("生成迁移后配置: %v", err)
	}
	if string(before) != string(after) {
		i := 0
		for i < len(before) && i < len(after) && before[i] == after[i] {
			i++
		}
		lo := i - 80
		if lo < 0 {
			lo = 0
		}
		hiB, hiA := i+80, i+80
		if hiB > len(before) {
			hiB = len(before)
		}
		if hiA > len(after) {
			hiA = len(after)
		}
		t.Fatalf("迁移前后生成的配置不一致（位置 %d）:\n  迁移前: ...%s...\n  迁移后: ...%s...",
			i, before[lo:hiB], after[lo:hiA])
	}
}

// TestMigrationAbortsOnMultipleActiveFinal 坏数据必须中止迁移并列出组名，
// 不得自行挑一个（那会让用户的分流方案静默改变）。
func TestMigrationAbortsOnMultipleActiveFinal(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	raw := legacyV7DB(t, paths)
	// 再造一个含活动 final 规则的组：老库允许（生成器与校验只拦新写入）
	res, err := raw.Exec(`INSERT INTO routing_groups (name, target, position, enabled) VALUES ('Final 2','Auto',0,1)`)
	if err != nil {
		t.Fatalf("写入坏数据: %v", err)
	}
	id, _ := res.LastInsertId()
	if _, err := raw.Exec(
		`INSERT INTO rules (routing_group_id, rule_type, value, mode, invert, enabled, position) VALUES (?, 'final','','',0,1,0)`, id); err != nil {
		t.Fatalf("写入坏 final 规则: %v", err)
	}
	raw.Close()

	db, err := Open(paths)
	if err == nil {
		db.Close()
		t.Fatal("含两个活动 final 规则的老库应中止迁移")
	}
	for _, want := range []string{"2", "Final", "Final 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q: %v", want, err)
		}
	}
}

// TestMigrationIdempotentRoutingLayers 已是新 schema 或重复迁移应跳过，无副作用。
func TestMigrationIdempotentRoutingLayers(t *testing.T) {
	db := newTestDB(t)
	g := &config.RoutingGroup{Name: "g1", Target: "Auto", Kind: config.KindGeoIP, Position: 5, Enabled: true,
		Rules: []config.Rule{{Type: "domain", Value: "a.com", Enabled: true}}}
	if err := db.CreateRoutingGroup(g); err != nil {
		t.Fatalf("CreateRoutingGroup: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := db.migrate(); err != nil {
			t.Fatalf("重复迁移: %v", err)
		}
	}
	got, err := db.GetRoutingGroup(g.ID)
	if err != nil {
		t.Fatalf("GetRoutingGroup: %v", err)
	}
	// 重复迁移不得改写既有层与层内序号
	if got.Kind != config.KindGeoIP || got.Position != 5 {
		t.Errorf("重复迁移改动了数据: kind=%q position=%d", got.Kind, got.Position)
	}
}

// TestSetRoutingGroupPlacement 定位列更新：kind/kind_rank/position 一并落库。
func TestSetRoutingGroupPlacement(t *testing.T) {
	db := newTestDB(t)
	g := &config.RoutingGroup{Name: "g1", Target: "Auto", Kind: config.KindCustom, Position: 0, Enabled: true,
		Rules: []config.Rule{{Type: "domain", Value: "a.com", Enabled: true}}}
	if err := db.CreateRoutingGroup(g); err != nil {
		t.Fatalf("CreateRoutingGroup: %v", err)
	}
	if err := db.SetRoutingGroupPlacement(g.ID, config.KindACL, 3); err != nil {
		t.Fatalf("SetRoutingGroupPlacement: %v", err)
	}
	got, err := db.GetRoutingGroup(g.ID)
	if err != nil {
		t.Fatalf("GetRoutingGroup: %v", err)
	}
	if got.Kind != config.KindACL || got.Position != 3 {
		t.Errorf("定位列 = kind %q position %d, 期望 acl/3", got.Kind, got.Position)
	}
	// 规则不应被触碰
	if len(got.Rules) != 1 || got.Rules[0].Value != "a.com" {
		t.Errorf("规则被意外改动: %+v", got.Rules)
	}
	var rank int
	if err := db.db.QueryRow(`SELECT kind_rank FROM routing_groups WHERE id=?`, g.ID).Scan(&rank); err != nil {
		t.Fatalf("读取 kind_rank: %v", err)
	}
	if rank != config.KindRank(config.KindACL) {
		t.Errorf("kind_rank = %d, 期望 %d", rank, config.KindRank(config.KindACL))
	}
	// 空 Kind 落库时归一为 custom
	other := &config.RoutingGroup{Name: "g2", Target: "Auto", Enabled: true}
	if err := db.CreateRoutingGroup(other); err != nil {
		t.Fatalf("CreateRoutingGroup(空 Kind): %v", err)
	}
	if err := db.SetRoutingGroupPlacement(other.ID, "", 0); err != nil {
		t.Fatalf("SetRoutingGroupPlacement(空 Kind): %v", err)
	}
	var kind string
	if err := db.db.QueryRow(`SELECT kind FROM routing_groups WHERE id=?`, other.ID).Scan(&kind); err != nil {
		t.Fatalf("读取 kind: %v", err)
	}
	if kind != config.KindCustom {
		t.Errorf("空 Kind 应落库为 custom, 实得 %q", kind)
	}
}

func TestLayerBackfillFinalRuleCleanup(t *testing.T) {
	// 非 final 组里残留的（停用）final 规则会被清理：引入 Kind 后它构成非法数据，
	// 且它本就不产出任何路由规则。
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	raw := legacyV7DB(t, paths)
	// 活动 final 组标记为停用，并在另一个组塞一条**停用**的 final 规则
	if _, err := raw.Exec(`UPDATE rules SET enabled=0 WHERE rule_type='final'`); err != nil {
		t.Fatalf("停用 final: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO rules (routing_group_id, rule_type, value, mode, invert, enabled, position)
		 SELECT id, 'final', '', '', 0, 0, 1 FROM routing_groups WHERE name='中国大陆'`); err != nil {
		t.Fatalf("插入停用 final 规则: %v", err)
	}
	raw.Close()

	db, err := Open(paths)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	defer db.Close()
	var orphans int
	if err := db.db.QueryRow(
		`SELECT COUNT(*) FROM rules r JOIN routing_groups g ON g.id=r.routing_group_id
		 WHERE r.rule_type='final' AND g.kind<>'final'`).Scan(&orphans); err != nil {
		t.Fatalf("统计残留 final 规则: %v", err)
	}
	if orphans != 0 {
		t.Errorf("非 final 组里仍残留 %d 条 final 规则", orphans)
	}
}
