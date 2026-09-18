package config

import (
	"strings"
	"testing"
)

// V7-8 的单元判据。真实内核侧的判决见 internal/core 的
// TestRealSingBoxDNSResolverCycle：check rc=0、run 报
// "circular server dependency"，所以这条校验必须在这里做，不能指望静态检查。

func dnsServer(tag, resolver string) DNSServer {
	return DNSServer{Tag: tag, Type: "udp", Address: "1.1.1.1", AddressResolver: resolver, Enabled: true}
}

func TestDNSResolverGraphRejectsIndirectCycle(t *testing.T) {
	cases := []struct {
		name    string
		servers []DNSServer
	}{
		{"直接互指", []DNSServer{dnsServer("a", "b"), dnsServer("b", "a")}},
		{"三元环", []DNSServer{dnsServer("a", "b"), dnsServer("b", "c"), dnsServer("c", "a")}},
		{"自指", []DNSServer{dnsServer("a", "a")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDNSResolverGraph(tc.servers)
			if err == nil {
				t.Fatal("成环必须被拒绝")
			}
			if !strings.Contains(err.Error(), "环") {
				t.Errorf("错误文案应指出「环」，实得 %v", err)
			}
		})
	}
}

func TestDNSResolverGraphAllowsChain(t *testing.T) {
	cases := []struct {
		name    string
		servers []DNSServer
	}{
		{"两段链", []DNSServer{dnsServer("a", "b"), dnsServer("b", "")}},
		{"三段链", []DNSServer{dnsServer("a", "b"), dnsServer("b", "c"), dnsServer("c", "")}},
		{"无引用", []DNSServer{dnsServer("a", ""), dnsServer("b", "")}},
		{"指向集合外的 tag", []DNSServer{dnsServer("a", "not-in-set")}},
		{"空集合", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateDNSResolverGraph(tc.servers); err != nil {
				t.Fatalf("正向链不得被拒绝: %v", err)
			}
		})
	}
}

// 反向核对：链上的重复 tag（生成器另有 Tag 重复校验）不应被误报成环。
func TestDNSResolverGraphDoesNotConfuseDuplicateTagWithCycle(t *testing.T) {
	servers := []DNSServer{dnsServer("a", "b"), dnsServer("a", "b"), dnsServer("b", "")}
	if err := ValidateDNSResolverGraph(servers); err != nil {
		t.Fatalf("tag 重复由生成器另行报错，本函数只判环: %v", err)
	}
}

// 环上的 tag 必须出现在错误文案里，用户才知道要改哪几个。
func TestDNSResolverGraphErrorNamesTheCycle(t *testing.T) {
	err := ValidateDNSResolverGraph([]DNSServer{dnsServer("a", "b"), dnsServer("b", "a")})
	if err == nil {
		t.Fatal("应报环")
	}
	for _, tag := range []string{"a", "b", "->"} {
		if !strings.Contains(err.Error(), tag) {
			t.Errorf("错误文案 %q 缺少 %q", err.Error(), tag)
		}
	}
}
