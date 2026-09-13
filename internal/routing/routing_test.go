package routing

import (
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	db, err := storage.Open(paths)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateProxyGroup(&config.ProxyGroup{Name: "Auto", Type: "urltest"}); err != nil {
		t.Fatalf("预置代理组: %v", err)
	}
	return NewManager(db, nil)
}

func TestCreateGroupWithLogicalRule(t *testing.T) {
	m := newTestManager(t)

	g, err := m.CreateGroup("逻辑组", "Auto", []config.Rule{{
		Type: "logical", Mode: "and", Enabled: true,
		Conditions: []config.RuleCondition{
			{Type: "domain_suffix", Value: "a.com"},
			{Type: "ip_cidr", Value: "10.0.0.0/8", Invert: true},
		},
	}})
	if err != nil {
		t.Fatalf("创建逻辑规则分流组: %v", err)
	}

	// 回读一致
	got, err := m.DB.GetRoutingGroup(g.ID)
	if err != nil {
		t.Fatalf("GetRoutingGroup: %v", err)
	}
	if got.Rules[0].Mode != "and" || len(got.Rules[0].Conditions) != 2 {
		t.Errorf("逻辑规则回读不符: %+v", got.Rules[0])
	}
}

func TestRejectsMultipleActiveFinalGroups(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.CreateGroup("Final 1", "Auto", []config.Rule{{Type: "final", Enabled: true}}); err != nil {
		t.Fatalf("创建第一个 final: %v", err)
	}
	if _, err := m.CreateGroup("Final 2", "Auto", []config.Rule{{Type: "final", Enabled: true}}); err == nil {
		t.Fatal("活动 final 分流组应全局唯一")
	}
}

func TestValidateLogicalErrors(t *testing.T) {
	m := newTestManager(t)

	cases := []struct {
		name string
		rule config.Rule
	}{
		{"非法组合方式", config.Rule{Type: "logical", Mode: "xor", Conditions: []config.RuleCondition{{Type: "domain", Value: "a"}}}},
		{"缺少子条件", config.Rule{Type: "logical", Mode: "and"}},
		{"未知条件类型", config.Rule{Type: "logical", Mode: "or", Conditions: []config.RuleCondition{{Type: "nosuch", Value: "x"}}}},
		{"条件值为空", config.Rule{Type: "logical", Mode: "and", Conditions: []config.RuleCondition{{Type: "domain", Value: " "}}}},
		{"final 混入组内", config.Rule{Type: "domain", Value: "a.com"}},
	}
	// final 规则校验：final 必须是组内唯一规则——先造一个含 final + 普通规则的组
	if _, err := m.CreateGroup("Final混用", "Auto", []config.Rule{
		{Type: "final", Enabled: true},
		{Type: "domain", Value: "a.com"},
	}); err == nil {
		t.Error("final 与其他规则同组应被拒绝")
	}
	cases = cases[:4]

	for _, c := range cases {
		if _, err := m.CreateGroup(c.name, "Auto", []config.Rule{c.rule}); err == nil {
			t.Errorf("%s 应被拒绝", c.name)
		}
	}
}

func TestUpdateGroupRejectsUnknownRuleType(t *testing.T) {
	m := newTestManager(t)
	g, err := m.CreateGroup("G", "Auto", []config.Rule{{Type: "domain", Value: "a.com"}})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	g.Rules[0].Type = "bogus"
	if err := m.UpdateGroup(g, g.Rules); err == nil {
		t.Error("未知规则类型应被拒绝")
	}
}

func TestCreateGroupWithDomainRegex(t *testing.T) {
	m := newTestManager(t)

	// 含逗号的正则（量词 {2,4}）必须原样保留——值不按逗号拆分
	pattern := `^ads\.[a-z]{2,4}\.com$`
	g, err := m.CreateGroup("正则组", "DIRECT", []config.Rule{
		{Type: "domain_regex", Value: pattern, Enabled: true},
	})
	if err != nil {
		t.Fatalf("创建 domain_regex 分流组: %v", err)
	}

	groups, err := m.DB.ListRoutingGroups()
	if err != nil {
		t.Fatal(err)
	}
	var got *config.RoutingGroup
	for _, x := range groups {
		if x.ID == g.ID {
			got = x
		}
	}
	if got == nil {
		t.Fatal("未回读到分流组")
	}
	if len(got.Rules) != 1 || got.Rules[0].Type != "domain_regex" || got.Rules[0].Value != pattern {
		t.Errorf("domain_regex 规则回读不符: %+v", got.Rules)
	}
}

func TestDomainRegexInvalidRejected(t *testing.T) {
	m := newTestManager(t)

	// 未闭合的分组：非法正则须在写入前拒绝，否则 sing-box 启动失败
	if _, err := m.CreateGroup("坏正则", "DIRECT", []config.Rule{
		{Type: "domain_regex", Value: `^ads\.(com$`, Enabled: true},
	}); err == nil {
		t.Error("非法 domain_regex 应被拒绝")
	}

	// 逻辑规则子条件里的非法正则同样要拒绝
	if _, err := m.CreateGroup("坏正则子条件", "DIRECT", []config.Rule{{
		Type: "logical", Mode: "or", Enabled: true,
		Conditions: []config.RuleCondition{{Type: "domain_regex", Value: `a{2,1}`}},
	}}); err == nil {
		t.Error("逻辑子条件中的非法 domain_regex 应被拒绝")
	}

	// 合法正则不受影响
	if _, err := m.CreateGroup("好正则", "DIRECT", []config.Rule{
		{Type: "domain_regex", Value: `^(a|b)\.com$`, Enabled: true},
	}); err != nil {
		t.Errorf("合法 domain_regex 不应被拒绝: %v", err)
	}
}
