package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustGenerate 生成配置并解析为 map，失败即终止测试。
func mustGenerate(t *testing.T, snap Snapshot) map[string]any {
	t.Helper()
	out, err := Generate(snap)
	if err != nil {
		t.Fatalf("Generate: %v\n快照: %+v", err, snap)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, out)
	}
	return m
}

func TestGenerateMinimalShell(t *testing.T) {
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings()})

	inbounds, ok := m["inbounds"].([]any)
	if !ok || len(inbounds) != 1 {
		t.Fatalf("期望 1 个入站，得到 %v", m["inbounds"])
	}
	inb := inbounds[0].(map[string]any)
	if inb["type"] != "mixed" || inb["listen"] != "127.0.0.1" || inb["listen_port"] != float64(2080) {
		t.Errorf("入站字段不符: %v", inb)
	}

	outbounds := m["outbounds"].([]any)
	if len(outbounds) != 1 || outbounds[0].(map[string]any)["type"] != "direct" {
		t.Errorf("出站字段不符: %v", outbounds)
	}
}

func TestGenerateIdempotent(t *testing.T) {
	snap := Snapshot{Settings: DefaultSettings()}
	a, err := Generate(snap)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, err := Generate(snap)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if string(a) != string(b) {
		t.Error("同一快照两次生成结果不一致")
	}
}

func TestGenerateFullSnapshotIdempotent(t *testing.T) {
	snap := testFullSnapshot(t)
	a, err := Generate(snap)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, err := Generate(snap)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if string(a) != string(b) {
		t.Error("全量快照两次生成结果不一致")
	}
}

func TestGenerateAllowLAN(t *testing.T) {
	s := DefaultSettings()
	s.AllowLAN = true
	inb := mustGenerate(t, Snapshot{Settings: s})["inbounds"].([]any)[0].(map[string]any)
	if inb["listen"] != "0.0.0.0" {
		t.Errorf("AllowLAN 时 listen = %v, 期望 0.0.0.0", inb["listen"])
	}
}

func TestGenerateRejectsBadPort(t *testing.T) {
	s := DefaultSettings()
	s.MixedPort = 0
	if _, err := Generate(Snapshot{Settings: s}); err == nil {
		t.Error("端口为 0 时应返回错误")
	}
}

// --- 节点出站 ---

func TestNodeOutboundProtocols(t *testing.T) {
	nodes := []*Node{
		{ID: 1, Name: "ss1", Protocol: "shadowsocks", Server: "1.2.3.4", Port: 8388, Enabled: true,
			Metadata: map[string]any{"method": "aes-128-gcm", "password": "pw"}},
		{ID: 2, Name: "vmess1", Protocol: "vmess", Server: "a.com", Port: 443, TLS: true, Transport: "ws", Enabled: true,
			Metadata: map[string]any{"uuid": "u2", "path": "/ws", "host": "a.com", "sni": "a.com"}},
		{ID: 3, Name: "vless1", Protocol: "vless", Server: "3.3.3.3", Port: 443, TLS: true, Enabled: true,
			Metadata: map[string]any{"uuid": "u3", "flow": "xtls-rprx-vision", "reality_public_key": "pk", "reality_short_id": "sid", "fingerprint": "chrome"}},
		{ID: 4, Name: "trojan1", Protocol: "trojan", Server: "4.4.4.4", Port: 443, TLS: true, Enabled: true,
			Metadata: map[string]any{"password": "pw4"}},
		{ID: 5, Name: "hy2", Protocol: "hysteria2", Server: "5.5.5.5", Port: 443, TLS: true, Enabled: true,
			Metadata: map[string]any{"password": "pw5", "obfs": "salamander", "obfs_password": "op"}},
		{ID: 6, Name: "tuic1", Protocol: "tuic", Server: "6.6.6.6", Port: 443, TLS: true, Enabled: true,
			Metadata: map[string]any{"uuid": "u6", "password": "pw6", "congestion_control": "bbr"}},
	}
	snap := Snapshot{Settings: DefaultSettings(), Nodes: nodes}
	m := mustGenerate(t, snap)

	obs := m["outbounds"].([]any)
	if len(obs) != len(nodes)+1 { // 节点 + direct
		t.Fatalf("期望 %d 个出站，得到 %d", len(nodes)+1, len(obs))
	}
	byTag := map[string]map[string]any{}
	for _, o := range obs {
		ob := o.(map[string]any)
		byTag[ob["tag"].(string)] = ob
	}

	ss := byTag["ss1"]
	if ss["method"] != "aes-128-gcm" || ss["password"] != "pw" || ss["server_port"] != float64(8388) {
		t.Errorf("shadowsocks 出站字段不符: %v", ss)
	}

	vm := byTag["vmess1"]
	if vm["uuid"] != "u2" || vm["security"] != "auto" {
		t.Errorf("vmess 出站字段不符: %v", vm)
	}
	tls := vm["tls"].(map[string]any)
	if tls["server_name"] != "a.com" {
		t.Errorf("vmess TLS server_name 不符: %v", tls)
	}
	tr := vm["transport"].(map[string]any)
	if tr["type"] != "ws" || tr["path"] != "/ws" {
		t.Errorf("vmess transport 不符: %v", tr)
	}

	vl := byTag["vless1"]
	if vl["flow"] != "xtls-rprx-vision" {
		t.Errorf("vless flow 不符: %v", vl)
	}
	rt := vl["tls"].(map[string]any)["reality"].(map[string]any)
	if rt["public_key"] != "pk" || rt["short_id"] != "sid" {
		t.Errorf("vless reality 不符: %v", rt)
	}
	if vl["tls"].(map[string]any)["utls"] == nil {
		t.Error("vless 缺少 uTLS")
	}

	if byTag["trojan1"]["password"] != "pw4" {
		t.Errorf("trojan 出站字段不符: %v", byTag["trojan1"])
	}

	hy := byTag["hy2"]
	if hy["password"] != "pw5" || hy["obfs"] == nil {
		t.Errorf("hysteria2 出站字段不符: %v", hy)
	}

	ranged := mustGenerate(t, Snapshot{Settings: DefaultSettings(), Nodes: []*Node{{
		ID: 7, Name: "hy-range", Protocol: "hysteria2", Server: "hy.example.com", Port: 20000, TLS: true, Enabled: true,
		Metadata: map[string]any{"password": "p", "server_ports": []string{"20000:50000"}},
	}}})
	var rangedOut map[string]any
	for _, raw := range ranged["outbounds"].([]any) {
		out := raw.(map[string]any)
		if out["tag"] == "hy-range" {
			rangedOut = out
			break
		}
	}
	if rangedOut["server_port"] != nil || fmt.Sprint(rangedOut["server_ports"]) != "[20000:50000]" {
		t.Errorf("hysteria2 server_ports 出站字段不符: %v", rangedOut)
	}

	tu := byTag["tuic1"]
	if tu["uuid"] != "u6" || tu["congestion_control"] != "bbr" {
		t.Errorf("tuic 出站字段不符: %v", tu)
	}

}

