package dns

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// V7-3：DNS 的三条多步写路径（EnsureDefaultDNS / SaveOptions / MoveRule）必须各自
// 在一个事务里完成。故障用 SQLite trigger 注入——它不需要给实现留可替换调用缝，
// 也比「磁盘满」这类条件稳定得多：失败点精确落在指定的那一条 SQL 上。

// newTxTestManager 额外返回一个直连同一数据库文件的 *sql.DB。
// storage.DB 不对外暴露执行任意 SQL 的入口，而 trigger 是 schema 对象，
// 用另一条连接创建后对 storage 的写入同样生效。
func newTxTestManager(t *testing.T) (*Manager, *sql.DB) {
	t.Helper()
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	raw, err := sql.Open("sqlite", paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return NewManager(db, nil), raw
}

func mustExec(t *testing.T, raw *sql.DB, stmt string) {
	t.Helper()
	if _, err := raw.Exec(stmt); err != nil {
		t.Fatalf("执行注入语句失败: %v\n%s", err, stmt)
	}
}

func countRows(t *testing.T, raw *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("计数 %s: %v", table, err)
	}
	return n
}

type dnsOptionsSnapshot struct {
	Strategy    string
	FakeIPOn    string
	FakeIPRange string
	Final       string
}

func readOptions(t *testing.T, m *Manager) dnsOptionsSnapshot {
	t.Helper()
	get := func(key string) string {
		v, err := m.DB.GetSetting(key)
		if err != nil {
			t.Fatalf("读取设置 %s: %v", key, err)
		}
		return v
	}
	return dnsOptionsSnapshot{
		Strategy:    get(keyStrategy),
		FakeIPOn:    get(keyFakeIPOn),
		FakeIPRange: get(keyFakeIPRange),
		Final:       get(keyFinal),
	}
}

func rulePositions(t *testing.T, m *Manager) map[string]int {
	t.Helper()
	rules, err := m.DB.ListDNSRules()
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]int, len(rules))
	for _, r := range rules {
		out[r.Value] = r.Position
	}
	return out
}

// 初始化默认 DNS 时 remote 写入失败：不得留下「只有 local」的残片，
// 否则下次启动会因为 len(servers)>0 而永久固化这份半成品。
func TestEnsureDefaultDNSRollsBackOnRemoteInsertFailure(t *testing.T) {
	m, raw := newTxTestManager(t)
	mustExec(t, raw, `CREATE TRIGGER inject_remote BEFORE INSERT ON dns_servers
		WHEN NEW.tag='remote' BEGIN SELECT RAISE(ABORT, 'injected'); END;`)

	if err := m.EnsureDefaultDNS(); err == nil {
		t.Fatal("remote 写入已被注入失败，EnsureDefaultDNS 仍返回成功")
	}
	if got := countRows(t, raw, "dns_servers"); got != 0 {
		t.Errorf("回滚后 dns_servers 应为 0 行，实得 %d", got)
	}
	if got := countRows(t, raw, "dns_rules"); got != 0 {
		t.Errorf("回滚后 dns_rules 应为 0 行，实得 %d", got)
	}
	if v, err := m.DB.GetSetting(keyFinal); err != nil || v != "" {
		t.Errorf("回滚后 dns_final 不应存在，实得 %q err=%v", v, err)
	}
}

// local/remote 都写成功、默认规则写入失败：三张表必须一起回到调用前。
func TestEnsureDefaultDNSRollsBackOnRuleFailure(t *testing.T) {
	m, raw := newTxTestManager(t)
	mustExec(t, raw, `CREATE TRIGGER inject_rule BEFORE INSERT ON dns_rules
		BEGIN SELECT RAISE(ABORT, 'injected'); END;`)

	if err := m.EnsureDefaultDNS(); err == nil {
		t.Fatal("dns_rules 写入已被注入失败，EnsureDefaultDNS 仍返回成功")
	}
	if got := countRows(t, raw, "dns_servers"); got != 0 {
		t.Errorf("回滚后 dns_servers 应为 0 行，实得 %d", got)
	}
	if got := countRows(t, raw, "dns_rules"); got != 0 {
		t.Errorf("回滚后 dns_rules 应为 0 行，实得 %d", got)
	}
	if v, err := m.DB.GetSetting(keyFinal); err != nil || v != "" {
		t.Errorf("回滚后 dns_final 不应存在，实得 %q err=%v", v, err)
	}
}

