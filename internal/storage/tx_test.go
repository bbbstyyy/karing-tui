package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// C14：WithTx 统一事务封装 + 可写 DSN 的 _txlock=immediate（方案 A）验证。

// TestWithTxCommitAndRollback fn 正常返回则提交，返回错误则整体回滚。
func TestWithTxCommitAndRollback(t *testing.T) {
	db := newTestDB(t)
	g := &config.RoutingGroup{Name: "tx-seed", Target: "Auto", Kind: config.KindCustom, Enabled: true,
		Rules: []config.Rule{{Type: "domain", Value: "seed.com", Enabled: true}}}
	if err := db.CreateRoutingGroup(g); err != nil {
		t.Fatalf("CreateRoutingGroup: %v", err)
	}

	// 提交路径：事务内写入在 WithTx 返回后可见。
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO routing_groups (name, target, kind, kind_rank, position, enabled)
			 VALUES ('tx-commit', 'Auto', 'custom', 0, 99, 1)`)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx 提交路径: %v", err)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM routing_groups WHERE name='tx-commit'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("提交路径写入不可见，行数 = %d", n)
	}

	// 回滚路径：fn 返回错误 → 事务内写入不落库，错误原样透出。
	injected := errors.New("rollback probe")
	err = db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO routing_groups (name, target, kind, kind_rank, position, enabled)
			 VALUES ('tx-rollback', 'Auto', 'custom', 0, 98, 1)`); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("期望注入错误透出，得到 %v", err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM routing_groups WHERE name='tx-rollback'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("回滚路径写入泄漏，行数 = %d", n)
	}
}

// TestWritableDSNTxlockImmediate 方案 A 的机器可查说明：可写 DSN 显式带
// _txlock=immediate，即所有 writable BeginTx/Begin 都是 IMMEDIATE 事务。
func TestWritableDSNTxlockImmediate(t *testing.T) {
	if !strings.Contains(writableDSNPragmas, "_txlock=immediate") {
		t.Fatalf("可写 DSN 缺少 _txlock=immediate（C14 方案 A）：%s", writableDSNPragmas)
	}
}

// TestQueryOnlyDSNNoTxlock 只读打开路径不得继承 _txlock=immediate：
// query-only DSN 是显式白名单，_txlock 不在其中（TestQueryOnlyDSNWhitelist
// 的 allowedKeys 同样守护这条边界，这里做独立断言便于定位）。
func TestQueryOnlyDSNNoTxlock(t *testing.T) {
	if strings.Contains(queryOnlyDSNPragmas, "_txlock") {
		t.Fatalf("query-only DSN 不得包含 _txlock（C14）：%s", queryOnlyDSNPragmas)
	}
}

// TestEnsureDefaultGroupsAtomicOnFailure C14-AUDIT：第二个默认组创建失败时，
// 第一个（Auto）必须回滚——否则下次调用因 proxy_groups 非空短路，Manual 永久缺失。
func TestEnsureDefaultGroupsAtomicOnFailure(t *testing.T) {
	db := newTestDB(t)

	calls := 0
	proxyGroupCreateProbe = func() error {
		calls++
		if calls == 2 {
			return errors.New("injected second create failure")
		}
		return nil
	}
	defer func() { proxyGroupCreateProbe = nil }()

	if err := db.EnsureDefaultGroups(); err == nil {
		t.Fatal("期望注入错误透出")
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM proxy_groups`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("回滚不干净：proxy_groups 仍有 %d 行（Auto 泄漏会卡死后续补建）", n)
	}

	// 清除探针后重试：完整创建两组（证明空库状态得以恢复）。
	proxyGroupCreateProbe = nil
	if err := db.EnsureDefaultGroups(); err != nil {
		t.Fatalf("重试失败: %v", err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM proxy_groups`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("重试后 proxy_groups = %d 行, 期望 2", n)
	}
}