func TestHysteriaV1AuthIsOptional(t *testing.T) {
	n := &Node{Name: "hy", Protocol: "hysteria", Server: "hy.example.com", Port: 443, TLS: true, Enabled: true}
	ob, err := NodeToOutbound(n, "hy")
	if err != nil {
		t.Fatalf("无认证 Hysteria v1 不应生成失败: %v", err)
	}
	if _, ok := ob["auth_str"]; ok {
		t.Fatalf("空认证不应写入 auth_str: %v", ob)
	}
}

func TestSSROutboundRejected(t *testing.T) {
	_, err := Generate(Snapshot{Settings: DefaultSettings(), Nodes: []*Node{{
		Name: "ssr1", Protocol: "ssr", Server: "7.7.7.7", Port: 666, Enabled: true,
		Metadata: map[string]any{"method": "rc4-md5", "password": "pw7"},
	}}})
	if err == nil || !strings.Contains(err.Error(), "sing-box 不支持 SSR") {
		t.Fatalf("SSR 应返回不支持错误，得到: %v", err)
	}
}

func TestNodeTagDedup(t *testing.T) {
	nodes := []*Node{
		{ID: 1, Name: "Auto", Protocol: "trojan", Server: "1.1.1.1", Port: 443, Enabled: true,
			Metadata: map[string]any{"password": "p"}},
		{ID: 2, Name: "Auto", Protocol: "trojan", Server: "2.2.2.2", Port: 443, Enabled: true,
			Metadata: map[string]any{"password": "p"}},
	}
	groups := []*ProxyGroup{{ID: 10, Name: "Auto", Type: "select", Members: []ProxyGroupMember{{Type: "all"}}}}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), Nodes: nodes, ProxyGroups: groups})

	tags := map[string]bool{}
	for _, o := range m["outbounds"].([]any) {
		tag := o.(map[string]any)["tag"].(string)
		if tags[tag] {
			t.Errorf("出站 tag 重复: %s", tag)
		}
		tags[tag] = true
	}
	// 组名 Auto 优先，节点依次得 Auto-2 / Auto-3
	if !tags["Auto"] || !tags["Auto-2"] || !tags["Auto-3"] {
		t.Errorf("tag 去重结果不符: %v", tags)
	}
}

func TestDisabledNodeExcluded(t *testing.T) {
	nodes := []*Node{
		{ID: 1, Name: "on", Protocol: "trojan", Server: "1.1.1.1", Port: 443, Enabled: true,
			Metadata: map[string]any{"password": "p"}},
		{ID: 2, Name: "off", Protocol: "trojan", Server: "2.2.2.2", Port: 443, Enabled: false,
			Metadata: map[string]any{"password": "p"}},
	}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), Nodes: nodes})
	for _, o := range m["outbounds"].([]any) {
		if o.(map[string]any)["tag"] == "off" {
			t.Error("禁用节点不应生成出站")
		}
	}
}

func TestUnsupportedProtocolError(t *testing.T) {
	nodes := []*Node{{ID: 1, Name: "x", Protocol: "wireguard", Server: "1.1.1.1", Port: 51820, Enabled: true}}
	if _, err := Generate(Snapshot{Settings: DefaultSettings(), Nodes: nodes}); err == nil {
		t.Error("不支持的协议应返回错误")
	}
}

// --- 代理组 ---

func TestSelectorGroupDefault(t *testing.T) {
	nodes := []*Node{
		{ID: 1, Name: "n1", Protocol: "trojan", Server: "1.1.1.1", Port: 443, Enabled: true,
			Metadata: map[string]any{"password": "p"}},
		{ID: 2, Name: "n2", Protocol: "trojan", Server: "2.2.2.2", Port: 443, Enabled: true,
			Metadata: map[string]any{"password": "p"}},
	}
	groups := []*ProxyGroup{{
		ID: 10, Name: "Manual", Type: "select",
		Members:  []ProxyGroupMember{{Type: "node", ID: 1}, {Type: "node", ID: 2}},
		Selected: "node:2",
	}}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), Nodes: nodes, ProxyGroups: groups})
	g := findOutbound(t, m, "Manual")
	if g["type"] != "selector" {
		t.Fatalf("select 组应生成 selector，得到 %v", g["type"])
	}
	members := toStringSlice(t, g["outbounds"])
	if len(members) != 2 || members[0] != "n1" || members[1] != "n2" {
		t.Errorf("组成员不符: %v", members)
	}
	if g["default"] != "n2" {
		t.Errorf("selector default = %v, 期望 n2", g["default"])
	}
}

func TestURLTestGroup(t *testing.T) {
	groups := []*ProxyGroup{{
		ID: 10, Name: "Auto", Type: "urltest", TestURL: "http://example.com/204", IntervalS: 300,
		Members: []ProxyGroupMember{{Type: "all"}},
	}}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), ProxyGroups: groups})
	g := findOutbound(t, m, "Auto")
	if g["type"] != "urltest" {
		t.Fatalf("urltest 组类型不符: %v", g["type"])
	}
	if g["url"] != "http://example.com/204" || g["interval"] != "300s" {
		t.Errorf("urltest url/interval 不符: %v", g)
	}
	// 空节点池：all 展开为空，回退 direct
	if members := toStringSlice(t, g["outbounds"]); len(members) != 1 || members[0] != "direct" {
		t.Errorf("空组应回退 direct: %v", members)
	}
}

func TestNestedGroupAndCycle(t *testing.T) {
	nodes := []*Node{
		{ID: 1, Name: "n1", Protocol: "trojan", Server: "1.1.1.1", Port: 443, Enabled: true,
			Metadata: map[string]any{"password": "p"}},
	}
	groups := []*ProxyGroup{
		{ID: 10, Name: "Outer", Type: "select", Members: []ProxyGroupMember{{Type: "group", ID: 11}}},
		{ID: 11, Name: "Inner", Type: "select", Members: []ProxyGroupMember{{Type: "node", ID: 1}}},
	}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), Nodes: nodes, ProxyGroups: groups})
	outer := findOutbound(t, m, "Outer")
	if members := toStringSlice(t, outer["outbounds"]); len(members) != 1 || members[0] != "n1" {
		t.Errorf("嵌套组应展开为节点: %v", members)
	}

	// 循环引用
	cycle := []*ProxyGroup{
		{ID: 10, Name: "A", Type: "select", Members: []ProxyGroupMember{{Type: "group", ID: 11}}},
		{ID: 11, Name: "B", Type: "select", Members: []ProxyGroupMember{{Type: "group", ID: 10}}},
	}
	if _, err := Generate(Snapshot{Settings: DefaultSettings(), ProxyGroups: cycle}); err == nil {
		t.Error("循环引用应返回错误")
	}
}

