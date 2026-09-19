package config

import (
	"errors"
	"strings"
	"testing"
)

// V9-6：正式生成器不得选中「只能经代理组到达」的 bootstrap resolver。
//
// 判据不看 `resolver.Detour == ""`，也不重新遍历 ProxyGroup→Member→Node：
// 组的安全性取的是 `resolveMembers()` 这一次**实际解析出来的** leaf 出站
// （动态 all、嵌套组、禁用过滤都已算完），节点是否依赖 DNS 取自 `NodeToOutbound`
// 生成的**真实出站**的 server 字段（tor 这类没有 server 字段的出站不依赖解析）。
//
// 口径是「该组的**所有**有效 leaf 都必须不依赖 DNS」——不依赖运行期选路时序：
// selector 可以切成员、urltest 选路会变，只要存在一个域名型成员就不算安全。
//
// 全候选都不安全时 fail-closed（ErrNoBootstrapDNS），不做「省略字段 + warning」：
// 实测（sing-box 1.14.0）省略 route.default_domain_resolver 后仍然卡死。

// bsNode 构造一个代理节点。
func bsNode(id int64, name, server string) *Node {
	return &Node{ID: id, Name: name, Protocol: "http", Server: server, Port: 8080, Enabled: true}
}

// bsSnapshot 组装一个带单个代理组的最小快照。
func bsSnapshot(nodes []*Node, group *ProxyGroup, dns []DNSServer, final string) Snapshot {
	return Snapshot{
		Settings:    DefaultSettings(),
		Nodes:       nodes,
		ProxyGroups: []*ProxyGroup{group},
		DNS:         &DNSConfig{Strategy: "prefer_ipv4", Servers: dns, Final: final},
	}
}

// bsAuto 一个含指定成员的 urltest 组。
func bsAuto(id int64, members ...ProxyGroupMember) *ProxyGroup {
	return &ProxyGroup{ID: id, Name: "Auto", Type: "urltest", Members: members}
}

func bsNodeMember(id int64) ProxyGroupMember { return ProxyGroupMember{Type: "node", ID: id} }

func bsErr(t *testing.T, snap Snapshot) error {
	t.Helper()
	out, err := Generate(snap)
	if err == nil {
		t.Fatalf("应当 fail-closed，却生成成功:\n%s", out)
	}
	if out != nil {
		t.Error("fail-closed 时不得返回半可用的配置字节")
	}
	return err
}

// 1. remote 经 Auto，Auto 里是域名型节点 → 必须拒绝（V9-5 组 3 的形状）。
//
// 判决性：旧实现会把它选成 route.default_domain_resolver，交给内核后静默卡死。
func TestBootstrapRejectsProxyGroupWithDomainNode(t *testing.T) {
	snap := bsSnapshot(
		[]*Node{bsNode(1, "HK-01", "node.example.com")},
		bsAuto(10, bsNodeMember(1)),
		[]DNSServer{{Tag: "remote", Type: "udp", Address: "127.0.0.1:53", Detour: "Auto", Enabled: true}},
		"remote",
	)
	err := bsErr(t, snap)

	var noBootstrap ErrNoBootstrapDNS
	if !errors.As(err, &noBootstrap) {
		t.Fatalf("应返回 ErrNoBootstrapDNS，得到 %T: %v", err, err)
	}
	if len(noBootstrap.Rejected) != 1 {
		t.Errorf("应逐条给出候选与原因，得到 %v", noBootstrap.Rejected)
	}
	// 文案必须点名「哪个 DNS → 哪个组 → 哪个节点(地址)」，否则用户不知道该改哪一条。
	for _, want := range []string{"remote", "Auto", "HK-01", "node.example.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误文案应包含 %q，实得: %s", want, err.Error())
		}
	}
}

// 2. remote 经 Auto，Auto 里是**字面 IP** 节点 → 仍然安全，必须保住（V9-5 组 4 不回归）。
func TestBootstrapKeepsProxyGroupWithLiteralIPNode(t *testing.T) {
	snap := bsSnapshot(
		[]*Node{bsNode(1, "IP-01", "203.0.113.7")},
		bsAuto(10, bsNodeMember(1)),
		[]DNSServer{{Tag: "remote", Type: "udp", Address: "127.0.0.1:53", Detour: "Auto", Enabled: true}},
		"remote",
	)
	m := mustGenerate(t, snap)
	route := m["route"].(map[string]any)
	if got := route["default_domain_resolver"]; got != "remote" {
		t.Errorf("default_domain_resolver = %v，期望 remote（字面 IP 节点时经 Auto 的 DNS 完全可用）", got)
	}
}

