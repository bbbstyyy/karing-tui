package config

import (
	"strings"
	"testing"
)

// V8-1：`AddressResolver` 只有在**地址是域名**时才有意义。
// 下面这组用例钉住「正式配置」这一侧：字面 IP 不得输出 domain_resolver，
// 域名必须继续输出；而完全用不到的悬空引用不得拖垮生成。

// T1：唯一的 resolver 需求判据。这是本轮修复最基础的判决测试——
// 后面所有「输出/校验/依赖/展示」行为都建立在它之上。
func TestDNSServerNeedsDomainResolver(t *testing.T) {
	cases := []struct {
		name   string
		server *DNSServer
		needs  bool
	}{
		{name: "nil", server: nil, needs: false},
		{name: "local", server: &DNSServer{Tag: "local", Type: "local"}, needs: false},
		{name: "udp 字面 IPv4", server: &DNSServer{Type: "udp", Address: "223.5.5.5"}, needs: false},
		{name: "udp 字面 IPv4 + 端口", server: &DNSServer{Type: "udp", Address: "223.5.5.5:53"}, needs: false},
		{name: "tcp 字面 IPv4", server: &DNSServer{Type: "tcp", Address: "1.1.1.1"}, needs: false},
		{name: "udp 裸 IPv6", server: &DNSServer{Type: "udp", Address: "2001:4860:4860::8888"}, needs: false},
		{name: "udp IPv6 + 端口", server: &DNSServer{Type: "udp", Address: "[2001:4860:4860::8888]:53"}, needs: false},
		{name: "https 字面 IPv4", server: &DNSServer{Type: "https", Address: "8.8.8.8"}, needs: false},
		{name: "https 字面 IPv4 URL", server: &DNSServer{Type: "https", Address: "https://8.8.8.8/dns-query"}, needs: false},
		{name: "tls 字面 IPv6 URL", server: &DNSServer{Type: "tls", Address: "tls://[2001:4860::8888]:853"}, needs: false},
		{name: "udp 域名", server: &DNSServer{Type: "udp", Address: "dns.google"}, needs: true},
		{name: "udp 域名 + 端口", server: &DNSServer{Type: "udp", Address: "dns.google:53"}, needs: true},
		{name: "https 域名", server: &DNSServer{Type: "https", Address: "dns.google"}, needs: true},
		{name: "https 域名 URL", server: &DNSServer{Type: "https", Address: "https://dns.google/dns-query"}, needs: true},
		{name: "tls 域名 + 端口", server: &DNSServer{Type: "tls", Address: "cloudflare-dns.com:853"}, needs: true},
		{name: "非 local 但地址为空", server: &DNSServer{Type: "udp", Address: ""}, needs: false},
		// V9-4：带 zone 的（链路本地）IPv6 同样是**字面 IP**，不存在「域名 → IP」这一步。
		// `net.ParseIP` 认不出 zone 写法，于是这类地址曾被当成域名——后果与 V8-1 修掉的
		// 那一类完全同形（假依赖参与环检测/依赖阻断、输出无意义的 domain_resolver）。
		{name: "udp 带 zone 的 IPv6 + 端口", server: &DNSServer{Type: "udp", Address: "[fe80::1%en0]:53"}, needs: false},
		{name: "tcp 带 zone 的 IPv6", server: &DNSServer{Type: "tcp", Address: "[fe80::1%en0]:53"}, needs: false},
		// 裸写（无方括号）的 zone IPv6：纯语义上仍是字面 IP。注意它当前**进不了库**
		// ——`validateServerAddress` 对含多个冒号的裸串要求 host 能被 net.ParseIP 解析，
		// 本项刻意不动那个语法门（见 §9 偏差 3），所以这一行记录的是语义边界。
		{name: "udp 裸 zone IPv6（当前不可入库，仅记录语义）", server: &DNSServer{Type: "udp", Address: "fe80::1%en0"}, needs: false},
		{name: "https 带 zone 的 IPv6 URL（%25 转义）", server: &DNSServer{Type: "https", Address: "https://[fe80::1%25lo0]/dns-query"}, needs: false},
		// URL 里 zone 的 `%` 必须写成 `%25`，否则 `url.Parse` 直接报
		// `invalid URL escape "%en"`（实测）。那种写法进不了库——`validateServerAddress`
		// 对含 "://" 的地址会因 url.Parse 失败而拒绝，所以这里不需要（也不应该）为它
		// 定义一个「期望」：它根本不是可达状态。
		{name: "tls 带 zone 的 IPv6 URL（%25 转义）", server: &DNSServer{Type: "tls", Address: "tls://[fe80::1%25en0]:853"}, needs: false},
		// 边界（记录现状，不是本轮目标）：zone 写在 IPv4 上不是合法地址，
		// `netip.ParseAddr` 与 `net.ParseIP` **都**认不出 → 仍按域名处理。
		// 该输入今天能被 `validateServerAddress` 存进去（无冒号即当主机名），行为与改动前一致。
		{name: "zone 写法用在 IPv4 上（两者都不认，按域名处理）", server: &DNSServer{Type: "udp", Address: "192.168.1.1%en0"}, needs: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DNSServerNeedsDomainResolver(tc.server); got != tc.needs {
				t.Fatalf("DNSServerNeedsDomainResolver = %v, 期望 %v", got, tc.needs)
			}
		})
	}
}