func TestStaleSelectedOmitted(t *testing.T) {
	nodes := []*Node{
		{ID: 1, Name: "n1", Protocol: "trojan", Server: "1.1.1.1", Port: 443, Enabled: true,
			Metadata: map[string]any{"password": "p"}},
	}
	groups := []*ProxyGroup{{
		ID: 10, Name: "G", Type: "select",
		Members:  []ProxyGroupMember{{Type: "node", ID: 1}},
		Selected: "node:99", // 已删除的节点
	}}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), Nodes: nodes, ProxyGroups: groups})
	if g := findOutbound(t, m, "G"); g["default"] != nil {
		t.Errorf("失效选中项不应写入 default: %v", g["default"])
	}
}

// --- 路由 ---

func TestRouteRulesAndFinal(t *testing.T) {
	snap := testFullSnapshot(t)
	m := mustGenerate(t, snap)
	route := m["route"].(map[string]any)

	rules := route["rules"].([]any)
	// 期望：hijack-dns、sniff、内网直连、geosite-cn、geoip-cn、Telegram 合并规则、
	// AI 组的两条分类引用规则、final 兜底 reject（BLOCK final）。
	// 默认设置未开 ResolveIPRules，故无 resolve 规则。
	if len(rules) != 9 {
		t.Fatalf("期望 9 条路由规则，得到 %d: %v", len(rules), rules)
	}
	r0 := rules[0].(map[string]any)
	if r0["action"] != "hijack-dns" || !intSliceContains(r0["port"], 53) {
		t.Errorf("第一条应为 53 端口 DNS 劫持: %v", r0)
	}
	if rules[1].(map[string]any)["action"] != "sniff" {
		t.Errorf("第二条应为嗅探规则: %v", rules[1])
	}
	priv := rules[2].(map[string]any)
	if priv["ip_is_private"] != true || priv["outbound"] != "direct" {
		t.Errorf("第三条应为内网直连规则: %v", priv)
	}
	cn := rules[3].(map[string]any)
	if rs := toStringSlice(t, cn["rule_set"]); len(rs) != 1 || rs[0] != "geosite-cn" {
		t.Errorf("geosite-cn 规则不符: %v", cn)
	}
	if cn["outbound"] != "direct" {
		t.Errorf("中国大陆目标应为 direct: %v", cn)
	}
	cnIP := rules[4].(map[string]any)
	if rs := toStringSlice(t, cnIP["rule_set"]); len(rs) != 1 || rs[0] != "geoip-cn" {
		t.Errorf("geoip-cn 规则不符: %v", cnIP)
	}
	tg := rules[5].(map[string]any)
	if tg["outbound"] != "Auto" {
		t.Errorf("Telegram 目标应为 Auto: %v", tg)
	}
	// 内置分类引用在规则里以派生 tag 出现（4.3）
	ai := rules[6].(map[string]any)
	if rs := toStringSlice(t, ai["rule_set"]); len(rs) != 1 || rs[0] != "geosite-category-ai-!cn" {
		t.Errorf("分类引用应生成派生 tag: %v", ai)
	}
	acl := rules[7].(map[string]any)
	if rs := toStringSlice(t, acl["rule_set"]); len(rs) != 1 || rs[0] != "acl-Netflix" {
		t.Errorf("acl 分类引用应生成派生 tag: %v", acl)
	}
	last := rules[8].(map[string]any)
	if last["action"] != "reject" {
		t.Errorf("BLOCK final 应生成 reject 兜底: %v", last)
	}
	if route["final"] != nil {
		t.Errorf("BLOCK final 不应再写 route.final: %v", route["final"])
	}

	// rule_set 引用
	ruleSets := route["rule_set"].([]any)
	tags := map[string]bool{}
	for _, r := range ruleSets {
		item := r.(map[string]any)
		tags[item["tag"].(string)] = true
		if item["type"] == "local" && item["path"] == "" {
			t.Errorf("local 规则集缺 path: %v", item)
		}
		if item["type"] == "remote" && item["url"] == "" {
			t.Errorf("remote 规则集缺 url: %v", item)
		}
	}
	if !tags["geosite-cn"] || !tags["geoip-cn"] || !tags["geosite-telegram"] || !tags["geoip-telegram"] {
		t.Errorf("rule_set 引用不全: %v", tags)
	}
	// 内置分类同样要生成条目（否则核心报 rule-set not found）
	if !tags["geosite-category-ai-!cn"] || !tags["acl-Netflix"] {
		t.Errorf("内置分类的 rule_set 条目缺失: %v", tags)
	}
	if route["final"] == nil && route["default_domain_resolver"] == nil {
		t.Error("缺少 default_domain_resolver")
	}
	if route["default_domain_resolver"] != "local" {
		t.Errorf("default_domain_resolver = %v, 期望 local", route["default_domain_resolver"])
	}
}

func TestRouteFinalGroup(t *testing.T) {
	groups := []*ProxyGroup{{ID: 10, Name: "Auto", Type: "select", Members: []ProxyGroupMember{{Type: "all"}}}}
	routings := []*RoutingGroup{{
		Name: "Final", Target: "Auto", Enabled: true,
		Rules: []Rule{{Type: "final", Enabled: true}},
	}}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), ProxyGroups: groups, RoutingGroups: routings})
	route := m["route"].(map[string]any)
	if route["final"] != "Auto" {
		t.Errorf("route.final = %v, 期望 Auto", route["final"])
	}
}

func TestDNSKaringStyleAddresses(t *testing.T) {
	dns := &DNSConfig{Servers: []DNSServer{
		{Tag: "v4", Type: "udp", Address: "udp://1.1.1.1:5353", Enabled: true},
		{Tag: "v6", Type: "udp", Address: "udp://[2001:4860:4860::8888]", Enabled: true},
	}}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), DNS: dns})
	servers := m["dns"].(map[string]any)["servers"].([]any)
	v4 := servers[0].(map[string]any)
	v6 := servers[1].(map[string]any)
	if v4["server"] != "1.1.1.1" || v4["server_port"] != float64(5353) {
		t.Errorf("IPv4 DNS 地址解析不符: %v", v4)
	}
	if v6["server"] != "2001:4860:4860::8888" {
		t.Errorf("IPv6 DNS 地址解析不符: %v", v6)
	}
}