// 3. 混合组（字面 IP + 域名节点）→ 拒绝。
//
// 不采用「当前恰好有一条 IP 路径就算安全」：selector 可以切到域名成员，
// urltest 的选路也随时序变化，那不是稳定不变量。
func TestBootstrapRejectsMixedGroup(t *testing.T) {
	snap := bsSnapshot(
		[]*Node{bsNode(1, "IP-01", "203.0.113.7"), bsNode(2, "HK-01", "node.example.com")},
		bsAuto(10, bsNodeMember(1), bsNodeMember(2)),
		[]DNSServer{{Tag: "remote", Type: "udp", Address: "127.0.0.1:53", Detour: "Auto", Enabled: true}},
		"remote",
	)
	err := bsErr(t, snap)
	if !strings.Contains(err.Error(), "HK-01") {
		t.Errorf("错误应点名域名型那个成员，实得: %v", err)
	}
}

// 4. 嵌套组里是域名型节点 → 拒绝。
//
// 这条专门证明判据复用了 resolveMembers 的**实际展开结果**：外层组自己没有任何
// 直接成员，若实现另写一套「只扫本层 Members」的遍历，这里就会漏判而放行。
func TestBootstrapRejectsNestedGroupWithDomainNode(t *testing.T) {
	inner := &ProxyGroup{ID: 11, Name: "Inner", Type: "urltest", Members: []ProxyGroupMember{bsNodeMember(1)}}
	snap := Snapshot{
		Settings: DefaultSettings(),
		Nodes:    []*Node{bsNode(1, "HK-01", "node.example.com")},
		ProxyGroups: []*ProxyGroup{
			inner,
			{ID: 12, Name: "Auto", Type: "urltest", Members: []ProxyGroupMember{{Type: "group", ID: 11}}},
		},
		DNS: &DNSConfig{Strategy: "prefer_ipv4", Final: "remote", Servers: []DNSServer{
			{Tag: "remote", Type: "udp", Address: "127.0.0.1:53", Detour: "Auto", Enabled: true},
		}},
	}
	if err := bsErr(t, snap); !strings.Contains(err.Error(), "HK-01") {
		t.Errorf("嵌套组里的域名型节点也必须被识别，实得: %v", err)
	}
}

// 5. 动态 all 成员里含域名型节点 → 拒绝（all 的展开同样来自 resolveMembers）。
func TestBootstrapRejectsAllMemberWithDomainNode(t *testing.T) {
	snap := bsSnapshot(
		[]*Node{bsNode(1, "IP-01", "203.0.113.7"), bsNode(2, "HK-01", "node.example.com")},
		bsAuto(10, ProxyGroupMember{Type: "all"}),
		[]DNSServer{{Tag: "remote", Type: "udp", Address: "127.0.0.1:53", Detour: "Auto", Enabled: true}},
		"remote",
	)
	if err := bsErr(t, snap); !strings.Contains(err.Error(), "HK-01") {
		t.Errorf("动态 all 展开出的域名型节点也必须被识别，实得: %v", err)
	}
}

// 6. 第一候选不安全、第二候选直连 → **跳过第一个选第二个**，不是整份配置失败。
func TestBootstrapSkipsUnsafeCandidateAndPicksNext(t *testing.T) {
	snap := bsSnapshot(
		[]*Node{bsNode(1, "HK-01", "node.example.com")},
		bsAuto(10, bsNodeMember(1)),
		[]DNSServer{
			{Tag: "proxy-first", Type: "udp", Address: "127.0.0.1:53", Detour: "Auto", Enabled: true},
			{Tag: "plain", Type: "udp", Address: "223.5.5.5", Enabled: true},
		},
		"plain",
	)
	m := mustGenerate(t, snap)
	route := m["route"].(map[string]any)
	if got := route["default_domain_resolver"]; got != "plain" {
		t.Errorf("default_domain_resolver = %v，期望跳过不安全的首选、落到 plain", got)
	}
}

