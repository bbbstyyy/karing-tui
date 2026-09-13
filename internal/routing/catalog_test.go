package routing

import (
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// 4.3：rule_set 规则的值可以是内置分类引用，无需预先写进 rulesets 表。

// TestCatalogRefAccepted 分类引用（两种写法）在校验时放行。
func TestCatalogRefAccepted(t *testing.T) {
	m := newTestManager(t)

	values := []string{
		"geosite:cn",
		"geoip:jp",
		"acl:ChinaDomain",
		"geosite-category-ai-!cn", // 派生 tag 形式
		"geosite:cn,geoip:cn",     // 多值
	}
	for _, v := range values {
		g, err := m.CreateGroup("组-"+v, "Auto", []config.Rule{
			{Type: "rule_set", Value: v, Enabled: true},
		})
		if err != nil {
			t.Errorf("分类引用 %q 应被接受: %v", v, err)
			continue
		}
		got, err := m.DB.GetRoutingGroup(g.ID)
		if err != nil {
			t.Fatalf("回读分流组: %v", err)
		}
		if len(got.Rules) != 1 || got.Rules[0].Value != v {
			t.Errorf("回读值 = %v, 期望 %q", got.Rules, v)
		}
	}
}

// TestCatalogRefInLogicalConditionAccepted 逻辑规则的子条件同样支持分类引用。
func TestCatalogRefInLogicalConditionAccepted(t *testing.T) {
	m := newTestManager(t)

	if _, err := m.CreateGroup("逻辑分类组", "Auto", []config.Rule{{
		Type: "logical", Mode: "and", Enabled: true,
		Conditions: []config.RuleCondition{
			{Type: "rule_set", Value: "acl:Netflix"},
			{Type: "domain_suffix", Value: "netflix.com"},
		},
	}}); err != nil {
		t.Fatalf("逻辑子条件的分类引用应被接受: %v", err)
	}
}

// TestUnknownRuleSetRejected 既非自定义规则集、又非合法分类引用时拒绝，
// 且错误信息要提示分类写法（否则用户不知道能直接写 geosite:cn）。
func TestUnknownRuleSetRejected(t *testing.T) {
	m := newTestManager(t)

	_, err := m.CreateGroup("坏组", "Auto", []config.Rule{
		{Type: "rule_set", Value: "压根不存在的规则集", Enabled: true},
	})
	if err == nil {
		t.Fatal("未知规则集应被拒绝")
	}
	if !strings.Contains(err.Error(), "geosite:cn") {
		t.Errorf("错误信息应提示内置分类写法，实得: %v", err)
	}
}

// TestUnknownCatalogCodeRejected 种类合法但分类码不存在时，错误要指出是分类码的问题，
// 而不是笼统地说"规则集不存在"。
func TestUnknownCatalogCodeRejected(t *testing.T) {
	m := newTestManager(t)

	_, err := m.CreateGroup("坏组2", "Auto", []config.Rule{
		{Type: "rule_set", Value: "geosite:这个码不存在", Enabled: true},
	})
	if err == nil {
		t.Fatal("不存在的分类码应被拒绝")
	}
	if !strings.Contains(err.Error(), "分类码") {
		t.Errorf("错误信息应指明分类码不存在，实得: %v", err)
	}
}

// TestCustomRuleSetStillAccepted 分类库上线后，rulesets 表里的自定义规则集
// 仍然可被引用（不能被分类校验挡掉）。
func TestCustomRuleSetStillAccepted(t *testing.T) {
	m := newTestManager(t)

	if err := m.DB.CreateRuleSet(&config.RuleSet{
		Name: "我的规则", Tag: "my-rules", SourceType: "remote", Format: "srs",
		URL: "https://example.com/my.srs", Enabled: true,
	}); err != nil {
		t.Fatalf("预置自定义规则集: %v", err)
	}
	if _, err := m.CreateGroup("自定义组", "Auto", []config.Rule{
		{Type: "rule_set", Value: "my-rules", Enabled: true},
	}); err != nil {
		t.Fatalf("自定义规则集引用应被接受: %v", err)
	}
}

// TestDefaultRoutingUsesCatalogRefs 默认分流方案改用分类引用后必须能通过自身校验——
// 新库不再预置那 7 条 rulesets 记录，若默认方案仍写老 tag 会导致应用启动失败。
func TestDefaultRoutingUsesCatalogRefs(t *testing.T) {
	m := newTestManager(t)

	if err := EnsureDefaultRouting(m.DB); err != nil {
		t.Fatalf("初始化默认分流方案: %v", err)
	}
	groups, err := m.DB.ListRoutingGroups()
	if err != nil {
		t.Fatalf("读取分流组: %v", err)
	}
	if len(groups) == 0 {
		t.Fatal("默认分流方案未创建任何分流组")
	}
	// rulesets 表应为空：分类引用不入表
	sets, err := m.DB.ListRuleSets()
	if err != nil {
		t.Fatalf("读取规则集: %v", err)
	}
	if len(sets) != 0 {
		t.Errorf("默认方案不应往 rulesets 表写记录，实得 %d 条", len(sets))
	}
	// 每条 rule_set 规则的值都要能通过校验（即都是合法分类引用）
	var ruleSetRules int
	for _, g := range groups {
		for _, r := range g.Rules {
			if r.Type != "rule_set" {
				continue
			}
			ruleSetRules++
			if err := m.checkRuleSetTags(r.Value); err != nil {
				t.Errorf("默认方案的规则 %q 校验失败: %v", r.Value, err)
			}
		}
	}
	if ruleSetRules == 0 {
		t.Error("默认方案应包含 rule_set 规则")
	}
}
