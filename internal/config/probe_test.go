package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// --- 测速配置生成（probe.go） ---
//
// 这些测试的判据是「独立测速是否自带域名解析环境」，也就是本次故障的回归门。
// 把 probe.go 里 dns / route.default_domain_resolver 的输出删掉，T1/T2/T4 必须变红。

func mustProbe(t *testing.T, p LatencyProbe) (map[string]any, *LatencyProbePlan) {
	t.Helper()
	plan, err := GenerateLatencyProbe(p)
	if err != nil {
		t.Fatalf("GenerateLatencyProbe: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(plan.Data, &m); err != nil {
		t.Fatalf("测速配置不是合法 JSON: %v\n%s", err, plan.Data)
	}
	return m, plan
}

// trojanNode 服务器为域名的 trojan 节点：域名解析正是本次故障的触发条件。
func trojanNode(id int64, server string) *Node {
	return &Node{
		ID: id, Name: "n", Protocol: "trojan", Server: server, Port: 443, Enabled: true,
		Metadata: map[string]any{"password": "p"},
	}
}

func probeDNS(dns *DNSConfig, nodes ...*Node) LatencyProbe {
	return LatencyProbe{Nodes: nodes, DNS: dns, APIPort: 39000}
}

// probeDNSServers 取 dns.servers；缺失时直接判失败——测试里不要用裸类型断言，
// 一旦探针丢掉 dns 段就会 panic 并中断整包测试，掩盖其余用例的结论。
func probeDNSServers(t *testing.T, m map[string]any) []any {
	t.Helper()
	dns, ok := m["dns"].(map[string]any)
	if !ok {
		t.Fatalf("测速配置缺少 dns 段: %v", m)
	}
	servers, _ := dns["servers"].([]any)
	return servers
}

func probeDNSTags(t *testing.T, m map[string]any) []string {
	t.Helper()
	servers := probeDNSServers(t, m)
	tags := make([]string, 0, len(servers))
	for _, s := range servers {
		tags = append(tags, s.(map[string]any)["tag"].(string))
	}
	return tags
}

func probeRoute(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	route, ok := m["route"].(map[string]any)
	if !ok {
		t.Fatalf("测速配置缺少 route 段: %v", m)
	}
	return route
}

func probeOutboundTags(t *testing.T, m map[string]any) []string {
	t.Helper()
	outbounds, _ := m["outbounds"].([]any)
	tags := make([]string, 0, len(outbounds))
	for _, o := range outbounds {
		tags = append(tags, o.(map[string]any)["tag"].(string))
	}
	return tags
}

// T1 节点服务器为域名时，测速配置必须自带 dns 与 route.default_domain_resolver。
// 这是本次故障最核心的回归门：删掉 buildProbeDNS 的输出，本测试必须失败。
func TestProbeConfigCarriesDomainResolver(t *testing.T) {
	dns := &DNSConfig{Final: "remote", Servers: []DNSServer{
		{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true},
		{Tag: "remote", Type: "https", Address: "8.8.8.8", AddressResolver: "local", Detour: "Auto", Enabled: true},
	}}
	m, plan := mustProbe(t, probeDNS(dns, trojanNode(7, "node.example.test")))

	if tags := probeDNSTags(t, m); !reflect.DeepEqual(tags, []string{"local"}) {
		t.Errorf("测速 dns.servers = %v, 期望仅自举解析器 [local]", tags)
	}
	if got := probeRoute(t, m)["default_domain_resolver"]; got != "local" {
		t.Errorf("route.default_domain_resolver = %v, 期望 local", got)
	}
	// 悬空引用会让临时核心直接启动失败，比「测速失败」更难排查。
	if strings.Contains(string(plan.Data), "Auto") {
		t.Errorf("测速配置不应包含代理组出站引用 Auto:\n%s", plan.Data)
	}
}

// T2 地址为字面 IP 的 udp DNS 优先于「地址是域名、需要代理」的 DNS。
func TestProbePrefersLiteralIPResolver(t *testing.T) {
	dns := &DNSConfig{Final: "remote", Servers: []DNSServer{
		{Tag: "remote", Type: "https", Address: "dns.example.com", Detour: "Auto", Enabled: true},
		{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true},
	}}
	m, _ := mustProbe(t, probeDNS(dns, trojanNode(1, "node.example.test")))

	if got := probeRoute(t, m)["default_domain_resolver"]; got != "local" {
		t.Errorf("default_domain_resolver = %v, 期望 local（纯 IP 的 udp）", got)
	}
	// remote 的 detour 指向代理组，探针里没有这个出站，绝不能把它复制进来。
	if tags := probeDNSTags(t, m); !reflect.DeepEqual(tags, []string{"local"}) {
		t.Errorf("测速 dns.servers = %v, 期望 [local]", tags)
	}
}

// T3 代理型 DNS 的 detour 不得被静默改写为直连后塞进探针。
func TestProbeDoesNotCopyProxiedDNSDetour(t *testing.T) {
	dns := &DNSConfig{Final: "remote", Servers: []DNSServer{
		{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true},
		{Tag: "remote", Type: "https", Address: "8.8.8.8", AddressResolver: "local", Detour: "Auto", Enabled: true},
	}}
	plan, err := GenerateLatencyProbe(probeDNS(dns, trojanNode(3, "node.example.test")))
	if err != nil {
		t.Fatalf("GenerateLatencyProbe: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(plan.Data, &m); err != nil {
		t.Fatal(err)
	}

	// 断言 1：不存在悬空 outbound "Auto"。
	allowed := map[string]bool{"direct": true}
	for _, tag := range probeOutboundTags(t, m) {
		allowed[tag] = true
	}
	servers := probeDNSServers(t, m)
	for _, s := range servers {
		detour, _ := s.(map[string]any)["detour"].(string)
		if detour != "" && !allowed[detour] {
			t.Errorf("dns 服务器 %v 的 detour %q 在测速配置中没有对应出站", s, detour)
		}
	}
	// 断言 2：remote 整个没进探针（既不是「加进来但 Auto 不存在」）。
	if got := probeDNSTags(t, m); !reflect.DeepEqual(got, []string{"local"}) {
		t.Errorf("测速 dns.servers = %v, 期望 [local]", got)
	}
}

// T4 被选中服务器地址是域名时，AddressResolver 必须一并进配置（依赖先、使用者后）。
func TestProbeIncludesAddressResolverChain(t *testing.T) {
	// doh 在列表首位且两个候选都属「其他」类别，因此按原顺序选中 doh；
	// 若没有闭包，doh 的 domain_resolver=bootstrap 会在启动时报 server not found。
	dns := &DNSConfig{Servers: []DNSServer{
		{Tag: "doh", Type: "https", Address: "dns.example.test", AddressResolver: "bootstrap", Enabled: true},
		{Tag: "bootstrap", Type: "tls", Address: "1.1.1.1", Enabled: true},
	}}
	m, _ := mustProbe(t, probeDNS(dns, trojanNode(4, "node.example.test")))

	if got := probeRoute(t, m)["default_domain_resolver"]; got != "doh" {
		t.Fatalf("default_domain_resolver = %v, 期望 doh（本夹具刻意让 doh 成为唯一自举候选）", got)
	}
	if tags := probeDNSTags(t, m); !reflect.DeepEqual(tags, []string{"bootstrap", "doh"}) {
		t.Errorf("测速 dns.servers = %v, 期望 [bootstrap doh]（依赖在前）", tags)
	}
}

// T5 AddressResolver 环必须 fail-closed，不能死循环也不能静默忽略。
func TestProbeRejectsAddressResolverCycle(t *testing.T) {
	dns := &DNSConfig{Final: "a", Servers: []DNSServer{
		{Tag: "a", Type: "udp", Address: "1.1.1.1", AddressResolver: "b", Enabled: true},
		{Tag: "b", Type: "udp", Address: "1.1.1.2", AddressResolver: "a", Enabled: true},
	}}
	_, err := GenerateLatencyProbe(probeDNS(dns, trojanNode(5, "node.example.test")))
	if err == nil {
		t.Fatal("AddressResolver 环应返回明确错误")
	}
	if !strings.Contains(err.Error(), "环") {
		t.Errorf("错误文案应指出环引用: %v", err)
	}
}

// T6 没有可自举的 DNS 时必须报错，不得偷偷回退系统解析器。
// 静默回退会让「DNS 切换」在测速路径上失去判决意义——那正是本次故障的另一面。
func TestProbeRejectsProxiedOnlyDNS(t *testing.T) {
	dns := &DNSConfig{Final: "remote", Servers: []DNSServer{
		{Tag: "remote", Type: "https", Address: "dns.example.com", Detour: "Auto", Enabled: true},
	}}
	plan, err := GenerateLatencyProbe(probeDNS(dns, trojanNode(6, "node.example.test")))
	if err == nil {
		t.Fatalf("只有代理型 DNS 时应报错，实际生成: %s", plan.Data)
	}
	for _, want := range []string{"bootstrap", "代理组"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误文案应可操作（含 %q）: %v", want, err)
		}
	}
}

// T7 探针里的节点出站必须与正式配置完全同源（NodeToOutbound），
// 防止以后有人在 probe 里另写一套协议转换。
func TestProbeNodesReuseNodeToOutbound(t *testing.T) {
	nodes := []*Node{
		trojanNode(11, "trojan.example.test"),
		{ID: 12, Name: "vless", Protocol: "vless", Server: "vless.example.test", Port: 8443, Enabled: true,
			TLS: true, Metadata: map[string]any{"uuid": "u-1", "sni": "sni.example.test"}},
		{ID: 13, Name: "ss", Protocol: "shadowsocks", Server: "ss.example.test", Port: 8388, Enabled: true,
			Metadata: map[string]any{"method": "aes-128-gcm", "password": "pw"}},
	}
	dns := &DNSConfig{Servers: []DNSServer{{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true}}}
	m, plan := mustProbe(t, probeDNS(dns, nodes...))

	byTag := map[string]map[string]any{}
	for _, o := range m["outbounds"].([]any) {
		ob := o.(map[string]any)
		byTag[ob["tag"].(string)] = ob
	}
	if len(plan.Targets) != len(nodes) {
		t.Fatalf("Targets = %+v, 期望 %d 项", plan.Targets, len(nodes))
	}
	for i, n := range nodes {
		target := plan.Targets[i]
		if target.ID != n.ID || target.Tag != ProbeTag(n.ID) {
			t.Errorf("Targets[%d] = %+v, 期望 ID=%d Tag=%s", i, target, n.ID, ProbeTag(n.ID))
		}
		// NodeToOutbound 返回 int，JSON 往返后是 float64：比对两边都过一遍 JSON。
		want := jsonValue(t, mustOutbound(t, n, target.Tag))
		got := byTag[target.Tag]
		if !reflect.DeepEqual(got, want) {
			t.Errorf("节点 %d 出站与 NodeToOutbound 不一致:\n got=%v\nwant=%v", n.ID, got, want)
		}
	}
}

func mustOutbound(t *testing.T, n *Node, tag string) map[string]any {
	t.Helper()
	ob, err := NodeToOutbound(n, tag)
	if err != nil {
		t.Fatalf("NodeToOutbound: %v", err)
	}
	return ob
}

func jsonValue(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestProbeTagIgnoresNodeName 测速出站 tag 由 ID 决定：节点重名、中文名或叫
// "direct" 都不会与保留出站冲突。
func TestProbeTagIgnoresNodeName(t *testing.T) {
	nodes := []*Node{
		{ID: 20, Name: "direct", Protocol: "trojan", Server: "1.1.1.1", Port: 443, Enabled: true,
			Metadata: map[string]any{"password": "p"}},
		{ID: 21, Name: "direct", Protocol: "trojan", Server: "1.1.1.2", Port: 443, Enabled: true,
			Metadata: map[string]any{"password": "p"}},
	}
	m, plan := mustProbe(t, probeDNS(nil, nodes...))
	tags := probeOutboundTags(t, m)
	seen := map[string]bool{}
	for _, tag := range tags {
		if seen[tag] {
			t.Fatalf("出站 tag 重复: %v", tags)
		}
		seen[tag] = true
	}
	for _, target := range plan.Targets {
		if target.Tag == "direct" || !seen[target.Tag] {
			t.Errorf("测速 tag %q 与保留出站冲突或缺失（tags=%v）", target.Tag, tags)
		}
	}
	if tags[len(tags)-1] != "direct" {
		t.Errorf("直连出站应保留且唯一: %v", tags)
	}
}

// TestProbeWithoutDNSServersOmitsSection 没有 DNS 服务器时不生成 dns 段：
// 此时主核心同样走系统解析器，两边语义一致（属于用户配置现状，不是探针缺陷）。
func TestProbeWithoutDNSServersOmitsSection(t *testing.T) {
	m, _ := mustProbe(t, probeDNS(nil, trojanNode(30, "1.2.3.4")))
	if _, ok := m["dns"]; ok {
		t.Errorf("没有 DNS 服务器时不应生成 dns 段: %v", m["dns"])
	}
	if got := probeRoute(t, m)["default_domain_resolver"]; got != nil {
		t.Errorf("没有 DNS 服务器时不应设置 default_domain_resolver: %v", got)
	}
}

// TestProbeAllowsDirectDetour detour 指向内置 direct 时可用：临时配置里就有这个出站。
func TestProbeAllowsDirectDetour(t *testing.T) {
	dns := &DNSConfig{Servers: []DNSServer{
		{Tag: "remote", Type: "https", Address: "8.8.8.8", Detour: "direct", Enabled: true},
	}}
	m, _ := mustProbe(t, probeDNS(dns, trojanNode(31, "node.example.test")))
	if got := probeRoute(t, m)["default_domain_resolver"]; got != "remote" {
		t.Errorf("default_domain_resolver = %v, 期望 remote（detour=direct 可用）", got)
	}
	if got := probeDNSServers(t, m)[0].(map[string]any)["detour"]; got != "direct" {
		t.Errorf("detour 应原样保留: %v", got)
	}
}

// TestProbeSkipsUnbuildableNodes 无法生成出站的节点进入 Skipped 而不是整体失败，
// 调用方需要逐条回报「哪个节点、为什么」。
func TestProbeSkipsUnbuildableNodes(t *testing.T) {
	nodes := []*Node{
		{ID: 40, Name: "bad", Protocol: "vmess", Server: "1.1.1.1", Port: 443, Enabled: true},
		trojanNode(41, "1.1.1.2"),
	}
	_, plan := mustProbe(t, probeDNS(nil, nodes...))
	if len(plan.Skipped) != 1 || plan.Skipped[0].ID != 40 || plan.Skipped[0].Err == nil {
		t.Fatalf("Skipped = %+v, 期望恰好跳过节点 40 并带上原因", plan.Skipped)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].ID != 41 {
		t.Errorf("Targets = %+v, 期望仅节点 41", plan.Targets)
	}
}

// TestProbeConfigIsDeterministic 同一输入生成字节一致：临时配置会落盘并被
// sing-box 读取，幂等才能保证「配置漂移」不会被误判成节点问题。
func TestProbeConfigIsDeterministic(t *testing.T) {
	dns := &DNSConfig{Servers: []DNSServer{
		{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true},
		{Tag: "remote", Type: "https", Address: "8.8.8.8", AddressResolver: "local", Enabled: true},
	}}
	p := probeDNS(dns, trojanNode(50, "node.example.test"))
	first, err := GenerateLatencyProbe(p)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateLatencyProbe(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Data) != string(second.Data) {
		t.Errorf("两次生成不一致:\n%s\n---\n%s", first.Data, second.Data)
	}
}

// TestProbeRejectsInvalidAPIPort 端口非法时立刻报错，而不是生成一份起不来的配置。
func TestProbeRejectsInvalidAPIPort(t *testing.T) {
	p := probeDNS(nil, trojanNode(60, "1.1.1.1"))
	p.APIPort = 0
	if _, err := GenerateLatencyProbe(p); err == nil {
		t.Fatal("非法 API 端口应返回错误")
	}
}
