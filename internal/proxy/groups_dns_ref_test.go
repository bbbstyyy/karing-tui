package proxy

import (
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// V9-3：代理组的名称不只被 `routing_groups.target` 引用，还被我 DNS 服务器的
// 「出站代理组」（`dns_servers.detour`）引用 —— 而**默认配置里 `remote.detour = Auto`**。
//
// 全库的组名引用只有这两处（`proxy_groups.selected` 用 `group:<id>`、
// `settings.download_proxy` 是 URL，都不是组名）。少了任何一处，
// 重命名/删除代理组都会留下悬空引用，直到生成配置才报
// 「DNS 服务器 remote 的 Detour Auto 不是已生成的出站」。

// rawDNSDetour 直接读库取 detour（不经任何规范化），用来观察磁盘上到底存了什么。
func rawDNSDetour(t *testing.T, m *Manager, tag string) string {
	t.Helper()
	servers, err := m.DB.ListDNSServers()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range servers {
		if s.Tag == tag {
			return s.Detour
		}
	}
	t.Fatalf("库中没有 DNS 服务器 %q", tag)
	return ""
}

// assertNoDanglingDNSDetour 是本项的验收不变量：任何一个非空 detour 都必须是真实存在的
// 代理组名（`direct` 这类直连写法除外——它在生成侧被归一成「不写 detour」）。
func assertNoDanglingDNSDetour(t *testing.T, m *Manager) {
	t.Helper()
	groups, err := m.DB.ListProxyGroups()
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(groups))
	for _, g := range groups {
		names[g.Name] = true
	}
	servers, err := m.DB.ListDNSServers()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range servers {
		if s.Detour == "" || strings.EqualFold(s.Detour, "direct") {
			continue
		}
		if !names[s.Detour] {
			t.Errorf("DNS 服务器 %q 的 detour %q 悬空：代理组里没有这个名字", s.Tag, s.Detour)
		}
	}
}

