package routing

import (
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

func testGroups() []*config.RoutingGroup {
	return []*config.RoutingGroup{
		{
			ID: 1, Name: "广告", Target: "BLOCK", Position: 0, Enabled: true,
			Rules: []config.Rule{{Type: "domain_suffix", Value: "ads.example.com", Enabled: true}},
		},
		{
			ID: 2, Name: "工作", Target: "DIRECT", Position: 1, Enabled: true,
			Rules: []config.Rule{{Type: "logical", Mode: "and", Enabled: true, Conditions: []config.RuleCondition{
				{Type: "domain_keyword", Value: "internal"},
				{Type: "domain_regex", Value: `^.+\.example\.com$`},
			}}},
		},
		{
			ID: 3, Name: "IP", Target: "Auto", Position: 2, Enabled: true,
			Rules: []config.Rule{{Type: "ip_cidr", Value: "10.0.0.0/8,192.168.0.0/16", Enabled: true}},
		},
		{
			ID: 4, Name: "Final", Target: "Auto", Position: 3, Enabled: true,
			Rules: []config.Rule{{Type: "final", Enabled: true}},
		},
	}
}

func TestDetectDomainAndFallback(t *testing.T) {
	got, err := Detect(testGroups(), "foo.ads.example.com.")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Matched || got.Fallback || got.Group.Name != "广告" || got.Target != "BLOCK" {
		t.Fatalf("广告规则结果不符: %+v", got)
	}

	got, err = Detect(testGroups(), "www.example.net")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Matched || !got.Fallback || got.Group.Name != "Final" || got.Target != "Auto" {
		t.Fatalf("兜底结果不符: %+v", got)
	}
}

func TestDetectLogicalAndInvert(t *testing.T) {
	groups := testGroups()
	got, err := Detect(groups, "internal.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Group.Name != "工作" || got.Fallback {
		t.Fatalf("逻辑规则未命中: %+v", got)
	}

	groups[1].Rules[0].Invert = true
	got, err = Detect(groups, "internal.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Fallback || got.Group.Name != "Final" {
		t.Fatalf("规则级 invert 未生效: %+v", got)
	}
}

func TestDetectIPAndUnknownRuleSet(t *testing.T) {
	got, err := Detect(testGroups(), "192.168.1.10")
	if err != nil {
		t.Fatal(err)
	}
	if got.Group.Name != "IP" || got.Fallback {
		t.Fatalf("CIDR 规则未命中: %+v", got)
	}

	groups := []*config.RoutingGroup{
		{Name: "分类", Target: "DIRECT", Enabled: true, Rules: []config.Rule{{Type: "rule_set", Value: "geosite:cn", Enabled: true}}},
	}
	got, err = Detect(groups, "www.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.UnknownRules) != 1 || !strings.Contains(got.UnknownRules[0], "geosite:cn") {
		t.Fatalf("未知规则集提示不符: %+v", got.UnknownRules)
	}
}

func TestDetectInvertAcrossAddressKinds(t *testing.T) {
	ipRule := []*config.RoutingGroup{{Name: "非内网", Target: "PROXY", Enabled: true, Rules: []config.Rule{{Type: "ip_cidr", Value: "10.0.0.0/8", Invert: true, Enabled: true}}}}
	got, err := Detect(ipRule, "example.com")
	if err != nil || !got.Matched || got.Target != "PROXY" {
		t.Fatalf("反转 CIDR 对域名应命中: %+v, err=%v", got, err)
	}
	domainRule := []*config.RoutingGroup{{Name: "非示例", Target: "PROXY", Enabled: true, Rules: []config.Rule{{Type: "domain_suffix", Value: "example.com", Invert: true, Enabled: true}}}}
	got, err = Detect(domainRule, "192.0.2.1")
	if err != nil || !got.Matched || got.Target != "PROXY" {
		t.Fatalf("反转域名规则对 IP 应命中: %+v, err=%v", got, err)
	}
	logical := []*config.RoutingGroup{{Name: "非内网域名", Target: "PROXY", Enabled: true, Rules: []config.Rule{{
		Type: "logical", Mode: "and", Invert: true, Enabled: true,
		Conditions: []config.RuleCondition{{Type: "domain_suffix", Value: "example.com"}, {Type: "ip_cidr", Value: "10.0.0.0/8"}},
	}}}}
	got, err = Detect(logical, "www.example.com")
	if err != nil || !got.Matched || got.Target != "PROXY" {
		t.Fatalf("逻辑规则子条件不匹配时仍应应用反转: %+v, err=%v", got, err)
	}
}

func TestDetectPrivateDirect(t *testing.T) {
	for _, input := range []string{"127.0.0.1", "0.0.0.0", "224.0.0.1", "ff02::1"} {
		got, err := DetectWithPrivateDirect(testGroups(), input, true)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Matched || !got.PrivateDirect || got.Target != "DIRECT" {
			t.Fatalf("地址 %s 应命中内网直连: %+v", input, got)
		}
	}
	got, err := DetectWithPrivateDirect(testGroups(), "127.0.0.1", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.PrivateDirect || got.Target == "DIRECT" && got.Matched {
		t.Fatalf("关闭内网直连后不应命中合成规则: %+v", got)
	}
}

func TestDetectRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{"", "localhost", "https://example.com", "bad domain"} {
		if _, err := Detect(nil, input); err == nil {
			t.Errorf("输入 %q 应被拒绝", input)
		}
	}
}