func TestRouteRejectsMultipleActiveFinalGroups(t *testing.T) {
	routings := []*RoutingGroup{
		{Name: "Final 1", Target: "DIRECT", Enabled: true, Rules: []Rule{{Type: "final", Enabled: true}}},
		{Name: "Final 2", Target: "DIRECT", Enabled: true, Rules: []Rule{{Type: "final", Enabled: true}}},
	}
	if _, err := Generate(Snapshot{Settings: DefaultSettings(), RoutingGroups: routings}); err == nil {
		t.Fatal("多个活动 final 应返回错误")
	}
}

func TestRouteNoFinalDefaultsDirect(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "domain_suffix", Value: "example.com", Enabled: true}},
	}}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), RoutingGroups: routings})
	route := m["route"].(map[string]any)
	if route["final"] != "direct" {
		t.Errorf("无 final 规则时 route.final = %v, 期望 direct", route["final"])
	}
}

func TestRouteUnknownTargetError(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "NoSuchGroup", Enabled: true,
		Rules: []Rule{{Type: "domain_suffix", Value: "example.com", Enabled: true}},
	}}
	if _, err := Generate(Snapshot{Settings: DefaultSettings(), RoutingGroups: routings}); err == nil {
		t.Error("未知分流目标应返回错误")
	}
}

func TestRouteMissingRuleSetError(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "rule_set", Value: "no-such-ruleset", Enabled: true}},
	}}
	if _, err := Generate(Snapshot{Settings: DefaultSettings(), RoutingGroups: routings}); err == nil {
		t.Error("引用不存在的规则集应返回错误")
	}
}

func TestRouteDisabledGroupSkipped(t *testing.T) {
	routings := []*RoutingGroup{
		{Name: "off", Target: "BLOCK", Enabled: false,
			Rules: []Rule{{Type: "domain_suffix", Value: "ads.example.com", Enabled: true}}},
		{Name: "Final", Target: "DIRECT", Enabled: true,
			Rules: []Rule{{Type: "final", Enabled: true}}},
	}
	set := DefaultSettings()
	set.PrivateDirect = false // 只关心用户规则是否生成，排除内网直连规则
	m := mustGenerate(t, Snapshot{Settings: set, RoutingGroups: routings})
	route := m["route"].(map[string]any)
	rules := route["rules"].([]any)
	if len(rules) != 1 { // 仅 sniff（无 DNS 服务器时无 hijack-dns）
		t.Fatalf("禁用分流组的规则不应生成，得到 %v", rules)
	}
	if route["final"] != "direct" {
		t.Errorf("route.final = %v, 期望 direct", route["final"])
	}
}

func TestRouteInvertAndMultiValue(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "domain_suffix", Value: "a.com, b.com ,", Invert: true, Enabled: true}},
	}}
	set := DefaultSettings()
	set.PrivateDirect = false // 让用户规则稳定落在 rules[1]
	m := mustGenerate(t, Snapshot{Settings: set, RoutingGroups: routings})
	// rules[0] 为 sniff 规则（无 DNS 服务器时无 hijack-dns），用户规则在 [1]
	rule := m["route"].(map[string]any)["rules"].([]any)[1].(map[string]any)
	got := toStringSlice(t, rule["domain_suffix"])
	if len(got) != 2 || got[0] != "a.com" || got[1] != "b.com" {
		t.Errorf("多值拆分不符: %v", got)
	}
	if rule["invert"] != true {
		t.Error("invert 未写入")
	}
}

// TestRouteDomainRegex domain_regex 的值整体作为一条正则，不按逗号拆分
// （正则自身可能含逗号，如 a{1,3}）。
func TestRouteDomainRegex(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "BLOCK", Enabled: true,
		Rules: []Rule{{Type: "domain_regex", Value: `^ads\.[a-z]{2,4}\.com$`, Enabled: true}},
	}}
	set := DefaultSettings()
	set.PrivateDirect = false
	m := mustGenerate(t, Snapshot{Settings: set, RoutingGroups: routings})
	rule := routeRules(t, m)[1]
	got := toStringSlice(t, rule["domain_regex"])
	if len(got) != 1 || got[0] != `^ads\.[a-z]{2,4}\.com$` {
		t.Errorf("domain_regex 不应按逗号拆分: %v", got)
	}
	if rule["action"] != "reject" {
		t.Errorf("BLOCK 目标应生成 reject: %v", rule)
	}
}

func TestRouteDomainRegexInLogicalRule(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "logical", Mode: "and", Enabled: true, Conditions: []RuleCondition{
			{Type: "domain_regex", Value: `^cdn[0-9]+\.example\.com$`},
			{Type: "domain_suffix", Value: "example.com"},
		}}},
	}}
	set := DefaultSettings()
	set.PrivateDirect = false
	m := mustGenerate(t, Snapshot{Settings: set, RoutingGroups: routings})
	rule := routeRules(t, m)[1]
	if rule["type"] != "logical" || rule["mode"] != "and" {
		t.Fatalf("逻辑规则字段不符: %v", rule)
	}
	subs := rule["rules"].([]any)
	if len(subs) != 2 {
		t.Fatalf("期望 2 个子条件，得到 %v", subs)
	}
	got := toStringSlice(t, subs[0].(map[string]any)["domain_regex"])
	if len(got) != 1 || got[0] != `^cdn[0-9]+\.example\.com$` {
		t.Errorf("逻辑子条件 domain_regex 不符: %v", got)
	}
}

// TestRouteDomainRegexNeedsNoResolve domain_regex 是域名类条件，不应触发 resolve。
func TestRouteDomainRegexNeedsNoResolve(t *testing.T) {
	r := Rule{Type: "domain_regex", Value: `^a\.com$`}
	if ruleNeedsResolve(&r) {
		t.Error("domain_regex 属域名类条件，不应触发 resolve")
	}
}

// --- 内网直连与 resolve ---

// routeRules 取出生成配置的路由规则列表。
func routeRules(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range m["route"].(map[string]any)["rules"].([]any) {
		out = append(out, r.(map[string]any))
	}
	return out
}

// ruleActions 把路由规则压成便于断言顺序的字符串序列。
func ruleActions(rules []map[string]any) []string {
	var out []string
	for _, r := range rules {
		switch {
		case r["action"] != nil:
			out = append(out, r["action"].(string))
		case r["ip_is_private"] == true:
			out = append(out, "ip_is_private")
		case r["rule_set"] != nil:
			tags, _ := r["rule_set"].([]any)
			if len(tags) > 0 {
				out = append(out, "rule_set:"+tags[0].(string))
				continue
			}
			out = append(out, "rule_set")
		default:
			out = append(out, "rule")
		}
	}
	return out
}

