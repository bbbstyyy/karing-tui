package config

import (
	"errors"
	"strings"
	"testing"
)

// C15 · 空代理组 fail-closed（见 CHECKLIST-v4.md C15）。
//
// 两类错误必须分开，因为它们指向完全不同的用户动作：
//
//	ErrNoUsableNodes   —— 全局没有任何可用节点：首次安装/尚未添加订阅，或节点全被禁用。
//	                      这是 onboarding 状态，不是「配置损坏」。
//	ErrEmptyProxyGroup —— 全局有可用节点，但**某个顶层代理组**解析后为空：成员引用了
//	                      已删除/已禁用的节点或代理组、子组失效、筛选条件为空。
//
// 旧行为是「空组回退注入 direct，保证配置可用」——它会让用户以为代理已生效，
// 实际流量全部直连。本项把这条隐式直连彻底删掉。

func fcNode(id int64, name string) *Node {
	return &Node{ID: id, Name: name, Protocol: "trojan", Server: "1.1.1.1", Port: 443,
		Enabled: true, Metadata: map[string]any{"password": "p"}}
}

// fcDefaultGroups 复刻 storage.EnsureDefaultGroups 在全新安装时创建的 Auto/Manual：
// 两者成员均为动态"全部节点"（all）。
func fcDefaultGroups() []*ProxyGroup {
	return []*ProxyGroup{
		{ID: 1, Name: "Auto", Type: "urltest", IntervalS: 300, Members: []ProxyGroupMember{{Type: "all"}}},
		{ID: 2, Name: "Manual", Type: "select", Members: []ProxyGroupMember{{Type: "all"}}},
	}
}

// A. 新安装：0 usable node + 默认 Auto/Manual(all) → ErrNoUsableNodes（onboarding），
// 且不得被判成 ErrEmptyProxyGroup。
func TestFailClosedFreshInstallNoUsableNodes(t *testing.T) {
	_, err := Generate(Snapshot{Settings: DefaultSettings(), ProxyGroups: fcDefaultGroups()})
	if err == nil {
		t.Fatal("零节点时默认 Auto/Manual 的 all 成员展开为空，必须 fail-closed")
	}
	var onboarding ErrNoUsableNodes
	if !errors.As(err, &onboarding) {
		t.Fatalf("期望 ErrNoUsableNodes，得到 %T: %v", err, err)
	}
	var empty ErrEmptyProxyGroup
	if errors.As(err, &empty) {
		t.Error("零节点属于 onboarding 状态，不应报成 ErrEmptyProxyGroup")
	}
	for _, want := range []string{"订阅", "节点"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("onboarding 文案应引导用户先添加 %q，得到 %q", want, err.Error())
		}
	}
}

// E. 所有节点全局禁用 → ErrNoUsableNodes。
func TestFailClosedAllNodesDisabled(t *testing.T) {
	nodes := []*Node{fcNode(1, "n1"), fcNode(2, "n2")}
	nodes[0].Enabled = false
	nodes[1].Enabled = false
	_, err := Generate(Snapshot{Settings: DefaultSettings(), Nodes: nodes, ProxyGroups: fcDefaultGroups()})
	var onboarding ErrNoUsableNodes
	if !errors.As(err, &onboarding) {
		t.Fatalf("全部节点被禁用时应返回 ErrNoUsableNodes，得到 %T: %v", err, err)
	}
	// 反向核对：启用其中一个节点后必须恢复正常，否则上一条断言可能因为「永远报 onboarding」而假通过。
	nodes[0].Enabled = true
	if _, err := Generate(Snapshot{Settings: DefaultSettings(), Nodes: nodes, ProxyGroups: fcDefaultGroups()}); err != nil {
		t.Fatalf("存在可用节点时不应 fail-closed: %v", err)
	}
}

