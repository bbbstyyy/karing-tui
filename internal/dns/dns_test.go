package dns

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
		t.Fatal(err)
	}
	db, err := storage.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewManager(db, nil)
}

func TestUpdateServerRenamesReferences(t *testing.T) {
	m := newTestManager(t)
	s := &config.DNSServer{Tag: "old", Type: "udp", Address: "1.1.1.1", Enabled: true}
	if err := m.DB.CreateDNSServer(s); err != nil {
		t.Fatal(err)
	}
	other := &config.DNSServer{Tag: "other", Type: "https", Address: "8.8.8.8", AddressResolver: "old", Enabled: true}
	if err := m.DB.CreateDNSServer(other); err != nil {
		t.Fatal(err)
	}
	if err := m.DB.CreateDNSRule(&config.DNSRule{Type: "domain", Value: "example.com", Server: "old", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.DB.SetSetting(keyFinal, "old"); err != nil {
		t.Fatal(err)
	}
	s.Tag = "new"
	if err := m.UpdateServer(s); err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	rules, _ := m.DB.ListDNSRules()
	if len(rules) != 1 || rules[0].Server != "new" {
		t.Fatalf("DNS 规则引用未更新: %+v", rules)
	}
	servers, _ := m.DB.ListDNSServers()
	for _, got := range servers {
		if got.Tag == "other" && got.AddressResolver != "new" {
			t.Fatalf("address_resolver 未更新: %+v", got)
		}
	}
	final, _ := m.DB.GetSetting(keyFinal)
	if final != "new" {
		t.Fatalf("dns_final 未更新: %q", final)
	}
}

func TestEnsureDefaultDNSRoutesRemoteThroughAutoGroup(t *testing.T) {
	m := newTestManager(t)
	if err := m.EnsureDefaultDNS(); err != nil {
		t.Fatalf("EnsureDefaultDNS: %v", err)
	}
	servers, err := m.DB.ListDNSServers()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range servers {
		if s.Tag == "remote" && s.Detour != storage.DefaultGroupAuto {
			t.Fatalf("默认远程 DNS 应经 Auto 组，得到 detour=%q", s.Detour)
		}
	}
}

func TestSaveOptionsRejectsInvalidFakeIPRange(t *testing.T) {
	m := newTestManager(t)
	if err := m.SaveOptions(DefaultStrategy, true, "not-a-cidr", ""); err == nil {
		t.Fatal("非法 FakeIP 网段应被拒绝")
	}
	if err := m.SaveOptions(DefaultStrategy, true, "2001:db8::/64", ""); err == nil {
		t.Fatal("IPv6 FakeIP 网段应被拒绝")
	}
	if err := m.SaveOptions(DefaultStrategy, true, "198.18.0.0/15", ""); err != nil {
		t.Fatalf("合法 FakeIP 网段不应失败: %v", err)
	}
}

func TestServerCannotUseItselfAsAddressResolver(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.AddServer("loop", "udp", "1.1.1.1", "loop", ""); err == nil {
		t.Fatal("DNS 服务器不应允许将自身作为 address_resolver")
	}
}

func TestServerRejectsInvalidPort(t *testing.T) {
	m := newTestManager(t)
	for _, address := range []string{"1.1.1.1:abc", "1.1.1.1:0", "1.1.1.1:65536", "[2001:4860:4860::8888]:99999"} {
		if _, err := m.AddServer("dns-"+address, "udp", address, "", ""); err == nil {
			t.Errorf("地址 %q 应拒绝非法端口", address)
		}
	}
	if _, err := m.AddServer("bare-host", "udp", "1.1.1.1", "", ""); err != nil {
		t.Fatalf("无端口地址应保持默认端口语义: %v", err)
	}
	if _, err := m.AddServer("scheme-host", "udp", "udp://1.1.1.1:53", "", ""); err != nil {
		t.Fatalf("Karing 风格的带 scheme 地址应被接受: %v", err)
	}
	if _, err := m.AddServer("ipv6-host", "udp", "[2001:4860:4860::8888]:53", "", ""); err != nil {
		t.Fatalf("带端口的 IPv6 地址应被接受: %v", err)
	}
	if _, err := m.AddServer("wrong-scheme", "udp", "https://1.1.1.1:53", "", ""); err == nil {
		t.Fatal("DNS 类型与 URL scheme 不一致时应拒绝")
	}
}

func TestServerCannotDisableReferencedOrFinal(t *testing.T) {
	m := newTestManager(t)
	s := &config.DNSServer{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true}
	if err := m.DB.CreateDNSServer(s); err != nil {
		t.Fatal(err)
	}
	if err := m.DB.SetSetting(keyFinal, s.Tag); err != nil {
		t.Fatal(err)
	}
	if err := m.SetServerEnabled(s.ID, false); err == nil {
		t.Fatal("final DNS 服务器不应被停用")
	}
	if err := m.DB.SetSetting(keyFinal, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.DB.CreateDNSRule(&config.DNSRule{Type: "domain", Value: "example.com", Server: s.Tag, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetServerEnabled(s.ID, false); err == nil {
		t.Fatal("仍被 DNS 规则引用的服务器不应被停用")
	}
}

func TestServerCannotDisableOrDeleteAddressResolver(t *testing.T) {
	m := newTestManager(t)
	resolver := &config.DNSServer{Tag: "resolver", Type: "udp", Address: "1.1.1.1", Enabled: true}
	if err := m.DB.CreateDNSServer(resolver); err != nil {
		t.Fatal(err)
	}
	dependent := &config.DNSServer{Tag: "dependent", Type: "https", Address: "dns.example.com", AddressResolver: resolver.Tag, Enabled: true}
	if err := m.DB.CreateDNSServer(dependent); err != nil {
		t.Fatal(err)
	}
	if err := m.SetServerEnabled(resolver.ID, false); err == nil {
		t.Fatal("被 AddressResolver 引用的 DNS 服务器不应被停用")
	}
	if err := m.DeleteServer(resolver.ID); err == nil {
		t.Fatal("被 AddressResolver 引用的 DNS 服务器不应被删除")
	}
}

func TestServerRejectsDisabledAddressResolver(t *testing.T) {
	m := newTestManager(t)
	resolver := &config.DNSServer{Tag: "off", Type: "udp", Address: "1.1.1.1", Enabled: false}
	if err := m.DB.CreateDNSServer(resolver); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddServer("dependent", "https", "dns.example.com", resolver.Tag, ""); err == nil {
		t.Fatal("启用 DNS 服务器不应引用已停用的 resolver")
	}
}

func TestRuleRejectsDisabledServer(t *testing.T) {
	m := newTestManager(t)
	s := &config.DNSServer{Tag: "off", Type: "udp", Address: "1.1.1.1", Enabled: false}
	if err := m.DB.CreateDNSServer(s); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddRule("domain", "example.com", s.Tag); err == nil {
		t.Fatal("DNS 规则不应引用已停用的服务器")
	}
}