func TestPrivateDirectDisabled(t *testing.T) {
	set := DefaultSettings()
	set.PrivateDirect = false
	m := mustGenerate(t, Snapshot{Settings: set})
	for _, r := range routeRules(t, m) {
		if r["ip_is_private"] != nil {
			t.Errorf("关闭内网直连后不应生成 ip_is_private 规则: %v", r)
		}
	}
}

// TestPrivateDirectBeforeUserRules 内网直连须早于用户规则，否则会被前置的
// 广域规则（如 final 之前的兜底代理规则）抢先命中。
func TestPrivateDirectBeforeUserRules(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "domain_suffix", Value: "example.com", Enabled: true}},
	}}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), RoutingGroups: routings})
	got := ruleActions(routeRules(t, m))
	want := []string{"sniff", "ip_is_private", "rule"}
	if len(got) != len(want) {
		t.Fatalf("规则序列 = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("规则序列 = %v, 期望 %v", got, want)
		}
	}
}

// TestResolveInsertedBeforeFirstIPRule resolve 须插在第一条 IP 类规则之前、
// 其前的域名类规则之后——域名规则先终结匹配，可减少受影响的连接。
func TestResolveInsertedBeforeFirstIPRule(t *testing.T) {
	ruleSets := []*RuleSet{
		{ID: 1, Tag: "geosite-cn", Name: "cn 域名", SourceType: "remote", Format: "srs",
			URL: "https://example.com/cn.srs", Enabled: true},
		{ID: 2, Tag: "geoip-cn", Name: "cn IP", SourceType: "remote", Format: "srs",
			URL: "https://example.com/geoip-cn.srs", Enabled: true},
	}
	routings := []*RoutingGroup{{
		Name: "中国大陆", Target: "DIRECT", Enabled: true,
		Rules: []Rule{
			{Type: "rule_set", Value: "geosite-cn", Enabled: true},
			{Type: "rule_set", Value: "geoip-cn", Enabled: true},
		},
	}}
	set := DefaultSettings()
	set.PrivateDirect = false
	set.ResolveIPRules = true
	m := mustGenerate(t, Snapshot{Settings: set, RuleSets: ruleSets, RoutingGroups: routings})

	got := ruleActions(routeRules(t, m))
	want := []string{"sniff", "rule_set:geosite-cn", "resolve", "rule_set:geoip-cn"}
	if len(got) != len(want) {
		t.Fatalf("规则序列 = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("规则序列 = %v, 期望 %v", got, want)
		}
	}
	// 未开 FakeIP 时 resolve 不指定 server，解析走 DNS 规则（国内域名可用国内 DNS）
	if srv := routeRules(t, m)[2]["server"]; srv != nil {
		t.Errorf("未开 FakeIP 时 resolve 不应指定 server: %v", srv)
	}
}

func TestResolveNotGeneratedWithoutIPCondition(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "domain_suffix", Value: "example.com", Enabled: true}},
	}}
	set := DefaultSettings()
	set.ResolveIPRules = true
	m := mustGenerate(t, Snapshot{Settings: set, RoutingGroups: routings})
	for _, r := range routeRules(t, m) {
		if r["action"] == "resolve" {
			t.Error("无 IP 类条件时不应生成 resolve 规则")
		}
	}
}

func TestResolveDisabledByDefault(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "ip_cidr", Value: "10.0.0.0/8", Enabled: true}},
	}}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), RoutingGroups: routings})
	for _, r := range routeRules(t, m) {
		if r["action"] == "resolve" {
			t.Error("默认设置不应生成 resolve 规则")
		}
	}
}

// TestResolveWithFakeIPPinsServer FakeIP 开启时 resolve 必须显式指定 DNS 服务器，
// 否则解析会命中生成器追加的 FakeIP 规则、拿回 FakeIP 段地址，IP 类条件依旧不生效。
func TestResolveWithFakeIPPinsServer(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "geoip", Value: "cn", Enabled: true}},
	}}
	dns := &DNSConfig{
		Servers: []DNSServer{
			{ID: 1, Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true},
			{ID: 2, Tag: "remote", Type: "https", Address: "8.8.8.8", Enabled: true},
		},
		Final:         "remote",
		FakeIPEnabled: true,
	}
	set := DefaultSettings()
	set.ResolveIPRules = true
	m := mustGenerate(t, Snapshot{Settings: set, RoutingGroups: routings, DNS: dns})

	var resolve map[string]any
	for _, r := range routeRules(t, m) {
		if r["action"] == "resolve" {
			resolve = r
			break
		}
	}
	if resolve == nil {
		t.Fatal("未生成 resolve 规则")
	}
	if resolve["server"] != "remote" {
		t.Errorf("resolve.server = %v, 期望 DNS final 服务器 remote", resolve["server"])
	}
}

func TestRuleNeedsResolve(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"ip_cidr", Rule{Type: "ip_cidr", Value: "10.0.0.0/8"}, true},
		{"geoip", Rule{Type: "geoip", Value: "cn"}, true},
		{"IP 规则集", Rule{Type: "rule_set", Value: "geoip-cn"}, true},
		{"多值含 IP 规则集", Rule{Type: "rule_set", Value: "geosite-cn, geoip-cn"}, true},
		{"域名", Rule{Type: "domain_suffix", Value: "example.com"}, false},
		{"域名规则集", Rule{Type: "rule_set", Value: "geosite-cn"}, false},
		{"逻辑规则含 IP 条件", Rule{Type: "logical", Mode: "and", Conditions: []RuleCondition{
			{Type: "domain_suffix", Value: "example.com"},
			{Type: "geoip", Value: "cn"},
		}}, true},
		{"逻辑规则纯域名", Rule{Type: "logical", Mode: "or", Conditions: []RuleCondition{
			{Type: "domain_suffix", Value: "example.com"},
			{Type: "domain_keyword", Value: "ample"},
		}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ruleNeedsResolve(&c.rule); got != c.want {
				t.Errorf("ruleNeedsResolve = %v, 期望 %v", got, c.want)
			}
		})
	}
}

