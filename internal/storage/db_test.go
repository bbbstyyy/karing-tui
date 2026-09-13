package storage

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	db, err := Open(paths)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateCreatesTables(t *testing.T) {
	db := newTestDB(t)

	var version int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读取 user_version: %v", err)
	}
	if version != len(migrations) {
		t.Errorf("schema 版本 = %d, 期望 %d", version, len(migrations))
	}

	wantTables := []string{
		"subscriptions", "nodes", "proxy_groups", "proxy_group_members",
		"routing_groups", "rules", "rulesets", "dns_servers", "settings",
	}
	for _, table := range wantTables {
		var name string
		err := db.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("表 %s 不存在: %v", table, err)
		}
	}
}

func TestMigrateIdempotent(t *testing.T) {
	db := newTestDB(t)

	// 重复执行迁移不应报错、不应改变版本号。
	if err := db.migrate(); err != nil {
		t.Fatalf("重复迁移: %v", err)
	}
	var version int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读取 user_version: %v", err)
	}
	if version != len(migrations) {
		t.Errorf("重复迁移后版本 = %d, 期望 %d", version, len(migrations))
	}
}

func TestConcurrentOpenSerializesMigration(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			db, err := Open(paths)
			if db != nil {
				_ = db.Close()
			}
			errs <- err
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("并发 Open 失败: %v", err)
		}
	}
}

func TestSettingsKV(t *testing.T) {
	db := newTestDB(t)

	// 读不存在的键返回空串而非错误。
	v, err := db.GetSetting("missing")
	if err != nil || v != "" {
		t.Errorf("GetSetting(missing) = (%q, %v), 期望 (\"\", nil)", v, err)
	}

	if err := db.SetSetting("core.mode", "external"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	// upsert 覆盖
	if err := db.SetSetting("core.mode", "embedded"); err != nil {
		t.Fatalf("SetSetting 覆盖: %v", err)
	}
	v, err = db.GetSetting("core.mode")
	if err != nil || v != "embedded" {
		t.Errorf("GetSetting = (%q, %v), 期望 (\"embedded\", nil)", v, err)
	}

	all, err := db.AllSettings()
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if all["core.mode"] != "embedded" {
		t.Errorf("AllSettings[core.mode] = %q", all["core.mode"])
	}

	if err := db.DeleteSetting("core.mode"); err != nil {
		t.Fatalf("DeleteSetting: %v", err)
	}
	v, _ = db.GetSetting("core.mode")
	if v != "" {
		t.Errorf("删除后 GetSetting = %q, 期望空", v)
	}
}

// TestMigrateFromV3 手工构建 v3 schema 的旧库并写入数据，用当前代码打开后
// 应迁移到 v4：旧数据保留、新列/新表可用（逻辑规则）。
func TestMigrateFromV3(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}

	// 手动应用 v1–v3 迁移，模拟旧版本创建的数据库
	old, err := sql.Open("sqlite", "file:"+paths.DB+"?_pragma=journal_mode(WAL)&_time_format=sqlite")
	if err != nil {
		t.Fatalf("打开旧库: %v", err)
	}
	for i, m := range migrations[:3] {
		for _, stmt := range m {
			if _, err := old.Exec(stmt); err != nil {
				t.Fatalf("应用 v%d 迁移: %v", i+1, err)
			}
		}
		if _, err := old.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			t.Fatalf("写入版本 v%d: %v", i+1, err)
		}
	}
	if _, err := old.Exec(`INSERT INTO routing_groups (name, target, position, enabled)
		VALUES ('旧分流组', 'DIRECT', 0, 1)`); err != nil {
		t.Fatalf("插入旧分流组: %v", err)
	}
	if _, err := old.Exec(`INSERT INTO rules (routing_group_id, rule_type, value, invert, enabled, position)
		VALUES (1, 'domain_suffix', 'example.com', 0, 1, 0)`); err != nil {
		t.Fatalf("插入旧规则: %v", err)
	}
	old.Close()

	// 当前代码打开 → 自动迁移到 v4
	db, err := Open(paths)
	if err != nil {
		t.Fatalf("Open（应完成迁移）: %v", err)
	}
	defer db.Close()

	var version int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Errorf("迁移后 user_version = %d, 期望 %d", version, len(migrations))
	}

	// 旧数据保留
	rgs, err := db.ListRoutingGroups()
	if err != nil || len(rgs) != 1 {
		t.Fatalf("旧分流组应保留: %d 组, %v", len(rgs), err)
	}
	if rgs[0].Name != "旧分流组" || len(rgs[0].Rules) != 1 || rgs[0].Rules[0].Value != "example.com" {
		t.Errorf("旧规则数据不符: %+v", rgs[0])
	}

	// v4 能力可用：写入逻辑规则
	rg := &config.RoutingGroup{
		Name: "新逻辑组", Target: "DIRECT", Enabled: true,
		Rules: []config.Rule{{
			Type: "logical", Mode: "and", Enabled: true,
			Conditions: []config.RuleCondition{{Type: "domain_suffix", Value: "a.com"}},
		}},
	}
	if err := db.CreateRoutingGroup(rg); err != nil {
		t.Errorf("迁移后应可写入逻辑规则: %v", err)
	}
}