// 7. 候选自身直连，但 AddressResolver 链上有跨图依赖 → 必须识别二阶环。
//
//	doh  = https dns.google, AddressResolver=boot, detour=（直连）
//	boot = udp 1.1.1.1,     detour=Auto
//	Auto = 含节点 node.example.com
//
// 只看候选自己的 detour 会认为 doh 安全；实际要用 doh 得先经 boot 解析 dns.google，
// 而 boot 经 Auto，Auto 又要先解析 node.example.com。
func TestBootstrapDetectsSecondOrderCrossGraphCycle(t *testing.T) {
	snap := bsSnapshot(
		[]*Node{bsNode(1, "HK-01", "node.example.com")},
		bsAuto(10, bsNodeMember(1)),
		[]DNSServer{
			{Tag: "doh", Type: "https", Address: "dns.google", AddressResolver: "boot", Enabled: true},
			{Tag: "boot", Type: "udp", Address: "1.1.1.1", Detour: "Auto", Enabled: true},
		},
		"doh",
	)
	err := bsErr(t, snap)
	for _, want := range []string{"doh", "boot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误文案应体现 %q 在依赖链上，实得: %s", want, err.Error())
		}
	}
}

// 8. 所有候选都不安全 → typed error，且逐条给出原因。
func TestBootstrapAllCandidatesUnsafe(t *testing.T) {
	snap := bsSnapshot(
		[]*Node{bsNode(1, "HK-01", "node.example.com")},
		bsAuto(10, bsNodeMember(1)),
		[]DNSServer{
			{Tag: "r1", Type: "udp", Address: "127.0.0.1:53", Detour: "Auto", Enabled: true},
			{Tag: "r2", Type: "https", Address: "dns.google", Detour: "Auto", Enabled: true},
		},
		"r1",
	)
	err := bsErr(t, snap)
	var noBootstrap ErrNoBootstrapDNS
	if !errors.As(err, &noBootstrap) {
		t.Fatalf("应返回 ErrNoBootstrapDNS，得到 %T: %v", err, err)
	}
	if len(noBootstrap.Rejected) != 2 {
		t.Errorf("两条候选都该被逐条报告原因，实得 %v", noBootstrap.Rejected)
	}
}

// 反向核对：没有 DNS 服务器时不写 default_domain_resolver，也不报错（既有行为）。
func TestBootstrapNoServersIsNotAnError(t *testing.T) {
	m := mustGenerate(t, Snapshot{Settings: DefaultSettings()})
	route, ok := m["route"].(map[string]any)
	if !ok {
		t.Fatal("配置里没有 route 段")
	}
	if _, has := route["default_domain_resolver"]; has {
		t.Errorf("没有 DNS 服务器时不该写 default_domain_resolver: %v", route)
	}
}

// 反向核对：detour 为 DIRECT/空 的 DNS 一律安全，不得被新判据误伤。
func TestBootstrapDirectDetourStaysSafe(t *testing.T) {
	for _, detour := range []string{"", "DIRECT", "direct"} {
		snap := bsSnapshot(
			[]*Node{bsNode(1, "HK-01", "node.example.com")},
			bsAuto(10, bsNodeMember(1)),
			[]DNSServer{{Tag: "plain", Type: "udp", Address: "223.5.5.5", Detour: detour, Enabled: true}},
			"plain",
		)
		m := mustGenerate(t, snap)
		route := m["route"].(map[string]any)
		if got := route["default_domain_resolver"]; got != "plain" {
			t.Errorf("detour=%q 是直连写法，应当安全；default_domain_resolver = %v", detour, got)
		}
	}
}