// TestRuleSetReferencedOnlyByDNS 只被 DNS 规则引用的规则集也必须生成 route.rule_set 条目，
// 否则 sing-box 启动时报 "rule-set not found"。
func TestRuleSetReferencedOnlyByDNS(t *testing.T) {
	ruleSets := []*RuleSet{
		{ID: 1, Name: "cn 域名", Tag: "geosite-cn", SourceType: "remote", Format: "srs",
			URL: "https://example.com/cn.srs", Enabled: true},
	}
	// 路由规则不引用任何规则集，仅 DNS 规则引用
	routings := []*RoutingGroup{{
		Name: "Final", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "final", Enabled: true}},
	}}
	dns := &DNSConfig{
		Servers: []DNSServer{
			{ID: 1, Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true},
			{ID: 2, Tag: "remote", Type: "https", Address: "8.8.8.8", Enabled: true},
		},
		Rules: []DNSRule{{Type: "rule_set", Value: "geosite-cn", Server: "local", Enabled: true}},
		Final: "remote",
	}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), RuleSets: ruleSets,
		RoutingGroups: routings, DNS: dns})

	items, ok := m["route"].(map[string]any)["rule_set"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("期望 1 个 rule_set 条目，得到 %v", m["route"].(map[string]any)["rule_set"])
	}
	if tag := items[0].(map[string]any)["tag"]; tag != "geosite-cn" {
		t.Errorf("rule_set tag = %v, 期望 geosite-cn", tag)
	}
}

// --- 规则集 ---

func TestRuleSetLocalCachePreferred(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "geosite-cn.srs")
	if err := os.WriteFile(cache, []byte("fake-srs"), 0o644); err != nil {
		t.Fatal(err)
	}
	ruleSets := []*RuleSet{
		{ID: 1, Name: "cn", Tag: "geosite-cn", SourceType: "remote", Format: "srs",
			URL: "https://example.com/cn.srs", Enabled: true, CachedPath: cache},
		{ID: 2, Name: "tg", Tag: "geosite-telegram", SourceType: "remote", Format: "srs",
			URL: "https://example.com/tg.srs", Enabled: true}, // 无缓存 → remote
	}
	routings := []*RoutingGroup{
		{Name: "G", Target: "DIRECT", Enabled: true,
			Rules: []Rule{{Type: "rule_set", Value: "geosite-cn,geosite-telegram", Enabled: true}}},
	}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), RuleSets: ruleSets, RoutingGroups: routings})
	items := m["route"].(map[string]any)["rule_set"].([]any)
	byTag := map[string]map[string]any{}
	for _, r := range items {
		item := r.(map[string]any)
		byTag[item["tag"].(string)] = item
	}
	cn := byTag["geosite-cn"]
	if cn["type"] != "local" || cn["path"] != cache || cn["format"] != "binary" {
		t.Errorf("有缓存应生成 local 引用: %v", cn)
	}
	tg := byTag["geosite-telegram"]
	if tg["type"] != "remote" || tg["url"] == "" || tg["download_detour"] != nil {
		t.Errorf("无缓存应生成 remote 引用: %v", tg)
	}
}

// --- 逻辑规则 ---

func TestRouteLogicalRule(t *testing.T) {
	ruleSets := []*RuleSet{
		{ID: 1, Name: "cn", Tag: "geosite-cn", SourceType: "remote", Format: "srs",
			URL: "https://example.com/cn.srs", Enabled: true},
	}
	groups := []*ProxyGroup{{ID: 1, Name: "Auto", Type: "urltest"}}
	routings := []*RoutingGroup{{
		Name: "G", Target: "Auto", Enabled: true,
		Rules: []Rule{{
			Type: "logical", Mode: "and", Enabled: true,
			Conditions: []RuleCondition{
				{Type: "rule_set", Value: "geosite-cn"},
				{Type: "ip_cidr", Value: "10.0.0.0/8", Invert: true},
			},
		}},
	}}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), ProxyGroups: groups, RuleSets: ruleSets, RoutingGroups: routings})
	rules := m["route"].(map[string]any)["rules"].([]any)
	var r map[string]any
	for _, item := range rules {
		if item.(map[string]any)["type"] == "logical" {
			r = item.(map[string]any)
			break
		}
	}
	if r == nil {
		t.Fatalf("未找到逻辑规则: %v", rules)
	}
	if r["type"] != "logical" || r["mode"] != "and" {
		t.Errorf("逻辑规则 type/mode 不符: %v / %v", r["type"], r["mode"])
	}
	if r["outbound"] != "Auto" {
		t.Errorf("outbound = %v, 期望 Auto", r["outbound"])
	}
	subs := r["rules"].([]any)
	if len(subs) != 2 {
		t.Fatalf("子条件数 = %d, 期望 2", len(subs))
	}
	sub0 := subs[0].(map[string]any)
	if sub0["rule_set"] == nil || sub0["action"] != nil || sub0["outbound"] != nil {
		t.Errorf("子条件 0 应仅含匹配字段: %v", sub0)
	}
	sub1 := subs[1].(map[string]any)
	if sub1["invert"] != true {
		t.Errorf("子条件 1 应反转: %v", sub1)
	}
}

func TestRouteLogicalRuleInvertAndOr(t *testing.T) {
	routings := []*RoutingGroup{{
		Name: "G", Target: "BLOCK", Enabled: true,
		Rules: []Rule{{
			Type: "logical", Mode: "or", Invert: true, Enabled: true,
			Conditions: []RuleCondition{
				{Type: "domain_suffix", Value: "example.com"},
				{Type: "domain_keyword", Value: "ads"},
			},
		}},
	}}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), RoutingGroups: routings})
	var r map[string]any
	for _, item := range m["route"].(map[string]any)["rules"].([]any) {
		if item.(map[string]any)["type"] == "logical" {
			r = item.(map[string]any)
			break
		}
	}
	if r == nil {
		t.Fatal("未找到逻辑规则")
	}
	if r["mode"] != "or" || r["invert"] != true {
		t.Errorf("mode/invert 不符: %v / %v", r["mode"], r["invert"])
	}
	if r["action"] != "reject" {
		t.Errorf("BLOCK 目标应生成 reject 动作: %v", r["action"])
	}
}

func TestRouteLogicalRuleErrors(t *testing.T) {
	// 缺子条件
	routings := []*RoutingGroup{{
		Name: "G", Target: "DIRECT", Enabled: true,
		Rules: []Rule{{Type: "logical", Mode: "and", Enabled: true}},
	}}
	if _, err := Generate(Snapshot{Settings: DefaultSettings(), RoutingGroups: routings}); err == nil {
		t.Error("缺子条件的逻辑规则应返回错误")
	}
	// 非法 mode
	routings[0].Rules[0].Mode = "xor"
	routings[0].Rules[0].Conditions = []RuleCondition{{Type: "domain", Value: "a.com"}}
	if _, err := Generate(Snapshot{Settings: DefaultSettings(), RoutingGroups: routings}); err == nil {
		t.Error("非法组合方式应返回错误")
	}
}