// B. 全局有 usable node，但某个顶层组没有有效成员 → ErrEmptyProxyGroup 并指出组名。
func TestFailClosedEmptyProxyGroupNamesTheGroup(t *testing.T) {
	snap := Snapshot{
		Settings: DefaultSettings(),
		Nodes:    []*Node{fcNode(1, "n1")},
		ProxyGroups: []*ProxyGroup{{
			ID: 10, Name: "EmptyGroup", Type: "select",
			Members: []ProxyGroupMember{{Type: "node", ID: 99}}, // 已删除的节点
		}},
	}
	_, err := Generate(snap)
	var empty ErrEmptyProxyGroup
	if !errors.As(err, &empty) {
		t.Fatalf("全局有可用节点但组解析为空时应返回 ErrEmptyProxyGroup，得到 %T: %v", err, err)
	}
	if empty.Group != "EmptyGroup" {
		t.Errorf("错误应携带具体组名，得到 %q", empty.Group)
	}
	if !strings.Contains(err.Error(), "EmptyGroup") {
		t.Errorf("文案应包含组名: %q", err.Error())
	}
	var onboarding ErrNoUsableNodes
	if errors.As(err, &onboarding) {
		t.Error("全局存在可用节点，不得误判为 onboarding")
	}
}

// C（语义层）。递归展开时子组解析为空，不得注入 direct；父组只保留真实成员。
// 直接对 resolveMembers 断言，因为顶层空组判定（要求 4）会先一步报错，见下一条测试的说明。
func TestResolveMembersNestedEmptyInjectsNoDirect(t *testing.T) {
	nodes := []*Node{fcNode(1, "n1")}
	emptyChild := &ProxyGroup{ID: 11, Name: "EmptyChild", Type: "select",
		Members: []ProxyGroupMember{{Type: "node", ID: 99}}}
	parent := &ProxyGroup{ID: 10, Name: "Parent", Type: "select",
		Members: []ProxyGroupMember{{Type: "group", ID: 11}, {Type: "node", ID: 1}}}
	g := &generator{snap: Snapshot{Settings: DefaultSettings(), Nodes: nodes,
		ProxyGroups: []*ProxyGroup{parent, emptyChild}}}
	g.init()
	// resolveMembers 读的是 g.nodeTag；正常路径上它由 buildOutbounds 的节点循环
	// 通过 assignNodeTag 填好。这里手工补上，否则 add("") 会被空 tag 判断过滤掉。
	if _, err := g.assignNodeTag(nodes[0]); err != nil {
		t.Fatalf("assignNodeTag: %v", err)
	}

	child, err := g.resolveMembers(emptyChild, map[int64]bool{})
	if err != nil {
		t.Fatalf("resolveMembers(EmptyChild): %v", err)
	}
	if len(child) != 0 {
		t.Fatalf("递归层解析为空时必须返回空切片（旧行为是注入 direct），得到 %v", child)
	}

	members, err := g.resolveMembers(parent, map[int64]bool{})
	if err != nil {
		t.Fatalf("resolveMembers(Parent): %v", err)
	}
	if len(members) != 1 || members[0] != "n1" {
		t.Fatalf("父组应只保留真实成员 [n1]，得到 %v", members)
	}
}

// C（端到端）。Parent{失效子组引用, ValidNode} → 只包含 ValidNode，不注入 direct。
//
// 与清单原文的差异（必须记录）：清单 C 写成 Parent{EmptyChild, ValidNode}，但按改法第 4 条，
// **任何顶层组**解析为空都要报 ErrEmptyProxyGroup；若 EmptyChild 同时出现在 ProxyGroups 里
// （这是 resolveMembers 能找到它的前提），它会先撞上该判定，C 与 D 无法同时成立。
// 故这里用「子组引用已失效」表达空子组——清单「错误分类」第 2 条本来就把它列为成因之一。
func TestFailClosedNestedDanglingChildDropsSilently(t *testing.T) {
	snap := Snapshot{
		Settings: DefaultSettings(),
		Nodes:    []*Node{fcNode(1, "n1")},
		ProxyGroups: []*ProxyGroup{{
			ID: 10, Name: "Parent", Type: "select",
			Members: []ProxyGroupMember{{Type: "group", ID: 99}, {Type: "node", ID: 1}},
		}},
	}
	m := mustGenerate(t, snap)
	members := toStringSlice(t, findOutbound(t, m, "Parent")["outbounds"])
	if len(members) != 1 || members[0] != "n1" {
		t.Fatalf("Parent 成员应只含 n1（不得注入 direct），得到 %v", members)
	}
}

