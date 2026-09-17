package migrate

import (
	"errors"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// C14-AUDIT 失败回滚验证：导入事务内任一写失败 → 整体回滚，不留半套导入数据。
// 注入点：txProbe（每次 DB 写之前以操作名调用）。

var errImportInjected = errors.New("injected import failure")

// seedImportBase 预置一个手动节点，验证「导入失败不影响既有数据」。
func seedImportBase(t *testing.T, db *storage.DB) {
	t.Helper()
	if err := db.CreateNode(&config.Node{Name: "既有节点", Protocol: "trojan",
		Server: "old.example.com", Port: 443, Enabled: true,
		Metadata: map[string]any{"password": "p"}}); err != nil {
		t.Fatalf("预置节点: %v", err)
	}
}

// countAll 统计库中各类对象数量，供回滚后比对。
type counts struct{ nodes, groups, routings int }

func countAll(t *testing.T, db *storage.DB) counts {
	t.Helper()
	nodes, err := db.ListNodes(0)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := db.ListProxyGroups()
	if err != nil {
		t.Fatal(err)
	}
	routings, err := db.ListRoutingGroups()
	if err != nil {
		t.Fatal(err)
	}
	return counts{len(nodes), len(groups), len(routings)}
}

// TestImportClashRollsBackOnMidwayFailure 在分流组写入处注入失败：
// 此前已写的节点与代理组必须一并回滚，既有数据原样保留。
func TestImportClashRollsBackOnMidwayFailure(t *testing.T) {
	db := setupDB(t)
	seedImportBase(t, db)
	before := countAll(t, db)

	txProbe = func(op string) error {
		if op == "CreateRoutingGroup" {
			return errImportInjected
		}
		return nil
	}
	defer func() { txProbe = nil }()

	_, err := ImportClash(db, clashSample)
	if !errors.Is(err, errImportInjected) {
		t.Fatalf("期望注入错误透出，得到 %v", err)
	}
	after := countAll(t, db)
	if after != before {
		t.Fatalf("导入回滚不干净：before=%+v after=%+v（写入泄漏）", before, after)
	}
}

// TestImportClashSucceedsAfterRollback 注入失败后清除探针再导入：
// 确认回滚不留脏数据（如残留的自增序、半截组名占用）。
func TestImportClashSucceedsAfterRollback(t *testing.T) {
	db := setupDB(t)
	seedImportBase(t, db)

	txProbe = func(op string) error {
		if op == "CreateRoutingGroup" {
			return errImportInjected
		}
		return nil
	}
	if _, err := ImportClash(db, clashSample); err == nil {
		t.Fatal("注入的导入应当失败")
	}
	txProbe = nil

	rep, err := ImportClash(db, clashSample)
	if err != nil {
		t.Fatalf("清除探针后导入失败: %v", err)
	}
	if rep.Nodes != 3 || rep.Groups != 3 {
		t.Fatalf("重导结果异常: %s", rep.Summary())
	}
}

// TestImportSingBoxRollsBackOnMidwayFailure 在代理组写入处注入失败：
// 此前已写的规则集与节点必须一并回滚。
func TestImportSingBoxRollsBackOnMidwayFailure(t *testing.T) {
	db := setupDB(t)
	seedImportBase(t, db)
	before := countAll(t, db)
	setsBefore, err := db.ListRuleSets()
	if err != nil {
		t.Fatal(err)
	}

	txProbe = func(op string) error {
		if op == "CreateProxyGroup" {
			return errImportInjected
		}
		return nil
	}
	defer func() { txProbe = nil }()

	if _, err := ImportSingBox(db, singboxSample); !errors.Is(err, errImportInjected) {
		t.Fatalf("期望注入错误透出，得到 %v", err)
	}
	after := countAll(t, db)
	if after != before {
		t.Fatalf("导入回滚不干净：before=%+v after=%+v（写入泄漏）", before, after)
	}
	setsAfter, err := db.ListRuleSets()
	if err != nil {
		t.Fatal(err)
	}
	if len(setsAfter) != len(setsBefore) {
		t.Fatalf("规则集回滚不干净：before=%d after=%d", len(setsBefore), len(setsAfter))
	}
}
