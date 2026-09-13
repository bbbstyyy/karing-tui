package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 4.3 内置分类库：规则以 "geosite:cn" 引用分类，生成器按需产出 route.rule_set
// 条目（有缓存 local、缺缓存 remote），不再依赖 rulesets 表里的固定条目。

// ruleSetItems 取 route.rule_set 条目，按 tag 索引。
func ruleSetItems(t *testing.T, m map[string]any) map[string]map[string]any {
	t.Helper()
	route, ok := m["route"].(map[string]any)
	if !ok {
		return nil
	}
	items, ok := route["rule_set"].([]any)
	if !ok {
		return nil
	}
	out := map[string]map[string]any{}
	for _, it := range items {
		e := it.(map[string]any)
		out[e["tag"].(string)] = e
	}
	return out
}

// TestCatalogRefGeneratesRuleSetOnDemand 分类引用无需预先进 rulesets 表，
// 生成器按需产出条目；缺缓存时回退 remote 并带上官方下载地址。
func TestCatalogRefGeneratesRuleSetOnDemand(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "中国大陆", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "rule_set", Value: "geosite:cn,geoip:cn", Enabled: true}},
	}}
	set := DefaultSettings()
	set.PrivateDirect = false
	// RuleSets 为空：整条链路只靠分类库
	m := mustGenerate(t, Snapshot{Settings: set, RoutingGroups: routings})

	// 规则里引用的是派生 tag
	rules := routeRules(t, m)
	var tags []string
	for _, r := range rules {
		if r["rule_set"] != nil {
			tags = toStringSlice(t, r["rule_set"])
		}
	}
	if len(tags) != 2 || tags[0] != "geosite-cn" || tags[1] != "geoip-cn" {
		t.Fatalf("规则 rule_set = %v, 期望 [geosite-cn geoip-cn]", tags)
	}

	items := ruleSetItems(t, m)
	if len(items) != 2 {
		t.Fatalf("期望 2 个 rule_set 条目，得到 %d: %v", len(items), items)
	}
	cn := items["geosite-cn"]
	if cn["type"] != "remote" {
		t.Errorf("无缓存时应回退 remote，得到 %v", cn["type"])
	}
	if cn["format"] != "binary" {
		t.Errorf("分类库一律 srs → binary，得到 %v", cn["format"])
	}
	wantURL := "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geosite/cn.srs"
	if cn["url"] != wantURL {
		t.Errorf("url = %v, 期望 %v", cn["url"], wantURL)
	}
	if cn["download_detour"] != nil {
		t.Errorf("不应生成已废弃的 download_detour: %v", cn["download_detour"])
	}
}

// TestCatalogRefUsesCacheWhenPresent 缓存文件存在时应生成 local 条目，
// 使核心启动不依赖网络。
func TestCatalogRefUsesCacheWhenPresent(t *testing.T) {
	dir := t.TempDir()
	cached := filepath.Join(dir, "geosite-cn.srs")
	if err := os.WriteFile(cached, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "rule_set", Value: "geosite:cn,geosite:telegram", Enabled: true}},
	}}
	set := DefaultSettings()
	set.PrivateDirect = false
	m := mustGenerate(t, Snapshot{
		Settings: set, RoutingGroups: routings, RuleSetCacheDir: dir,
	})

	items := ruleSetItems(t, m)
	if got := items["geosite-cn"]; got["type"] != "local" || got["path"] != cached {
		t.Errorf("有缓存应为 local %s，得到 type=%v path=%v", cached, got["type"], got["path"])
	}
	// 同一批里未缓存的仍回退 remote
	if got := items["geosite-telegram"]; got["type"] != "remote" {
		t.Errorf("未缓存的分类应为 remote，得到 %v", got["type"])
	}
}

// TestCatalogRefTagForm 连字符形式（即生成的配置里的 tag）同样可作为规则值，
// 便于用户照抄配置；解析结果与冒号形式一致。
func TestCatalogRefTagForm(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "rule_set", Value: "geosite-category-ai-!cn", Enabled: true}},
	}}
	set := DefaultSettings()
	set.PrivateDirect = false
	m := mustGenerate(t, Snapshot{Settings: set, RoutingGroups: routings})

	items := ruleSetItems(t, m)
	if _, ok := items["geosite-category-ai-!cn"]; !ok {
		t.Fatalf("期望生成 geosite-category-ai-!cn 条目，实得 %v", items)
	}
}

