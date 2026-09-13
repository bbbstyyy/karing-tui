package proxy

import (
	"strconv"
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
		t.Fatalf("NewPaths: %v", err)
	}
	db, err := storage.Open(paths)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewManager(db, paths, nil, nil)
}

// TestSetGroupSelectedAllMember 动态"全部节点"组可按节点 ID 选中任意启用节点。
func TestSetGroupSelectedAllMember(t *testing.T) {
	m := newTestManager(t)

	n := &config.Node{Name: "JP-01", Protocol: "shadowsocks", Server: "1.2.3.4", Port: 8388,
		Enabled: true, Metadata: map[string]any{"method": "aes-128-gcm", "password": "p"}}
	if err := m.DB.CreateNode(n); err != nil {
		t.Fatal(err)
	}
	disabled := &config.Node{Name: "US-01", Protocol: "shadowsocks", Server: "5.6.7.8", Port: 8388,
		Enabled: false, Metadata: map[string]any{"method": "aes-128-gcm", "password": "p"}}
	if err := m.DB.CreateNode(disabled); err != nil {
		t.Fatal(err)
	}
	g := &config.ProxyGroup{Name: "Manual", Type: "select", Members: []config.ProxyGroupMember{{Type: "all"}}}
	if err := m.DB.CreateProxyGroup(g); err != nil {
		t.Fatal(err)
	}

	// 启用节点可选
	if err := m.SetGroupSelected(g.ID, "node:"+strconv.FormatInt(n.ID, 10)); err != nil {
		t.Fatalf("选中启用节点: %v", err)
	}
	got, _ := m.DB.GetProxyGroup(g.ID)
	if got.Selected != "node:"+strconv.FormatInt(n.ID, 10) {
		t.Errorf("Selected = %q", got.Selected)
	}

	// 禁用节点与不存在的节点拒绝
	if err := m.SetGroupSelected(g.ID, "node:"+strconv.FormatInt(disabled.ID, 10)); err == nil {
		t.Error("禁用节点应被拒绝")
	}
	if err := m.SetGroupSelected(g.ID, "node:99999"); err == nil {
		t.Error("不存在的节点应被拒绝")
	}
	// 非 node 键在 all 组仍拒绝
	if err := m.SetGroupSelected(g.ID, "group:1"); err == nil {
		t.Error("all 组的 group 键应被拒绝")
	}

	// 更新动态成员组的其他字段/成员时，合法的 node:<id> 选择必须保留。
	got.Members = []config.ProxyGroupMember{{Type: "all"}}
	got.TestURL = "http://example.com/204"
	if err := m.UpdateGroup(got, got.Members); err != nil {
		t.Fatalf("UpdateGroup: %v", err)
	}
	got, _ = m.DB.GetProxyGroup(g.ID)
	if got.Selected != "node:"+strconv.FormatInt(n.ID, 10) {
		t.Fatalf("动态 all 组更新后 Selected = %q", got.Selected)
	}
}

func TestSetGroupSelectedAllMemberRejectsDisabledSubscription(t *testing.T) {
	m := newTestManager(t)
	sub := &config.Subscription{Name: "机场", URL: "https://example.com/sub"}
	if err := m.DB.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	n := &config.Node{Name: "sub-node", SubscriptionID: sub.ID, Protocol: "shadowsocks", Server: "1.2.3.4", Port: 443,
		Enabled: true, Metadata: map[string]any{"method": "aes-128-gcm", "password": "p"}}
	if err := m.DB.CreateNode(n); err != nil {
		t.Fatal(err)
	}
	g := &config.ProxyGroup{Name: "All", Type: "select", Members: []config.ProxyGroupMember{{Type: "all"}}}
	if err := m.DB.CreateProxyGroup(g); err != nil {
		t.Fatal(err)
	}
	if err := m.SetGroupSelected(g.ID, "node:"+strconv.FormatInt(n.ID, 10)); err != nil {
		t.Fatalf("启用订阅节点应可选: %v", err)
	}
	sub.Enabled = false
	if err := m.DB.UpdateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	if err := m.SetGroupSelected(g.ID, "node:"+strconv.FormatInt(n.ID, 10)); err == nil {
		t.Fatal("禁用订阅中的节点不应可选")
	}
}

// TestSetGroupSelectedExplicitMember 显式成员组仍按成员键精确校验。
func TestSetGroupSelectedExplicitMember(t *testing.T) {
	m := newTestManager(t)

	n := &config.Node{Name: "JP-01", Protocol: "shadowsocks", Server: "1.2.3.4", Port: 8388,
		Enabled: true, Metadata: map[string]any{"method": "aes-128-gcm", "password": "p"}}
	if err := m.DB.CreateNode(n); err != nil {
		t.Fatal(err)
	}
	other := &config.Node{Name: "US-01", Protocol: "shadowsocks", Server: "5.6.7.8", Port: 8388,
		Enabled: true, Metadata: map[string]any{"method": "aes-128-gcm", "password": "p"}}
	if err := m.DB.CreateNode(other); err != nil {
		t.Fatal(err)
	}
	g := &config.ProxyGroup{Name: "G", Type: "select",
		Members: []config.ProxyGroupMember{{Type: "node", ID: n.ID}}}
	if err := m.DB.CreateProxyGroup(g); err != nil {
		t.Fatal(err)
	}

	// 组内成员可选
	if err := m.SetGroupSelected(g.ID, "node:"+strconv.FormatInt(n.ID, 10)); err != nil {
		t.Fatalf("选中组成员: %v", err)
	}
	// 非成员（即使存在）拒绝
	if err := m.SetGroupSelected(g.ID, "node:"+strconv.FormatInt(other.ID, 10)); err == nil {
		t.Error("非成员节点应被拒绝")
	}
	// urltest 组拒绝手动选择
	ut := &config.ProxyGroup{Name: "Auto", Type: "urltest", Members: []config.ProxyGroupMember{{Type: "all"}}}
	if err := m.DB.CreateProxyGroup(ut); err != nil {
		t.Fatal(err)
	}
	if err := m.SetGroupSelected(ut.ID, "node:"+strconv.FormatInt(n.ID, 10)); err == nil {
		t.Error("urltest 组不应允许手动选择")
	}
}

func TestUpdateGroupRenamesRoutingReferences(t *testing.T) {
	m := newTestManager(t)
	g := &config.ProxyGroup{Name: "旧组", Type: "select"}
	if err := m.DB.CreateProxyGroup(g); err != nil {
		t.Fatal(err)
	}
	rg := &config.RoutingGroup{Name: "规则", Target: "旧组", Enabled: true}
	if err := m.DB.CreateRoutingGroup(rg); err != nil {
		t.Fatal(err)
	}
	g.Name = "新组"
	if err := m.UpdateGroup(g, nil); err != nil {
		t.Fatalf("UpdateGroup: %v", err)
	}
	got, err := m.DB.GetRoutingGroup(rg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Target != "新组" {
		t.Fatalf("路由目标未随组名更新: %q", got.Target)
	}
}
