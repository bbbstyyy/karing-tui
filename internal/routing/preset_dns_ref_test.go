package routing

import (
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// V9-3 的「预设复活」路径：重命名 Auto 之后应用预置，DNS 不得被悄悄改接到新建的空组上。
//
// 背景：`ensurePresetProxyGroups` 在 Auto 缺失时会**就地补建一个 Auto**（既有行为，
// 本项不改）。若重命名没有把 `dns_servers.detour` 一起带走，就会出现一种比「生成失败」
// 更糟的半修复状态：DNS 的 detour 仍写着 "Auto"，而预置刚补建的那个新组正好叫 Auto ——
// 于是引用「看起来是通的」，实际把 DNS 出站换到了一条用户没在用的组上
// （实测：补建出来的是 `urltest + 动态「全部节点」` 的新行，与用户改名后的组
// 是不同的行、不同的成员集）。
//
// 判决性：旧实现在第一处断言就会红（detour 仍为 "Auto"），并且第二处断言额外证明
// 「指向的组就是用户那个组」——只比对名字不足以区分「同一行」与「同名的新行」。
func TestApplyPresetDoesNotRebindDNSDetourAfterGroupRename(t *testing.T) {
	m := newPresetTestManager(t)
	// 默认形态：remote 是字面 IP、detour = Auto（V8-1 之后不再带假 resolver）
	if err := m.DB.CreateDNSServer(&config.DNSServer{
		Tag: "remote", Type: "https", Address: "8.8.8.8",
		Detour: storage.DefaultGroupAuto, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	original := groupByName(t, m, storage.DefaultGroupAuto)

	// 重命名走 storage 的原子入口 —— proxy.UpdateGroup 最终调用的就是它
	// （proxy 侧已由 TestRenameProxyGroupUpdatesDNSDetourAtomically 覆盖整条链路）。
	const newName = "Auto2"
	renamed := *original
	renamed.Name = newName
	if err := m.DB.UpdateProxyGroupRenamed(&renamed, storage.DefaultGroupAuto); err != nil {
		t.Fatalf("重命名代理组: %v", err)
	}
	if got := dnsDetourOf(t, m, "remote"); got != newName {
		t.Fatalf("重命名后 DNS detour = %q，期望 %q", got, newName)
	}

	if _, err := m.ApplyPreset(PresetCN, PresetMerge); err != nil {
		t.Fatalf("ApplyPreset: %v", err)
	}

	if got := dnsDetourOf(t, m, "remote"); got != newName {
		t.Errorf("应用预置后 DNS detour = %q，期望仍为 %q（不得被补建的空组接管）", got, newName)
	}
	// 反向核对：补建确实发生了，否则上一条断言是空转。
	rebuilt := groupByName(t, m, storage.DefaultGroupAuto)
	if rebuilt == nil {
		t.Error("夹具失效：Auto 缺失时应用预置应当就地补建")
	}
	// 把「被接管」到底是什么形态记下来（实测修正）：补建的不是一个空组，而是
	// `urltest + 动态「全部节点」` 的新行 —— 与用户改名后的组是两个不同的行、不同的成员集。
	// 所以 detour 掉回 "Auto" 时引用会「看起来是通的」，实际把 DNS 出站换到了这个新建组上。
	if rebuilt != nil {
		t.Logf("补建的 %s：type=%s members=%+v", storage.DefaultGroupAuto, rebuilt.Type, rebuilt.Members)
	}
	// 再反向核对一次「指向的是同一行」：只比名字区分不了「用户那个组」与「同名的新组」。
	if got := groupByName(t, m, newName); got == nil || got.ID != original.ID {
		t.Errorf("DNS 指向的 %q 已经不是用户改名后的那个组（原 ID=%d，现 %+v）", newName, original.ID, got)
	}
}

func groupByName(t *testing.T, m *Manager, name string) *config.ProxyGroup {
	t.Helper()
	groups, err := m.DB.ListProxyGroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.Name == name {
			return g
		}
	}
	return nil
}

func dnsDetourOf(t *testing.T, m *Manager, tag string) string {
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