// TestCatalogRefInLogicalRule 逻辑规则的子条件同样支持分类引用。
func TestCatalogRefInLogicalRule(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "BLOCK", Enabled: true,
		Rules: []Rule{{
			Type: "logical", Mode: "and", Enabled: true,
			Conditions: []RuleCondition{
				{Type: "rule_set", Value: "acl:Netflix"},
				{Type: "domain_suffix", Value: "netflix.com"},
			},
		}},
	}}
	set := DefaultSettings()
	set.PrivateDirect = false
	m := mustGenerate(t, Snapshot{Settings: set, RoutingGroups: routings})

	items := ruleSetItems(t, m)
	item, ok := items["acl-Netflix"]
	if !ok {
		t.Fatalf("逻辑子条件的分类引用未生成条目: %v", items)
	}
	wantURL := "https://github.com/KaringX/karing-ruleset/raw/sing/ACL4SSR/Netflix.srs"
	if item["url"] != wantURL {
		t.Errorf("acl url = %v, 期望 %v", item["url"], wantURL)
	}
}

// TestCatalogRefInDNSRule 只被 DNS 规则引用的分类也要生成 route.rule_set 条目，
// 否则 sing-box 启动报 rule-set not found（4.1 修过自定义规则集的同类缺陷）。
func TestCatalogRefInDNSRule(t *testing.T) {
	dnsCfg := &DNSConfig{
		Servers: []DNSServer{
			{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true},
			{Tag: "remote", Type: "https", Address: "8.8.8.8", Enabled: true},
		},
		Rules: []DNSRule{{Type: "rule_set", Value: "geosite:cn", Server: "local", Enabled: true}},
		Final: "remote",
	}
	set := DefaultSettings()
	set.PrivateDirect = false
	m := mustGenerate(t, Snapshot{Settings: set, DNS: dnsCfg})

	items := ruleSetItems(t, m)
	if _, ok := items["geosite-cn"]; !ok {
		t.Fatalf("仅被 DNS 规则引用的分类未生成 route.rule_set 条目: %v", items)
	}
	// DNS 规则里引用的也是派生 tag
	dnsRules := m["dns"].(map[string]any)["rules"].([]any)
	first := dnsRules[0].(map[string]any)
	if tags := toStringSlice(t, first["rule_set"]); len(tags) != 1 || tags[0] != "geosite-cn" {
		t.Errorf("DNS 规则 rule_set = %v, 期望 [geosite-cn]", tags)
	}
}

// TestCatalogRefUnknownCodeRejected 分类码不存在时必须报错，而不是当成 remote
// 条目生成一个 404 的 URL——那样要等到核心启动下载才失败。
func TestCatalogRefUnknownCodeRejected(t *testing.T) {
	for _, value := range []string{"geosite:压根不存在", "geosite-压根不存在", "bogus:cn", "未知规则集"} {
		routings := []*RoutingGroup{{
			Name: "G", Target: "DIRECT", Enabled: true,
			Rules: []Rule{{Type: "rule_set", Value: value, Enabled: true}},
		}}
		if _, err := Generate(Snapshot{Settings: DefaultSettings(), RoutingGroups: routings}); err == nil {
			t.Errorf("规则集值 %q 应被拒绝", value)
		}
	}
}

// TestCustomRuleSetShadowsCatalog 派生 tag 命中 rulesets 表时优先用表记录
// （老库遗留的内置条目携带用户可能改过的 URL 与实际缓存路径）。
// 两条路径若互相跳过，rule_set 条目会整个丢失、核心报 rule-set not found。
func TestCustomRuleSetShadowsCatalog(t *testing.T) {
	dir := t.TempDir()
	cached := filepath.Join(dir, "mycn.srs")
	if err := os.WriteFile(cached, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ruleSets := []*RuleSet{{
		ID: 1, Tag: "geosite-cn", Name: "我改过的 cn", SourceType: "remote", Format: "srs",
		URL: "https://mirror.example.com/cn.srs", Enabled: true, CachedPath: cached,
	}}
	// 用冒号形式引用：派生 tag 为 geosite-cn，与上面的表记录同名
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "rule_set", Value: "geosite:cn", Enabled: true}},
	}}
	set := DefaultSettings()
	set.PrivateDirect = false
	m := mustGenerate(t, Snapshot{
		Settings: set, RuleSets: ruleSets, RoutingGroups: routings, RuleSetCacheDir: dir,
	})

	items := ruleSetItems(t, m)
	if len(items) != 1 {
		t.Fatalf("期望恰好 1 个条目（不得重复 tag），实得 %d: %v", len(items), items)
	}
	got := items["geosite-cn"]
	if got["type"] != "local" || got["path"] != cached {
		t.Errorf("应采用表记录的缓存路径 %s，得到 type=%v path=%v", cached, got["type"], got["path"])
	}
}

// TestCatalogRefDisabledCustomRuleSetRejected 派生 tag 命中的表记录被停用时报错，
// 与直接写该 tag 的行为一致。
func TestCatalogRefDisabledCustomRuleSetRejected(t *testing.T) {
	ruleSets := []*RuleSet{{
		ID: 1, Tag: "geosite-cn", Name: "cn", SourceType: "remote", Format: "srs",
		URL: "https://example.com/cn.srs", Enabled: false,
	}}
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "rule_set", Value: "geosite:cn", Enabled: true}},
	}}
	if _, err := Generate(Snapshot{
		Settings: DefaultSettings(), RuleSets: ruleSets, RoutingGroups: routings,
	}); err == nil {
		t.Error("引用被停用的规则集应报错")
	}
}

