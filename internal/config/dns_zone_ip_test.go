package config

import (
	"reflect"
	"testing"
)

// V9-4：带 zone 的 IPv6 必须与其它字面 IP 得到**完全一样**的对待。
//
// 判据统一到 netip.ParseAddr 之后，「是不是字面 IP」在全仓只有一处实现
// （`isLiteralIPHost`）。本文件从三个可观测面钉住这一点：
//  1. 是否需要 domain_resolver（DNSServerNeedsDomainResolver）
//  2. 能否作为独立测速的自举候选（probeResolverChain）
//  3. 在 bootstrap 优先级排序里落在哪个桶（dnsCandidateOrder / pickDNSResolver）
//
// 判决性：旧实现里 1 返回 true（当成域名）、3 把它排到域名之后。

// zoneServer 返回一个带 zone 的链路本地 IPv6 DNS 服务器（bootstrap 无需任何解析）。
func zoneServer() DNSServer {
	return DNSServer{Tag: "linklocal", Type: "udp", Address: "[fe80::1%en0]:53", Enabled: true}
}

func TestZoneIPv6TreatedExactlyLikeLiteralIPv4(t *testing.T) {
	zone := zoneServer()
	v4 := DNSServer{Tag: "v4", Type: "udp", Address: "1.1.1.1", Enabled: true}
	domain := DNSServer{Tag: "doh", Type: "https", Address: "dns.google", Enabled: true}

	// 1. 不需要 domain_resolver
	for _, s := range []DNSServer{zone, v4} {
		if DNSServerNeedsDomainResolver(&s) {
			t.Errorf("%s 是字面 IP，不该需要 domain_resolver", s.Address)
		}
	}
	if !DNSServerNeedsDomainResolver(&domain) {
		t.Error("域名地址仍然需要 domain_resolver（反向核对）")
	}

	// 2. 能作为自举候选：probeResolverChain 不该给出任何拒绝理由
	byTag := map[string]*DNSServer{zone.Tag: &zone, v4.Tag: &v4, domain.Tag: &domain}
	for _, s := range []*DNSServer{&zone, &v4} {
		if _, reason := probeResolverChain(s, byTag); reason != "" {
			t.Errorf("%s 应当可直接自举，实得拒绝理由 %q", s.Address, reason)
		}
	}
	if _, reason := probeResolverChain(&domain, byTag); reason == "" {
		t.Error("无 AddressResolver 的域名型服务器不应当能自举（反向核对）")
	}

	// 3. 同一个桶：zone IPv6 与字面 IPv4 都进「字面 IP」桶
	servers := []DNSServer{
		{Tag: "dom", Type: "udp", Address: "dns.example.test"}, // rest
		zone,                          // 字面 IP 桶
		{Tag: "local", Type: "local"}, // local 桶
		v4,                            // 字面 IP 桶
	}
	want := []int{2, 1, 3, 0} // local 桶 -> 字面 IP 桶（保持原相对顺序）-> 其余
	if got := dnsCandidateOrder(servers); !reflect.DeepEqual(got, want) {
		t.Errorf("dnsCandidateOrder = %v，期望 %v（zone IPv6 必须与字面 IPv4 同桶）", got, want)
	}
}

// bootstrap 选择的用户可见后果：只有「带 zone 的 udp」与「域名型 DoH」两条时，
// 必须选前者 —— 域名型那条要先解析自己，选它等于没有 bootstrap。
//
// 判决性：旧实现把 zone IPv6 归到「其余」桶，于是排在域名型之后被选中。
//
// V9-6 之后 pickDNSResolver 变成了生成器方法（还要过跨图可达性判据），
// 这里给 DoH 配一条**安全**的 AddressResolver，好让本用例仍然只考「排序」这一件事。
func TestPickDNSResolverPrefersZoneIPv6OverDomain(t *testing.T) {
	zone := zoneServer()
	servers := []DNSServer{
		{Tag: "doh", Type: "https", Address: "dns.google", AddressResolver: zone.Tag, Enabled: true},
		zone,
	}
	g := &generator{snap: Snapshot{}}
	g.init()
	got, err := g.pickDNSResolver(servers)
	if err != nil {
		t.Fatalf("pickDNSResolver: %v", err)
	}
	if got != zone.Tag {
		t.Errorf("pickDNSResolver = %q，期望 %q（字面 IP 优先于需要先解析自己的域名）", got, zone.Tag)
	}
}

// zone 只对 IPv6 合法：`192.168.1.1%en0` 新旧判据都认不出，仍按域名处理。
// 这条钉的是「换判据不引入回归」——不是本轮的目标行为。
func TestZoneOnIPv4StaysDomain(t *testing.T) {
	s := DNSServer{Tag: "weird", Type: "udp", Address: "192.168.1.1%en0", Enabled: true}
	if !DNSServerNeedsDomainResolver(&s) {
		t.Error("netip.ParseAddr 不认 IPv4 上的 zone，该输入应与改动前一致地按域名处理")
	}
}