// --- DNS ---

func TestDNSNewFormat(t *testing.T) {
	snap := testFullSnapshot(t)
	m := mustGenerate(t, snap)
	dns := m["dns"].(map[string]any)

	servers := dns["servers"].([]any)
	if len(servers) != 2 {
		t.Fatalf("期望 2 个 DNS 服务器，得到 %v", servers)
	}
	local := servers[0].(map[string]any)
	if local["type"] != "udp" || local["tag"] != "local" || local["server"] != "223.5.5.5" {
		t.Errorf("local 服务器不符: %v", local)
	}
	remote := servers[1].(map[string]any)
	if remote["type"] != "https" || remote["server"] != "8.8.8.8" {
		t.Errorf("remote 服务器不符: %v", remote)
	}
	if remote["domain_resolver"] != "local" {
		t.Errorf("remote 缺少 domain_resolver: %v", remote)
	}
	tls := remote["tls"].(map[string]any)
	if tls["enabled"] != true || tls["server_name"] != "8.8.8.8" {
		t.Errorf("remote TLS 不符: %v", tls)
	}

	if dns["final"] != "remote" {
		t.Errorf("dns.final = %v, 期望 remote", dns["final"])
	}
	if dns["strategy"] != "prefer_ipv4" {
		t.Errorf("dns.strategy = %v", dns["strategy"])
	}

	rules := dns["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("期望 1 条 DNS 规则，得到 %v", rules)
	}
	r0 := rules[0].(map[string]any)
	if rs := toStringSlice(t, r0["rule_set"]); len(rs) != 1 || rs[0] != "geosite-cn" {
		t.Errorf("DNS 规则 rule_set 不符: %v", r0)
	}
	if r0["server"] != "local" {
		t.Errorf("DNS 规则服务器不符: %v", r0)
	}
}

func TestDNSAddressForms(t *testing.T) {
	cfg := &DNSConfig{
		Servers: []DNSServer{
			{ID: 1, Tag: "d1", Type: "udp", Address: "1.1.1.1:53", Enabled: true},
			{ID: 2, Tag: "d2", Type: "https", Address: "https://dns.google/dns-query", Enabled: true},
			{ID: 3, Tag: "d3", Type: "tls", Address: "dns.google:853", Enabled: true},
			{ID: 4, Tag: "d4", Type: "local", Address: "-", Enabled: true},
		},
	}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), DNS: cfg})
	srvs := m["dns"].(map[string]any)["servers"].([]any)
	byTag := map[string]map[string]any{}
	for _, s := range srvs {
		item := s.(map[string]any)
		byTag[item["tag"].(string)] = item
	}
	if byTag["d1"]["server"] != "1.1.1.1" || byTag["d1"]["server_port"] != float64(53) {
		t.Errorf("udp host:port 解析不符: %v", byTag["d1"])
	}
	d2 := byTag["d2"]
	if d2["server"] != "dns.google" || d2["path"] != "/dns-query" {
		t.Errorf("https URL 解析不符: %v", d2)
	}
	if d2["server_port"] != nil {
		t.Errorf("https URL 未带端口时不应写 server_port: %v", d2)
	}
	if byTag["d3"]["server"] != "dns.google" || byTag["d3"]["server_port"] != float64(853) {
		t.Errorf("tls host:port 解析不符: %v", byTag["d3"])
	}
	if _, ok := byTag["d4"]["server"]; ok {
		t.Errorf("local 类型不应有 server 字段: %v", byTag["d4"])
	}
}

func TestDNSFakeIP(t *testing.T) {
	cfg := &DNSConfig{
		Servers: []DNSServer{
			{ID: 1, Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true},
		},
		Final: "local", Strategy: "prefer_ipv4",
		FakeIPEnabled: true, FakeIPRange: "198.18.0.0/15",
		Rules: []DNSRule{{Type: "rule_set", Value: "geosite-cn", Server: "local", Enabled: true}},
	}
	ruleSets := []*RuleSet{{ID: 1, Name: "cn", Tag: "geosite-cn", SourceType: "remote", Format: "srs",
		URL: "https://example.com/cn.srs", Enabled: true}}
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings(), DNS: cfg, RuleSets: ruleSets})

	dns := m["dns"].(map[string]any)
	srvs := dns["servers"].([]any)
	if len(srvs) != 2 {
		t.Fatalf("FakeIP 启用时应追加 fakeip 服务器: %v", srvs)
	}
	fake := srvs[1].(map[string]any)
	if fake["type"] != "fakeip" || fake["inet4_range"] != "198.18.0.0/15" {
		t.Errorf("fakeip 服务器不符: %v", fake)
	}
	rules := dns["rules"].([]any)
	if len(rules) != 2 {
		t.Fatalf("FakeIP 规则应追加在用户规则之后: %v", rules)
	}
	last := rules[1].(map[string]any)
	qt := toStringSlice(t, last["query_type"])
	if len(qt) != 2 || qt[0] != "A" || qt[1] != "AAAA" || last["server"] != "fakeip" {
		t.Errorf("FakeIP query_type 规则不符: %v", last)
	}
}

func TestDNSOmittedWithoutServers(t *testing.T) {
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings()})
	if _, ok := m["dns"]; ok {
		t.Error("无 DNS 服务器时不应生成 dns 段")
	}
}

func TestDNSReferencesMustResolveToEnabledServersAndOutbounds(t *testing.T) {
	set := DefaultSettings()
	base := func() *DNSConfig {
		return &DNSConfig{Servers: []DNSServer{
			{Tag: "local", Type: "udp", Address: "1.1.1.1", Enabled: true},
			{Tag: "remote", Type: "https", Address: "dns.example.com", AddressResolver: "missing", Enabled: true},
		}, Final: "local"}
	}
	if _, err := Generate(Snapshot{Settings: set, DNS: base()}); err == nil {
		t.Fatal("缺失的 AddressResolver 应导致配置生成失败")
	}
	cfg := base()
	cfg.Servers[1].AddressResolver = "local"
	cfg.Servers[1].Detour = "missing-outbound"
	if _, err := Generate(Snapshot{Settings: set, DNS: cfg}); err == nil {
		t.Fatal("缺失的 DNS Detour 应导致配置生成失败")
	}
}

// --- 入站 ---

func TestOnlyMixedInbound(t *testing.T) {
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings()})
	inbounds := m["inbounds"].([]any)
	if len(inbounds) != 1 {
		t.Fatalf("不实现 TUN 时应仅生成 mixed 入站，得到 %v", inbounds)
	}
	mixed := inbounds[0].(map[string]any)
	if mixed["type"] != "mixed" || mixed["tag"] != "mixed-in" {
		t.Errorf("mixed 入站字段不符: %v", mixed)
	}
}