// 判决性：重命名代理组必须把 DNS 的 detour 一起改写。
//
// 旧实现只 cascade `routing_groups.target`，于是改名后 detour 仍是旧名；
// `config.Generate` 会在 buildDNS 的 detour 引用校验处直接失败。
func TestRenameProxyGroupUpdatesDNSDetourAtomically(t *testing.T) {
	m := newTestManager(t)
	// 默认形态（与 dns.EnsureDefaultDNS 一致：字面 IP、无 AddressResolver）
	if err := m.DB.CreateDNSServer(&config.DNSServer{
		Tag: "remote", Type: "https", Address: "8.8.8.8",
		Detour: storage.DefaultGroupAuto, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	auto, err := m.CreateGroup(storage.DefaultGroupAuto, "urltest", config.DefaultTestURL, 300,
		[]config.ProxyGroupMember{{Type: "all"}})
	if err != nil {
		t.Fatal(err)
	}

	const newName = "Auto2"
	renamed := *auto
	renamed.Name = newName
	if err := m.UpdateGroup(&renamed, auto.Members); err != nil {
		t.Fatalf("重命名代理组: %v", err)
	}

	if got := rawDNSDetour(t, m, "remote"); got != newName {
		t.Errorf("重命名后 DNS detour = %q，期望 %q —— 否则生成配置会报「Detour 不是已生成的出站」",
			got, newName)
	}
	assertNoDanglingDNSDetour(t, m)
}

// 反向核对：未被任何 DNS 引用的代理组照常可改名，不得误伤。
func TestRenameProxyGroupWithoutDNSReferenceStillWorks(t *testing.T) {
	m := newTestManager(t)
	g, err := m.CreateGroup("plain", "select", "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	renamed := *g
	renamed.Name = "plain2"
	if err := m.UpdateGroup(&renamed, g.Members); err != nil {
		t.Fatalf("无引用时改名不得失败: %v", err)
	}
	assertNoDanglingDNSDetour(t, m)
}

// 判决性：删除被 DNS 出站引用的代理组必须被拒绝（与既有的 `routing_groups.target`
// 检查同口径），且错误要点名是谁在引用 —— 否则用户在 DNS 页里找不到该改哪一条。
func TestDeleteProxyGroupReferencedByDNSIsRejected(t *testing.T) {
	m := newTestManager(t)
	auto, err := m.CreateGroup(storage.DefaultGroupAuto, "urltest", config.DefaultTestURL, 300,
		[]config.ProxyGroupMember{{Type: "all"}})
	if err != nil {
		t.Fatal(err)
	}
	remote := &config.DNSServer{
		Tag: "remote", Type: "https", Address: "8.8.8.8",
		Detour: storage.DefaultGroupAuto, Enabled: true}
	if err := m.DB.CreateDNSServer(remote); err != nil {
		t.Fatal(err)
	}

	err = m.DeleteGroup(auto.ID)
	if err == nil {
		t.Fatal("删除正被 DNS 出站引用的代理组必须被拒绝")
	}
	if !strings.Contains(err.Error(), "remote") {
		t.Errorf("错误文案应点名引用者 remote，实得 %v", err)
	}
	if !strings.Contains(err.Error(), storage.DefaultGroupAuto) {
		t.Errorf("错误文案应点名代理组名，实得 %v", err)
	}
	if _, err := m.DB.GetProxyGroup(auto.ID); err != nil {
		t.Errorf("拒绝时不得删除组: %v", err)
	}

	// 反向核对：解除引用后必须能删，证明上一条不是把删除整体关掉了。
	if err := m.DB.DeleteDNSServer(remote.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteGroup(auto.ID); err != nil {
		t.Fatalf("解除引用后应当可以删除: %v", err)
	}
}

// 停用的 DNS 行同样构成引用：检查口径**不看 Enabled**，与既有的 `RoutingGroup.Target`
// 检查一致（`proxy/groups.go` 里那条也不看）。
//
// 另一层理由：V9-1 之后重新启用这条 DNS 会被 validateServer 拒绝（detour 指向不存在的组），
// 用户会撞上一条更难理解的错。
func TestDeleteProxyGroupReferencedByDisabledDNSIsAlsoRejected(t *testing.T) {
	m := newTestManager(t)
	auto, err := m.CreateGroup(storage.DefaultGroupAuto, "urltest", config.DefaultTestURL, 300,
		[]config.ProxyGroupMember{{Type: "all"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.DB.CreateDNSServer(&config.DNSServer{
		Tag: "remote", Type: "https", Address: "8.8.8.8",
		Detour: storage.DefaultGroupAuto, Enabled: false}); err != nil {
		t.Fatal(err)
	}

	if err := m.DeleteGroup(auto.ID); err == nil {
		t.Fatal("停用行持有的 detour 引用也应拦住删除")
	}
	if _, err := m.DB.GetProxyGroup(auto.ID); err != nil {
		t.Errorf("拒绝时不得删除组: %v", err)
	}
}

// 反向核对：没有任何 DNS 引用时删除照常成功。
func TestDeleteProxyGroupWithoutDNSReferenceStillWorks(t *testing.T) {
	m := newTestManager(t)
	g, err := m.CreateGroup("plain", "select", "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteGroup(g.ID); err != nil {
		t.Fatalf("无 DNS 引用时删除不得失败: %v", err)
	}
	assertNoDanglingDNSDetour(t, m)
}

// 引用校验不得把「直连」写法误判成组名：detour 填 direct/DIRECT 时删组必须照常成功。
func TestDeleteProxyGroupNotBlockedByDirectDetour(t *testing.T) {
	m := newTestManager(t)
	for i, detour := range []string{"direct", "DIRECT"} {
		g, err := m.CreateGroup("g"+string(rune('a'+i)), "select", "", 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.DB.CreateDNSServer(&config.DNSServer{
			Tag: "d" + string(rune('a'+i)), Type: "udp", Address: "1.1.1.1",
			Detour: detour, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if err := m.DeleteGroup(g.ID); err != nil {
			t.Errorf("detour=%q 是直连写法，不该拦住删除代理组: %v", detour, err)
		}
	}
}