// TestCatalogRuleSetIdempotent 分类条目按 tag 排序输出，保证多次生成字节一致。
func TestCatalogRuleSetIdempotent(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{
			Type:    "rule_set",
			Value:   "geosite:telegram,geoip:telegram,acl:ChinaDomain,geosite:cn,geoip:cn",
			Enabled: true,
		}},
	}}
	snap := Snapshot{Settings: DefaultSettings(), RoutingGroups: routings}
	first, err := Generate(snap)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := Generate(snap)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if string(again) != string(first) {
			t.Fatal("同一快照多次生成结果不一致（分类条目顺序不稳定）")
		}
	}
	// 顺序应为 tag 字典序
	m := mustGenerate(t, snap)
	var order []string
	for _, it := range m["route"].(map[string]any)["rule_set"].([]any) {
		order = append(order, it.(map[string]any)["tag"].(string))
	}
	want := []string{"acl-ChinaDomain", "geoip-cn", "geoip-telegram", "geosite-cn", "geosite-telegram"}
	if len(order) != len(want) {
		t.Fatalf("条目顺序 = %v, 期望 %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("条目顺序 = %v, 期望 %v", order, want)
		}
	}
}

// TestCatalogNeedsResolve 分类引用的 IP 属性判定按实测清单而非 tag 字面——
// 这决定 4.1 的 resolve 动作插不插、插在哪。
func TestCatalogNeedsResolve(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"geoip 分类", Rule{Type: "rule_set", Value: "geoip:cn"}, true},
		{"geoip 派生 tag", Rule{Type: "rule_set", Value: "geoip-cn"}, true},
		{"geosite 分类", Rule{Type: "rule_set", Value: "geosite:cn"}, false},
		// acl:ChinaDomain 名字里没有 "ip"，但内容含 ip_cidr——旧的字面判断会漏判
		{"acl 含 IP 条件", Rule{Type: "rule_set", Value: "acl:ChinaDomain"}, true},
		{"acl 名字含 ip 但无 IP 条件", Rule{Type: "rule_set", Value: "acl:Wikipedia"}, false},
		{"多值含 IP 分类", Rule{Type: "rule_set", Value: "geosite:cn,geoip:cn"}, true},
		{"逻辑规则子条件", Rule{Type: "logical", Mode: "or", Conditions: []RuleCondition{
			{Type: "domain", Value: "a.com"}, {Type: "rule_set", Value: "acl:Apple"},
		}}, true},
		// 自定义规则集仍退回命名约定
		{"自定义含 geoip 字样", Rule{Type: "rule_set", Value: "my-geoip-set"}, true},
		{"自定义普通 tag", Rule{Type: "rule_set", Value: "my-rules"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ruleNeedsResolve(&c.rule); got != c.want {
				t.Errorf("ruleNeedsResolve = %v, 期望 %v", got, c.want)
			}
		})
	}
}

// TestCatalogResolveInsertedForACLIPCategory acl 里的 IP 类分类要能触发 resolve
// 插入——这是把 IP 判定做成实测清单的实际收益。
func TestCatalogResolveInsertedForACLIPCategory(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{
			{Type: "domain_suffix", Value: "example.com", Enabled: true},
			{Type: "rule_set", Value: "acl:ChinaDomain", Enabled: true},
		},
	}}
	set := DefaultSettings()
	set.PrivateDirect = false
	set.ResolveIPRules = true
	m := mustGenerate(t, Snapshot{Settings: set, RoutingGroups: routings})

	got := ruleActions(routeRules(t, m))
	want := []string{"sniff", "rule", "resolve", "rule_set:acl-ChinaDomain"}
	if len(got) != len(want) {
		t.Fatalf("规则序列 = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("规则序列 = %v, 期望 %v", got, want)
		}
	}
}

// TestCatalogNoResolveForGeositeOnly 纯 geosite 分类不应触发 resolve——
// 上游全量实测均不含 IP 条件，插 resolve 只会白付代价。
func TestCatalogNoResolveForGeositeOnly(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "rule_set", Value: "geosite:cn,geosite:telegram", Enabled: true}},
	}}
	set := DefaultSettings()
	set.PrivateDirect = false
	set.ResolveIPRules = true
	m := mustGenerate(t, Snapshot{Settings: set, RoutingGroups: routings})

	for _, action := range ruleActions(routeRules(t, m)) {
		if action == "resolve" {
			t.Fatal("纯 geosite 分类不应插入 resolve")
		}
	}
}