// 出站是否依赖 DNS 的判据取自真实出站：没有 server 字段（tor）与带 zone 的 IPv6
// 都不依赖解析，域名才依赖。
func TestOutboundServerNeedsDNS(t *testing.T) {
	cases := []struct {
		name string
		ob   map[string]any
		want bool
	}{
		{"没有 server 字段（tor）", map[string]any{"type": "tor", "tag": "t"}, false},
		{"server 为空串", map[string]any{"type": "http", "server": ""}, false},
		{"字面 IPv4", map[string]any{"type": "http", "server": "203.0.113.7"}, false},
		{"带 zone 的链路本地 IPv6", map[string]any{"type": "http", "server": "fe80::1%en0"}, false},
		{"域名", map[string]any{"type": "http", "server": "node.example.com"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := outboundServerNeedsDNS(tc.ob); got != tc.want {
				t.Errorf("outboundServerNeedsDNS = %v，期望 %v", got, tc.want)
			}
		})
	}
	// 真实出站走一遍 tor：NodeToOutbound 会删掉 server 字段。
	tor, err := NodeToOutbound(&Node{ID: 1, Name: "t", Protocol: "tor", Enabled: true}, "t")
	if err != nil {
		t.Fatalf("NodeToOutbound(tor): %v", err)
	}
	if outboundServerNeedsDNS(tor) {
		t.Errorf("tor 出站没有 server 字段，不该需要 DNS: %+v", tor)
	}
}

// dnsBootstrapSafe 的原因串：链安全时为真，链上有跨图依赖时为假并给出原因。
func TestDNSBootstrapSafeReasons(t *testing.T) {
	g := &generator{snap: Snapshot{}}
	g.init()
	g.outboundNeedsDNS["HK-01"] = true
	g.groupLeafTags["Auto"] = []string{"HK-01"}
	g.groupLeafTags["Safe"] = []string{"IP-01"}

	byTag := map[string]*DNSServer{
		"boot":  {Tag: "boot", Type: "udp", Address: "1.1.1.1", Detour: "Auto", Enabled: true},
		"ok":    {Tag: "ok", Type: "udp", Address: "1.1.1.1", Detour: "Safe", Enabled: true},
		"doh":   {Tag: "doh", Type: "https", Address: "dns.google", AddressResolver: "boot", Enabled: true},
		"plain": {Tag: "plain", Type: "udp", Address: "223.5.5.5", Enabled: true},
	}

	if safe, why := g.dnsBootstrapSafe(byTag["plain"], byTag); !safe {
		t.Errorf("直连的纯 IP DNS 应当安全，实得 reason=%q", why)
	}
	if safe, why := g.dnsBootstrapSafe(byTag["ok"], byTag); !safe {
		t.Errorf("detour 指向只含 IP 节点的组应当安全，实得 reason=%q", why)
	}
	safe, why := g.dnsBootstrapSafe(byTag["boot"], byTag)
	if safe {
		t.Fatal("detour 指向含域名型节点的组不应安全")
	}
	if !strings.Contains(why, "Auto") {
		t.Errorf("原因应点名代理组，实得 %q", why)
	}
	// 地址是域名但没有 AddressResolver：只能靠 default_domain_resolver 解析自身。
	lonely := &DNSServer{Tag: "lonely", Type: "https", Address: "dns.google", Enabled: true}
	if safe, why = g.dnsBootstrapSafe(lonely, byTag); safe {
		t.Fatal("地址是域名且没有 AddressResolver 时不应安全")
	}
	if !strings.Contains(why, "AddressResolver") {
		t.Errorf("原因应说明缺 AddressResolver，实得 %q", why)
	}
	// 二阶：doh 自身直连，但依赖链上不安全。
	if safe, why = g.dnsBootstrapSafe(byTag["doh"], byTag); safe {
		t.Fatal("依赖链上有跨图依赖时不应安全")
	}
	if !strings.Contains(why, "boot") {
		t.Errorf("原因应体现依赖链，实得 %q", why)
	}
}

// 环的兜底：即使调用方绕过 buildDNS 的环校验，递归也不能死循环。
func TestDNSBootstrapSafeCycleDoesNotHang(t *testing.T) {
	g := &generator{snap: Snapshot{}}
	g.init()
	byTag := map[string]*DNSServer{
		"a": {Tag: "a", Type: "https", Address: "a.example", AddressResolver: "b", Enabled: true},
		"b": {Tag: "b", Type: "https", Address: "b.example", AddressResolver: "a", Enabled: true},
	}
	safe, why := g.dnsBootstrapSafe(byTag["a"], byTag)
	if safe {
		t.Fatal("成环的链不应安全")
	}
	if !strings.Contains(why, "环") {
		t.Errorf("原因应指出环，实得 %q", why)
	}
}