// --- 辅助 ---

func findOutbound(t *testing.T, m map[string]any, tag string) map[string]any {
	t.Helper()
	for _, o := range m["outbounds"].([]any) {
		ob := o.(map[string]any)
		if ob["tag"] == tag {
			return ob
		}
	}
	t.Fatalf("找不到出站 %q", tag)
	return nil
}

func toStringSlice(t *testing.T, v any) []string {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		if v == nil {
			return nil
		}
		t.Fatalf("期望数组，得到 %v (%T)", v, v)
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("期望字符串元素，得到 %v", e)
		}
		out = append(out, s)
	}
	return out
}

func intSliceContains(v any, want int) bool {
	arr, ok := v.([]any)
	if !ok {
		return false
	}
	for _, e := range arr {
		if f, ok := e.(float64); ok && int(f) == want {
			return true
		}
	}
	return false
}

// testFullSnapshot 构造覆盖节点/组/分流/规则集/DNS 的完整快照，
// 对应默认分流方案：中国大陆→DIRECT、Telegram→Auto、Final→BLOCK。
func testFullSnapshot(t *testing.T) Snapshot {
	t.Helper()
	nodes := []*Node{
		{ID: 1, Name: "hk-01", Protocol: "trojan", Server: "1.1.1.1", Port: 443, TLS: true, Enabled: true,
			Metadata: map[string]any{"password": "p1"}},
		{ID: 2, Name: "us-01", Protocol: "vless", Server: "2.2.2.2", Port: 443, TLS: true, Enabled: true,
			Metadata: map[string]any{"uuid": "u2"}},
	}
	groups := []*ProxyGroup{{
		ID: 10, Name: "Auto", Type: "urltest", IntervalS: 300,
		Members: []ProxyGroupMember{{Type: "all"}},
	}}
	ruleSets := []*RuleSet{
		{ID: 1, Name: "中国大陆域名", Tag: "geosite-cn", SourceType: "remote", Format: "srs",
			URL: "https://example.com/cn.srs", Enabled: true},
		{ID: 2, Name: "中国大陆 IP", Tag: "geoip-cn", SourceType: "remote", Format: "srs",
			URL: "https://example.com/geoip-cn.srs", Enabled: true},
		{ID: 3, Name: "Telegram 域名", Tag: "geosite-telegram", SourceType: "remote", Format: "srs",
			URL: "https://example.com/tg.srs", Enabled: true},
		{ID: 4, Name: "Telegram IP", Tag: "geoip-telegram", SourceType: "remote", Format: "srs",
			URL: "https://example.com/tgip.srs", Enabled: true},
	}
	routings := []*RoutingGroup{
		{ID: 1, Name: "中国大陆", Target: "DIRECT", Enabled: true, Rules: []Rule{
			{Type: "rule_set", Value: "geosite-cn", Enabled: true},
			{Type: "rule_set", Value: "geoip-cn", Enabled: true},
		}},
		{ID: 2, Name: "Telegram", Target: "Auto", Enabled: true, Rules: []Rule{
			{Type: "rule_set", Value: "geosite-telegram, geoip-telegram", Enabled: true},
		}},
		// 内置分类引用（4.3）：不在 RuleSets 表里，由分类库按需生成条目。
		// 与上面的自定义规则集并存，让基线同时覆盖两条生成路径。
		{ID: 3, Name: "AI", Target: "Auto", Enabled: true, Rules: []Rule{
			{Type: "rule_set", Value: "geosite:category-ai-!cn", Enabled: true},
			{Type: "rule_set", Value: "acl:Netflix", Enabled: true},
		}},
		{ID: 4, Name: "Final", Target: "BLOCK", Enabled: true, Rules: []Rule{
			{Type: "final", Enabled: true},
		}},
	}
	cfg := &DNSConfig{
		Strategy: "prefer_ipv4",
		Servers: []DNSServer{
			{ID: 1, Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true},
			{ID: 2, Tag: "remote", Type: "https", Address: "8.8.8.8", AddressResolver: "local", Enabled: true},
		},
		Rules: []DNSRule{{Type: "rule_set", Value: "geosite-cn", Server: "local", Enabled: true}},
		Final: "remote",
	}
	return Snapshot{
		Settings:      DefaultSettings(),
		Nodes:         nodes,
		ProxyGroups:   groups,
		RoutingGroups: routings,
		RuleSets:      ruleSets,
		DNS:           cfg,
	}
}

// TestGeneratedConfigKeys 验证顶层键顺序稳定（幂等的可读性保障）。
func TestGeneratedConfigKeys(t *testing.T) {
	out, err := Generate(testFullSnapshot(t))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(out)
	for _, key := range []string{`"log"`, `"dns"`, `"inbounds"`, `"outbounds"`, `"route"`} {
		if !strings.Contains(s, key) {
			t.Errorf("输出缺少顶层键 %s", key)
		}
	}
}

// TestGenerateClashAPI 验证 clash API 段：默认开启、端口 0 关闭、始终仅监听回环地址。
func TestGenerateClashAPI(t *testing.T) {
	// 默认设置：127.0.0.1:9090
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings()})
	exp, ok := m["experimental"].(map[string]any)
	if !ok {
		t.Fatalf("默认设置应生成 experimental 段: %v", m["experimental"])
	}
	api, ok := exp["clash_api"].(map[string]any)
	if !ok {
		t.Fatalf("experimental 缺少 clash_api: %v", exp)
	}
	if api["external_controller"] != "127.0.0.1:9090" {
		t.Errorf("external_controller 不符: %v", api["external_controller"])
	}
	if _, has := api["secret"]; has {
		t.Errorf("未设密钥时不应输出 secret")
	}

	// 端口 0：关闭
	s := DefaultSettings()
	s.ClashAPIPort = 0
	m = mustGenerate(t, Snapshot{Settings: s})
	if _, has := m["experimental"]; has {
		t.Errorf("端口为 0 时不应生成 experimental 段")
	}

	// AllowLAN 只影响代理入站，不暴露管理 API。
	s = DefaultSettings()
	s.AllowLAN = true
	s.ClashAPISecret = "s3cret"
	m = mustGenerate(t, Snapshot{Settings: s})
	api = m["experimental"].(map[string]any)["clash_api"].(map[string]any)
	if api["external_controller"] != "127.0.0.1:9090" {
		t.Errorf("AllowLAN 时 external_controller 不符: %v", api["external_controller"])
	}
	if api["secret"] != "s3cret" {
		t.Errorf("secret 不符: %v", api["secret"])
	}
}