func dnsSnapshot(servers []DNSServer, final string) Snapshot {
	return Snapshot{
		Settings: DefaultSettings(),
		DNS:      &DNSConfig{Strategy: "prefer_ipv4", Servers: servers, Final: final},
	}
}

func dnsServerByTag(t *testing.T, m map[string]any, tag string) map[string]any {
	t.Helper()
	dns, ok := m["dns"].(map[string]any)
	if !ok {
		t.Fatalf("配置里没有 dns 段: %v", m["dns"])
	}
	servers, ok := dns["servers"].([]any)
	if !ok {
		t.Fatalf("dns.servers 不是数组: %v", dns["servers"])
	}
	for _, raw := range servers {
		srv, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if srv["tag"] == tag {
			return srv
		}
	}
	t.Fatalf("未找到 DNS 服务器 %q: %v", tag, servers)
	return nil
}

// 字面 IP 的 DNS 服务器：不得输出 domain_resolver，其余字段不受影响，
// 且 route.default_domain_resolver（节点/出站的 bootstrap，另一件事）必须保留。
func TestMainConfigOmitsResolverForLiteralIPDNS(t *testing.T) {
	snap := dnsSnapshot([]DNSServer{
		{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true},
		{Tag: "remote", Type: "https", Address: "8.8.8.8", AddressResolver: "local", Enabled: true},
	}, "remote")
	m := mustGenerate(t, snap)

	remote := dnsServerByTag(t, m, "remote")
	if got, ok := remote["domain_resolver"]; ok {
		t.Fatalf("字面 IP 的 DNS 服务器不得输出 domain_resolver，实得 %v", got)
	}
	if remote["type"] != "https" || remote["server"] != "8.8.8.8" {
		t.Fatalf("其余字段不得受影响: %v", remote)
	}

	// 反向核对：bootstrap 来源与本次修复无关，删掉它会让域名型节点测速/出站
	// 重新失去解析环境（V6-1 修过的故障）。
	route, ok := m["route"].(map[string]any)
	if !ok {
		t.Fatal("配置里没有 route 段")
	}
	if route["default_domain_resolver"] != "local" {
		t.Fatalf("route.default_domain_resolver = %v，期望 local", route["default_domain_resolver"])
	}
}

// 域名地址的 DNS 服务器：必须继续输出 domain_resolver。
// 没有这条，「把字段全删掉」也会让上面那条用例变绿。
func TestMainConfigKeepsResolverForDomainDNS(t *testing.T) {
	m := mustGenerate(t, dnsSnapshot([]DNSServer{
		{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true},
		{Tag: "doh", Type: "https", Address: "dns.google", AddressResolver: "local", Enabled: true},
	}, "doh"))

	if got := dnsServerByTag(t, m, "doh")["domain_resolver"]; got != "local" {
		t.Fatalf("域名型 DNS 服务器的 domain_resolver = %v，期望 local", got)
	}
}

// 用不到的引用 = 不存在的引用：字面 IP 上残留的悬空 AddressResolver
// 不得让整份配置生成失败。
//
// 判决性：旧实现只判 `AddressResolver != ""`，这里会因为 ghost 不存在而报错。
func TestMainConfigIgnoresUnusedDanglingResolver(t *testing.T) {
	m := mustGenerate(t, dnsSnapshot([]DNSServer{
		{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true},
		{Tag: "remote", Type: "https", Address: "8.8.8.8", AddressResolver: "ghost", Enabled: true},
	}, "remote"))

	remote := dnsServerByTag(t, m, "remote")
	if got, ok := remote["domain_resolver"]; ok {
		t.Fatalf("悬空且用不到的 resolver 不得出现在输出里，实得 %v", got)
	}
}

// 反向核对：真的会被用到的悬空引用（域名地址）仍必须让生成失败——
// 证明上一条不是把引用校验整体关掉了。
func TestMainConfigRejectsUsedDanglingResolver(t *testing.T) {
	_, err := Generate(dnsSnapshot([]DNSServer{
		{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true},
		{Tag: "doh", Type: "https", Address: "dns.google", AddressResolver: "ghost", Enabled: true},
	}, "doh"))
	if err == nil {
		t.Fatal("域名地址引用不存在的 resolver 必须生成失败")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("错误文案应指出缺失的 tag，实得 %v", err)
	}
}