// TestWithTxOnQueryOnlyRejectedByQueryOnlyPragma 行为级验证：query-only 连接上
// BeginTx 仍是普通（deferred）事务，写入被 _query_only 拒绝且回滚后连接可用——
// 证明只读路径没有被 _txlock=immediate 波及。
func TestWithTxOnQueryOnlyRejectedByQueryOnlyPragma(t *testing.T) {
	// 同一个 KARING_HOME：先经 Open 完成初始化，关掉后以只读方式重开。
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	wdb, err := Open(paths)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := wdb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	qdb, err := OpenQueryOnly(paths)
	if err != nil {
		t.Fatalf("OpenQueryOnly: %v", err)
	}
	defer qdb.Close()

	tx, err := qdb.db.Begin()
	if err != nil {
		t.Fatalf("query-only BeginTx 应可用: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES ('probe', 'x')`); err == nil {
		t.Fatal("query-only 事务内写入应被 _query_only 拒绝")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	// 连接仍可用：只读查询正常，无残留锁。
	if err := qdb.db.QueryRow("PRAGMA user_version").Scan(new(int)); err != nil {
		t.Fatalf("回滚后连接不可用: %v", err)
	}
}

// TestSaveSettingsRollsBackOnMidwayFailure V7-4：应用设置的 9 个键必须同一事务提交。
// 中途失败时 LoadSettings() 必须逐字段等于调用前的 A——否则用户会拿到
// 「mixed_port 变了、clash_api_port 没变」这类混合配置，两个端口可能直接撞车。
func TestSaveSettingsRollsBackOnMidwayFailure(t *testing.T) {
	db := newTestDB(t)

	a := config.DefaultSettings()
	a.MixedPort, a.LogLevel, a.AutoUpdateMinutes = 1111, "debug", 7
	a.DownloadProxy, a.ClashAPIPort, a.ClashAPISecret = "http://127.0.0.1:1111", 1111, "secret-a"
	a.PrivateDirect, a.ResolveIPRules = false, true
	if err := db.SaveSettings(a); err != nil {
		t.Fatalf("预设 A: %v", err)
	}
	want, err := db.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}

	// 拦一个中间键。upsert 可能走 INSERT 也可能走 DO UPDATE 分支，两条都建。
	const blocked = "clash_api_secret"
	if _, err := db.db.Exec(`CREATE TRIGGER inject_secret_ins BEFORE INSERT ON settings
		WHEN NEW.key='` + blocked + `' BEGIN SELECT RAISE(ABORT, 'injected'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`CREATE TRIGGER inject_secret_upd BEFORE UPDATE ON settings
		WHEN NEW.key='` + blocked + `' BEGIN SELECT RAISE(ABORT, 'injected'); END;`); err != nil {
		t.Fatal(err)
	}

	b := config.DefaultSettings()
	b.MixedPort, b.LogLevel, b.AutoUpdateMinutes = 2222, "warn", 9
	b.DownloadProxy, b.ClashAPIPort, b.ClashAPISecret = "http://127.0.0.1:2222", 2222, "secret-b"
	b.PrivateDirect, b.ResolveIPRules = true, false
	if err := db.SaveSettings(b); err == nil {
		t.Fatal("注入失败后 SaveSettings 应返回错误")
	}

	got, err := db.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("中途失败必须整体回滚\n want=%+v\n got =%+v", want, got)
	}
}

// TestSaveSettingsRoundTripsEveryField 反向核对：每个字段都取「与默认值不同」的值，
// 于是任何一个字段没被持久化都会在读回时暴露成默认值。没有这条，
// 上面那条回滚测试可能因为「字段根本没写」而假通过。
func TestSaveSettingsRoundTripsEveryField(t *testing.T) {
	db := newTestDB(t)
	want := config.Settings{
		MixedPort:         1234,                      // 默认 2080
		AllowLAN:          true,                      // 默认 false
		DownloadProxy:     " http://127.0.0.1:3067 ", // SaveSettings 会 trim
		LogLevel:          "trace",                   // 默认 info
		ClashAPIPort:      8080,                      // 默认 9090
		ClashAPISecret:    "s3cret",                  // 默认空
		PrivateDirect:     false,                     // 默认 true
		ResolveIPRules:    true,                      // 默认 false
		AutoUpdateMinutes: 42,                        // 默认 0
	}
	if err := db.SaveSettings(want); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	want.DownloadProxy = strings.TrimSpace(want.DownloadProxy)
	if got != want {
		t.Errorf("设置未逐字段往返\n want=%+v\n got =%+v", want, got)
	}
}

// TestReplaceSubscriptionNodesRollsBackWhenTrafficUpdateFails V7-10：
// 流量元数据写失败时，整次订阅刷新（节点替换 + 订阅状态 + 组引用清理）必须一起回滚。
// 旧实现把 traffic 当成独立写并在调用方吞错，于是「节点已换、流量未更新」也报成功。
func TestReplaceSubscriptionNodesRollsBackWhenTrafficUpdateFails(t *testing.T) {
	db := newTestDB(t)
	s := &config.Subscription{Name: "traffic", URL: "https://example.com"}
	if err := db.CreateSubscription(s); err != nil {
		t.Fatal(err)
	}
	old := &config.Node{Name: "old", Protocol: "trojan", Server: "example.com", Port: 443, Enabled: true,
		Metadata: map[string]any{"password": "p"}, SubscriptionID: s.ID}
	if err := db.CreateNode(old); err != nil {
		t.Fatal(err)
	}
	// 组引用：回滚不彻底时这条引用会先被 ReplaceSubscriptionNodes 清掉。
	g := &config.ProxyGroup{Name: "显式组", Type: "select",
		Members: []config.ProxyGroupMember{{Type: "node", ID: old.ID}}, Selected: fmt.Sprintf("node:%d", old.ID)}
	if err := db.CreateProxyGroup(g); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProxyGroupSelected(g.ID, g.Selected); err != nil {
		t.Fatal(err)
	}
	before, err := db.GetSubscription(s.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.db.Exec(`CREATE TRIGGER inject_traffic BEFORE UPDATE ON subscriptions
		WHEN NEW.traffic_total <> OLD.traffic_total BEGIN SELECT RAISE(ABORT, 'injected'); END;`); err != nil {
		t.Fatal(err)
	}

	fresh := &config.Node{ID: old.ID, Name: "new", Protocol: "trojan", Server: "example.com", Port: 443,
		Enabled: true, Metadata: map[string]any{"password": "p"}, SubscriptionID: s.ID}
	meta := SubscriptionRefreshMeta{HasTraffic: true, Upload: 1, Download: 2, Total: 999}
	if err := db.ReplaceSubscriptionNodesWithMeta(s.ID, []*config.Node{fresh}, time.Now(), meta); err == nil {
		t.Fatal("traffic 写入失败时替换必须报错")
	}

	nodes, err := db.ListNodes(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].ID != old.ID || nodes[0].Name != "old" {
		t.Errorf("回滚后旧节点应原样保留，实得 %+v", nodes)
	}
	after, err := db.GetSubscription(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.NodeCount != before.NodeCount {
		t.Errorf("node_count 不应变化: %d -> %d", before.NodeCount, after.NodeCount)
	}
	if !after.LastUpdated.Equal(before.LastUpdated) {
		t.Errorf("last_updated 不应变化: %v -> %v", before.LastUpdated, after.LastUpdated)
	}
	if after.TrafficTotal != before.TrafficTotal {
		t.Errorf("traffic 不应被写入: %d -> %d", before.TrafficTotal, after.TrafficTotal)
	}
	got, err := db.GetProxyGroup(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 1 || got.Members[0].ID != old.ID || got.Selected != g.Selected {
		t.Errorf("回滚后组引用应原样保留，实得 %+v", got)
	}
}
