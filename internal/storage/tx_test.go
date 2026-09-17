package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// C14：WithTx 统一事务封装 + 可写 DSN 的 _txlock=immediate（方案 A）验证。

var errWithTxInjected = errors.New("injected WithTx failure")

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