// 对照组：不被注入时初始化必须真的写成功（否则上面的断言可能因为「永不触发」而假通过）。
func TestEnsureDefaultDNSWritesAllPieces(t *testing.T) {
	m, raw := newTxTestManager(t)
	if err := m.EnsureDefaultDNS(); err != nil {
		t.Fatalf("EnsureDefaultDNS: %v", err)
	}
	if got := countRows(t, raw, "dns_servers"); got != 2 {
		t.Errorf("dns_servers = %d, want 2", got)
	}
	if got := countRows(t, raw, "dns_rules"); got != 1 {
		t.Errorf("dns_rules = %d, want 1", got)
	}
	if v, err := m.DB.GetSetting(keyFinal); err != nil || v != "remote" {
		t.Errorf("dns_final = %q err=%v, want remote", v, err)
	}
}

// SaveOptions 的 4 个 setting 必须同一事务提交：任一 key 写失败就全部回滚。
func TestSaveOptionsRollsBackOnMidwayFailure(t *testing.T) {
	m, raw := newTxTestManager(t)
	if err := m.DB.CreateDNSServer(&config.DNSServer{Tag: "keep", Type: "udp", Address: "223.5.5.5", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveOptions("ipv4_only", true, "198.18.0.0/15", "keep"); err != nil {
		t.Fatalf("预设旧选项: %v", err)
	}
	before := readOptions(t, m)

	// upsert 可能走 INSERT 也可能走 DO UPDATE 分支，两条都拦。
	mustExec(t, raw, `CREATE TRIGGER inject_final_ins BEFORE INSERT ON settings
		WHEN NEW.key='dns_final' BEGIN SELECT RAISE(ABORT, 'injected'); END;`)
	mustExec(t, raw, `CREATE TRIGGER inject_final_upd BEFORE UPDATE ON settings
		WHEN NEW.key='dns_final' BEGIN SELECT RAISE(ABORT, 'injected'); END;`)

	if err := m.SaveOptions("prefer_ipv6", false, "", "keep"); err == nil {
		t.Fatal("dns_final 写入已被注入失败，SaveOptions 仍返回成功")
	}
	if after := readOptions(t, m); after != before {
		t.Errorf("任一 key 失败后 4 个选项必须整体回滚\n before=%+v\n after =%+v", before, after)
	}
}

// MoveRule 重新编号必须整体原子：中途失败时所有 position 保持原值。
func TestMoveRuleRollsBackOnMidwayFailure(t *testing.T) {
	m, raw := newTxTestManager(t)
	if err := m.DB.CreateDNSServer(&config.DNSServer{Tag: "keep", Type: "udp", Address: "223.5.5.5", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"a.example", "b.example", "c.example"} {
		if _, err := m.AddRule("domain", v, "keep"); err != nil {
			t.Fatalf("AddRule %s: %v", v, err)
		}
	}
	before := rulePositions(t, m)
	if len(before) != 3 {
		t.Fatalf("夹具应有 3 条规则，实得 %+v", before)
	}

	// 上移第二条会依次写出 position=0（第一条）与 position=1（第二条）；
	// 拦 position=1 即「第二个 UPDATE 失败」。
	mustExec(t, raw, `CREATE TRIGGER inject_pos BEFORE UPDATE ON dns_rules
		WHEN NEW.position=1 BEGIN SELECT RAISE(ABORT, 'injected'); END;`)

	rules, err := m.DB.ListDNSRules()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MoveRule(rules[1].ID, -1); err == nil {
		t.Fatal("第二个 UPDATE 已被注入失败，MoveRule 仍返回成功")
	}
	if after := rulePositions(t, m); !reflect.DeepEqual(before, after) {
		t.Errorf("中途失败后 position 必须整体回滚\n before=%+v\n after =%+v", before, after)
	}
}