// D. Parent{失效子组引用} + 全局另有 unrelated usable node → ErrEmptyProxyGroup{Parent}。
func TestFailClosedParentWithOnlyDanglingChild(t *testing.T) {
	snap := Snapshot{
		Settings: DefaultSettings(),
		Nodes:    []*Node{fcNode(1, "unrelated")},
		ProxyGroups: []*ProxyGroup{{
			ID: 10, Name: "Parent", Type: "select",
			Members: []ProxyGroupMember{{Type: "group", ID: 99}},
		}},
	}
	_, err := Generate(snap)
	var empty ErrEmptyProxyGroup
	if !errors.As(err, &empty) {
		t.Fatalf("期望 ErrEmptyProxyGroup，得到 %T: %v", err, err)
	}
	if empty.Group != "Parent" {
		t.Errorf("应指出 Parent，得到 %q", empty.Group)
	}
	var onboarding ErrNoUsableNodes
	if errors.As(err, &onboarding) {
		t.Error("全局存在可用节点，不得误判为 onboarding")
	}
}

// 无代理组时不 fail-closed。config 包不假定调用方一定建了默认组，
// 纯 direct 骨架（如 core 集成测试、golden 骨架）必须继续可生成。
func TestNoProxyGroupsStillGenerates(t *testing.T) {
	if _, err := Generate(Snapshot{Settings: DefaultSettings()}); err != nil {
		t.Fatalf("没有代理组时不应 fail-closed: %v", err)
	}
}

// F. 路由规则引用代理组时，生成阶段必须保证引用的 outbound tag 真实存在。
func TestRouteTargetOutboundExists(t *testing.T) {
	snap := Snapshot{
		Settings:    DefaultSettings(),
		Nodes:       []*Node{fcNode(1, "n1")},
		ProxyGroups: []*ProxyGroup{{ID: 10, Name: "Auto", Type: "select", Members: []ProxyGroupMember{{Type: "all"}}}},
		RoutingGroups: []*RoutingGroup{{
			Name: "G", Target: "Auto", Enabled: true,
			Rules: []Rule{{Type: "domain_suffix", Value: "example.com", Enabled: true}},
		}},
	}
	m := mustGenerate(t, snap)
	tags := map[string]bool{}
	for _, o := range m["outbounds"].([]any) {
		tags[o.(map[string]any)["tag"].(string)] = true
	}
	used := 0
	for _, r := range m["route"].(map[string]any)["rules"].([]any) {
		out, _ := r.(map[string]any)["outbound"].(string)
		if out == "" {
			continue
		}
		used++
		if !tags[out] {
			t.Errorf("规则引用了不存在的出站 %q（已生成 %v）", out, tags)
		}
	}
	if used == 0 {
		t.Fatal("没有任何带 outbound 的路由规则，本测试未覆盖目标路径")
	}
}

// F 的反向核对：校验器本身必须真的会拦。只用正向断言（引用都存在 → 通过）无法
// 区分「校验生效」与「校验从不触发」。
func TestValidateOutboundRefsRejectsDanglingTag(t *testing.T) {
	out := []any{
		sbDirectOutbound{Type: "direct", Tag: "direct"},
		map[string]any{"type": "selector", "tag": "Auto", "outbounds": []string{"n1"}},
	}
	if err := validateOutboundRefs(out, &sbRoute{Final: "Auto", Rules: []sbRouteRule{{Outbound: "direct"}}}); err != nil {
		t.Fatalf("引用存在的出站不应报错: %v", err)
	}
	if err := validateOutboundRefs(out, &sbRoute{Final: "Ghost"}); err == nil {
		t.Error("route.final 指向不存在的出站应报错")
	}
	if err := validateOutboundRefs(out, &sbRoute{Rules: []sbRouteRule{{Outbound: "Ghost"}}}); err == nil {
		t.Error("规则 outbound 指向不存在的出站应报错")
	}
}