// TestMigrateFromV4WithOrphanRows 库里存在孤儿行时 v5 迁移仍须成功：外部工具在
// foreign_keys=OFF 下删除分流组会留下 routing_group_id 悬空的规则，若不清理，
// 重建 rules 时的外键校验会让迁移失败、应用无法启动。
func TestMigrateFromV4WithOrphanRows(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}

	// 不带 foreign_keys pragma，模拟外部工具的连接
	old, err := sql.Open("sqlite", "file:"+paths.DB+"?_pragma=journal_mode(WAL)&_time_format=sqlite")
	if err != nil {
		t.Fatalf("打开旧库: %v", err)
	}
	for i, m := range migrations[:4] {
		for _, stmt := range m {
			if _, err := old.Exec(stmt); err != nil {
				t.Fatalf("应用 v%d 迁移: %v", i+1, err)
			}
		}
		if _, err := old.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			t.Fatalf("写入版本 v%d: %v", i+1, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO routing_groups (name, target, position, enabled) VALUES ('保留组', 'DIRECT', 0, 1)`,
		`INSERT INTO routing_groups (name, target, position, enabled) VALUES ('待删组', 'BLOCK', 1, 1)`,
		`INSERT INTO rules (routing_group_id, rule_type, value, mode, invert, enabled, position)
		 VALUES (1, 'domain_suffix', 'keep.com', '', 0, 1, 0)`,
		`INSERT INTO rules (routing_group_id, rule_type, value, mode, invert, enabled, position)
		 VALUES (2, 'logical', '', 'and', 0, 1, 0)`,
		`INSERT INTO rule_conditions (rule_id, cond_type, value, invert, position)
		 VALUES (2, 'domain', 'orphan.com', 0, 0)`,
		// 外键关闭下删除分流组 → 规则 2 及其子条件成为孤儿
		`DELETE FROM routing_groups WHERE id = 2`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("写入 v4 数据: %v\n语句: %s", err, stmt)
		}
	}
	old.Close()

	db, err := Open(paths)
	if err != nil {
		t.Fatalf("含孤儿行的库应能完成迁移: %v", err)
	}
	defer db.Close()

	rgs, err := db.ListRoutingGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(rgs) != 1 || rgs[0].Name != "保留组" {
		t.Fatalf("正常分流组应保留: %+v", rgs)
	}
	if len(rgs[0].Rules) != 1 || rgs[0].Rules[0].Value != "keep.com" {
		t.Errorf("正常规则应保留: %+v", rgs[0].Rules)
	}
	// 孤儿规则与其子条件被丢弃
	var rules, conds int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM rules`).Scan(&rules); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM rule_conditions`).Scan(&conds); err != nil {
		t.Fatal(err)
	}
	if rules != 1 || conds != 0 {
		t.Errorf("孤儿数据应被清理: rules=%d（期望 1）, rule_conditions=%d（期望 0）", rules, conds)
	}
}

// TestMigrateFromV4 手工构建 v4 schema 的旧库（含逻辑规则子条件）并迁移到 v5。
// 重点：v5 要重建 rules 表，而 rule_conditions 通过外键 ON DELETE CASCADE 引用它——
// 若直接 DROP TABLE rules，foreign_keys=ON 下的隐式 DELETE FROM 会连带清空子条件。
func TestMigrateFromV4(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}

	old, err := sql.Open("sqlite", "file:"+paths.DB+"?_pragma=journal_mode(WAL)&_time_format=sqlite")
	if err != nil {
		t.Fatalf("打开旧库: %v", err)
	}
	for i, m := range migrations[:4] {
		for _, stmt := range m {
			if _, err := old.Exec(stmt); err != nil {
				t.Fatalf("应用 v%d 迁移: %v", i+1, err)
			}
		}
		if _, err := old.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			t.Fatalf("写入版本 v%d: %v", i+1, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO routing_groups (name, target, position, enabled) VALUES ('v4 组', 'DIRECT', 0, 1)`,
		`INSERT INTO rules (routing_group_id, rule_type, value, mode, invert, enabled, position)
		 VALUES (1, 'logical', '', 'and', 0, 1, 0)`,
		`INSERT INTO rule_conditions (rule_id, cond_type, value, invert, position)
		 VALUES (1, 'domain_suffix', 'a.com', 0, 0)`,
		`INSERT INTO rule_conditions (rule_id, cond_type, value, invert, position)
		 VALUES (1, 'geoip', 'cn', 1, 1)`,
		`INSERT INTO rules (routing_group_id, rule_type, value, mode, invert, enabled, position)
		 VALUES (1, 'domain_keyword', 'example', '', 0, 1, 1)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("写入 v4 数据: %v\n语句: %s", err, stmt)
		}
	}
	old.Close()

	db, err := Open(paths)
	if err != nil {
		t.Fatalf("Open（应完成迁移）: %v", err)
	}
	defer db.Close()

	var version int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Errorf("迁移后 user_version = %d, 期望 %d", version, len(migrations))
	}

	rgs, err := db.ListRoutingGroups()
	if err != nil || len(rgs) != 1 {
		t.Fatalf("旧分流组应保留: %d 组, %v", len(rgs), err)
	}
	if len(rgs[0].Rules) != 2 {
		t.Fatalf("旧规则应保留 2 条，得到 %d: %+v", len(rgs[0].Rules), rgs[0].Rules)
	}
	// 子条件必须完整存活（CASCADE 保护的核心断言）
	logical := rgs[0].Rules[0]
	if logical.Type != "logical" || logical.Mode != "and" {
		t.Errorf("逻辑规则字段不符: %+v", logical)
	}
	if len(logical.Conditions) != 2 {
		t.Fatalf("子条件应保留 2 条，得到 %d: %+v", len(logical.Conditions), logical.Conditions)
	}
	if logical.Conditions[0].Type != "domain_suffix" || logical.Conditions[0].Value != "a.com" {
		t.Errorf("子条件[0] 不符: %+v", logical.Conditions[0])
	}
	if logical.Conditions[1].Type != "geoip" || logical.Conditions[1].Value != "cn" || !logical.Conditions[1].Invert {
		t.Errorf("子条件[1] 不符（含 invert）: %+v", logical.Conditions[1])
	}

	// v5 能力可用：domain_regex 作为普通规则与逻辑子条件均可写入
	rg := &config.RoutingGroup{
		Name: "正则组", Target: "DIRECT", Enabled: true,
		Rules: []config.Rule{
			{Type: "domain_regex", Value: `^ads\..*\.com$`, Enabled: true},
			{Type: "logical", Mode: "or", Enabled: true, Conditions: []config.RuleCondition{
				{Type: "domain_regex", Value: `^cdn[0-9]+\.example\.com$`},
			}},
		},
	}
	if err := db.CreateRoutingGroup(rg); err != nil {
		t.Fatalf("迁移后应可写入 domain_regex 规则: %v", err)
	}
	got, err := db.ListRoutingGroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if g.Name != "正则组" {
			continue
		}
		if g.Rules[0].Value != `^ads\..*\.com$` {
			t.Errorf("domain_regex 值回读不符: %q", g.Rules[0].Value)
		}
		if len(g.Rules[1].Conditions) != 1 || g.Rules[1].Conditions[0].Type != "domain_regex" {
			t.Errorf("domain_regex 子条件回读不符: %+v", g.Rules[1].Conditions)
		}
	}
}
