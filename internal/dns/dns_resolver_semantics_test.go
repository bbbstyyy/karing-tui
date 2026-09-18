package dns

import (
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// V8-1：DNS Manager 侧的语义收口。
//
// 老库形态（也是修复前的默认值）是 `remote = https 8.8.8.8 + AddressResolver=local`：
// 字面 IP 上挂着一个用不到的 resolver。它在四处造成可见后果——
// 正式配置多一个字段、环检测误报、local 无法被停用/删除、UI 显示一条假依赖。
// 本文件钉住后三处。

func createRawServer(t *testing.T, m *Manager, s *config.DNSServer) {
	t.Helper()
	if err := m.DB.CreateDNSServer(s); err != nil {
		t.Fatal(err)
	}
}

// rawServerByTag 直接从库里读，**不经过** ListServers 的规范化，
// 用来观察「磁盘上到底存了什么」。
func rawServerByTag(t *testing.T, m *Manager, tag string) *config.DNSServer {
	t.Helper()
	servers, err := m.DB.ListDNSServers()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range servers {
		if s.Tag == tag {
			return s
		}
	}
	t.Fatalf("库中没有 DNS 服务器 %q", tag)
	return nil
}

// 字面 IP 上的残留 resolver 不得阻止停用被「引用」的那个服务器。
//
// 判决性：旧实现只判 `other.AddressResolver == s.Tag`，于是用户永远停不掉 local。
func TestLiteralIPDNSFakeResolverDoesNotBlockDisable(t *testing.T) {
	m := newTestManager(t)
	local := &config.DNSServer{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true}
	createRawServer(t, m, local)
	createRawServer(t, m, &config.DNSServer{
		Tag: "remote", Type: "https", Address: "8.8.8.8", AddressResolver: "local", Enabled: true})

	// 刻意不建 DNS 规则、也不把 final 指向 local——只留下这一个假引用。
	if err := m.SetServerEnabled(local.ID, false); err != nil {
		t.Fatalf("字面 IP 上的残留 resolver 不应阻止停用: %v", err)
	}
}

// 同上，删除路径。
func TestLiteralIPDNSFakeResolverDoesNotBlockDelete(t *testing.T) {
	m := newTestManager(t)
	local := &config.DNSServer{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true}
	createRawServer(t, m, local)
	createRawServer(t, m, &config.DNSServer{
		Tag: "remote", Type: "https", Address: "8.8.8.8", AddressResolver: "local", Enabled: true})

	if err := m.DeleteServer(local.ID); err != nil {
		t.Fatalf("字面 IP 上的残留 resolver 不应阻止删除: %v", err)
	}
}

// 反向核对：域名地址构成的是真依赖，仍必须阻止停用与删除。
func TestDomainDNSRealResolverStillBlocksDisableAndDelete(t *testing.T) {
	m := newTestManager(t)
	local := &config.DNSServer{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true}
	createRawServer(t, m, local)
	createRawServer(t, m, &config.DNSServer{
		Tag: "doh", Type: "https", Address: "dns.google", AddressResolver: "local", Enabled: true})

	if err := m.SetServerEnabled(local.ID, false); err == nil {
		t.Fatal("域名型服务器真的依赖 local，停用必须被拒绝")
	}
	if err := m.DeleteServer(local.ID); err == nil {
		t.Fatal("域名型服务器真的依赖 local，删除必须被拒绝")
	}
}

// 数据入口收口：字面 IP 的 resolver 在保存时就清掉，域名地址原样保存。
func TestAddServerNormalizesResolverForLiteralIP(t *testing.T) {
	m := newTestManager(t)
	createRawServer(t, m, &config.DNSServer{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true})

	got, err := m.AddServer("remote", "https", "8.8.8.8", "local", "")
	if err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if got.AddressResolver != "" {
		t.Fatalf("字面 IP 不应保存 resolver，实得 %q", got.AddressResolver)
	}
	if raw := rawServerByTag(t, m, "remote"); raw.AddressResolver != "" {
		t.Fatalf("落库后也必须为空，实得 %q", raw.AddressResolver)
	}

	doh, err := m.AddServer("doh", "https", "dns.google", "local", "")
	if err != nil {
		t.Fatalf("AddServer(doh): %v", err)
	}
	if doh.AddressResolver != "local" {
		t.Fatalf("域名地址应原样保存 resolver，实得 %q", doh.AddressResolver)
	}
}

// ListServers 返回规范化后的副本：老库里的残留值不得显示在界面上。
func TestListServersHidesStaleResolver(t *testing.T) {
	m := newTestManager(t)
	createRawServer(t, m, &config.DNSServer{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true})
	createRawServer(t, m, &config.DNSServer{
		Tag: "remote", Type: "https", Address: "8.8.8.8", AddressResolver: "local", Enabled: true})

	servers, err := m.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range servers {
		if s.Tag != "remote" {
			continue
		}
		if s.AddressResolver != "" {
			t.Fatalf("界面视图不应显示用不到的 resolver，实得 %q", s.AddressResolver)
		}
		if s.Address != "8.8.8.8" || s.Type != "https" {
			t.Fatalf("其余字段不得被改动: %+v", s)
		}
	}
}

// LoadConfig 返回的副本必须规范化，但**不得写库**（只读启动路径不允许偷偷写入）。
func TestLoadConfigStripsStaleResolverWithoutWriting(t *testing.T) {
	m := newTestManager(t)
	createRawServer(t, m, &config.DNSServer{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true})
	createRawServer(t, m, &config.DNSServer{
		Tag: "remote", Type: "https", Address: "8.8.8.8", AddressResolver: "local", Enabled: true})

	cfg, err := m.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range cfg.Servers {
		if s.Tag == "remote" && s.AddressResolver != "" {
			t.Fatalf("LoadConfig 的副本应已规范化，实得 %q", s.AddressResolver)
		}
	}

	if raw := rawServerByTag(t, m, "remote"); raw.AddressResolver != "local" {
		t.Fatalf("LoadConfig 不得修改数据库，磁盘上应仍为 local，实得 %q", raw.AddressResolver)
	}
}

// 用户把地址从域名改成字面 IP 时，旧的 resolver 必须一并清掉。
func TestUpdateServerClearsResolverWhenAddressBecomesLiteralIP(t *testing.T) {
	m := newTestManager(t)
	createRawServer(t, m, &config.DNSServer{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true})
	doh := &config.DNSServer{Tag: "doh", Type: "https", Address: "dns.google", AddressResolver: "local", Enabled: true}
	createRawServer(t, m, doh)

	doh.Address = "8.8.8.8"
	if err := m.UpdateServer(doh); err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	if raw := rawServerByTag(t, m, "doh"); raw.AddressResolver != "" {
		t.Fatalf("地址变成字面 IP 后不应保留 resolver，实得 %q", raw.AddressResolver)
	}
}
