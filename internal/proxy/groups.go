package proxy

import (
	"fmt"
	"github.com/bbbstyyy/karing-tui/internal/validation"
	"strconv"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// 组类型与保留出站标签。
var groupTypes = map[string]bool{"select": true, "urltest": true}

var reservedTags = map[string]bool{"direct": true, "block": true, "dns": true}

// CreateGroup 新建代理组。
func (m *Manager) CreateGroup(name, typ, testURL string, intervalS int, members []config.ProxyGroupMember) (*config.ProxyGroup, error) {
	g := &config.ProxyGroup{
		Name:      strings.TrimSpace(name),
		Type:      strings.TrimSpace(typ),
		TestURL:   strings.TrimSpace(testURL),
		IntervalS: intervalS,
		Members:   members,
	}
	if err := m.validateGroup(g, 0); err != nil {
		return nil, err
	}
	if err := m.DB.CreateProxyGroup(g); err != nil {
		return nil, err
	}
	m.Logf("创建代理组 %q (%s, %d 成员)", g.Name, g.Type, len(members))
	return g, nil
}

// UpdateGroup 更新代理组（含成员整体替换）。
func (m *Manager) UpdateGroup(g *config.ProxyGroup, members []config.ProxyGroupMember) error {
	old, err := m.DB.GetProxyGroup(g.ID)
	if err != nil {
		return err
	}
	g.Name = strings.TrimSpace(g.Name)
	g.Type = strings.TrimSpace(g.Type)
	g.TestURL = strings.TrimSpace(g.TestURL)
	g.Members = members
	if err := m.validateGroup(g, g.ID); err != nil {
		return err
	}
	if old.Name != g.Name {
		if err := m.DB.UpdateProxyGroupRenamed(g, old.Name); err != nil {
			return err
		}
	} else if err := m.DB.UpdateProxyGroup(g); err != nil {
		return err
	}
	// 选中项若不在新成员里则清空。动态 all 组允许 node:<id> 作为
	// 选中项，即使该键不是 proxy_group_members 中的显式成员。
	if g.Selected != "" && !m.selectionExists(g, g.Selected) {
		g.Selected = ""
		if err := m.DB.SetProxyGroupSelected(g.ID, ""); err != nil {
			return err
		}
	}
	m.Logf("修改代理组 %q (%s, %d 成员)", g.Name, g.Type, len(members))
	return nil
}

// DeleteGroup 删除代理组；被分流组引用时拒绝。
func (m *Manager) DeleteGroup(id int64) error {
	g, err := m.DB.GetProxyGroup(id)
	if err != nil {
		return err
	}
	routings, err := m.DB.ListRoutingGroups()
	if err != nil {
		return err
	}
	for _, rg := range routings {
		if rg.Target == g.Name {
			return fmt.Errorf("分流组 %q 正在引用代理组 %q，请先修改其目标", rg.Name, g.Name)
		}
	}
	if err := m.DB.DeleteProxyGroup(id); err != nil {
		return err
	}
	m.Logf("删除代理组 %q", g.Name)
	return nil
}

// SetGroupMembers 整体替换组成员。
func (m *Manager) SetGroupMembers(id int64, members []config.ProxyGroupMember) error {
	g, err := m.DB.GetProxyGroup(id)
	if err != nil {
		return err
	}
	g.Members = members
	if err := m.validateGroup(g, g.ID); err != nil {
		return err
	}
	if err := m.DB.UpdateProxyGroup(g); err != nil {
		return err
	}
	if g.Selected != "" && !m.selectionExists(g, g.Selected) {
		g.Selected = ""
		return m.DB.SetProxyGroupSelected(g.ID, "")
	}
	m.Logf("代理组 %q 成员更新为 %d 项", g.Name, len(members))
	return nil
}

// SetGroupSelected 设置 select 组的当前选中成员并持久化。
// memberKey 形如 "node:12" / "group:3"（"all" 表示跟随动态全部节点）。
// 组含动态"全部节点"成员时，任意启用节点均可选（生成配置时按展开成员解析）。
func (m *Manager) SetGroupSelected(groupID int64, memberKey string) error {
	g, err := m.DB.GetProxyGroup(groupID)
	if err != nil {
		return err
	}
	if g.Type != "select" {
		return fmt.Errorf("代理组 %q 不是 select 类型", g.Name)
	}
	if memberKey == "all" {
		return fmt.Errorf("全部节点是动态成员范围，请选择展开后的具体节点")
	}
	if strings.HasPrefix(memberKey, "node:") {
		id, err := strconv.ParseInt(strings.TrimPrefix(memberKey, "node:"), 10, 64)
		if err != nil {
			return fmt.Errorf("节点引用无效")
		}
		n, err := m.DB.GetNode(id)
		if err != nil || !m.nodeUsable(n) {
			return fmt.Errorf("节点不存在、已禁用或所属订阅已停用")
		}
	}
	if memberKey != "" && !m.memberExists(g, memberKey) {
		if err := m.checkAllGroupNode(g, memberKey); err != nil {
			return err
		}
	}
	if err := m.DB.SetProxyGroupSelected(groupID, memberKey); err != nil {
		return err
	}
	m.Logf("代理组 %q 选中 %s", g.Name, memberDisplayKey(memberKey))
	return nil
}

// EffectiveMembers expands the dynamic all placeholder exactly at the node
// boundary. Nested groups remain selectable groups; disabled subscriptions and
// nodes are omitted just as in application.buildSnapshot/config.Generate.
func (m *Manager) EffectiveMembers(id int64) ([]config.ProxyGroupMember, error) {
	g, err := m.DB.GetProxyGroup(id)
	if err != nil {
		return nil, err
	}
	nodes, err := m.DB.ListNodes(0)
	if err != nil {
		return nil, err
	}
	subs, err := m.DB.ListSubscriptions()
	if err != nil {
		return nil, err
	}
	enabledSubs := map[int64]bool{config.ManualSubscriptionID: true}
	for _, s := range subs {
		enabledSubs[s.ID] = s.Enabled
	}
	usable := map[int64]bool{}
	for _, n := range nodes {
		usable[n.ID] = n.Enabled && enabledSubs[n.SubscriptionID]
	}
	var result []config.ProxyGroupMember
	seen := map[string]bool{}
	add := func(mem config.ProxyGroupMember) {
		if !seen[mem.MemberKey()] {
			seen[mem.MemberKey()] = true
			result = append(result, mem)
		}
	}
	for _, mem := range g.Members {
		switch mem.Type {
		case "all":
			for _, n := range nodes {
				if usable[n.ID] {
					add(config.ProxyGroupMember{Type: "node", ID: n.ID})
				}
			}
		case "node":
			if usable[mem.ID] {
				add(mem)
			}
		case "group":
			add(mem)
		}
	}
	return result, nil
}

// checkAllGroupNode 校验动态"全部节点"组的节点选中项：节点及所属订阅须启用。
func (m *Manager) checkAllGroupNode(g *config.ProxyGroup, memberKey string) error {
	rest, ok := strings.CutPrefix(memberKey, "node:")
	if ok && m.hasAllMember(g) {
		if id, err := strconv.ParseInt(rest, 10, 64); err == nil {
			if n, err := m.DB.GetNode(id); err == nil && m.nodeUsable(n) {
				return nil
			}
			return fmt.Errorf("节点 %q 不存在或已禁用", memberDisplayKey(memberKey))
		}
	}
	return fmt.Errorf("成员 %q 不在代理组 %q 中", memberDisplayKey(memberKey), g.Name)
}

// nodeUsable 与配置快照保持一致：手动节点只看自身状态，订阅节点还要求订阅启用。
func (m *Manager) nodeUsable(n *config.Node) bool {
	if n == nil || !n.Enabled {
		return false
	}
	if n.SubscriptionID == config.ManualSubscriptionID {
		return true
	}
	s, err := m.DB.GetSubscription(n.SubscriptionID)
	return err == nil && s.Enabled
}

// hasAllMember 报告组是否含动态"全部节点"成员。
func (m *Manager) hasAllMember(g *config.ProxyGroup) bool {
	for _, mem := range g.Members {
		if mem.Type == "all" {
			return true
		}
	}
	return false
}

// validateGroup 校验组字段、成员合法性与环引用。
func (m *Manager) validateGroup(g *config.ProxyGroup, selfID int64) error {
	if g.Name == "" {
		return validation.New("name", "代理组名称不能为空")
	}
	if !groupTypes[g.Type] {
		return validation.New("type", "组类型 %q 不支持（仅 select/urltest，sing-box 无 fallback/loadbalance）", g.Type)
	}
	if g.Type == "urltest" && g.IntervalS < 0 {
		return validation.New("interval", "测速间隔非法")
	}
	// 名称唯一（组间）+ 保留标签
	groups, err := m.DB.ListProxyGroups()
	if err != nil {
		return err
	}
	for _, other := range groups {
		if other.ID != selfID && other.Name == g.Name {
			return validation.New("name", "代理组名称 %q 已存在", g.Name)
		}
	}
	if reservedTags[strings.ToLower(g.Name)] {
		return validation.New("name", "名称 %q 是保留出站标签", g.Name)
	}
	// 成员合法性
	for _, mem := range g.Members {
		switch mem.Type {
		case "all":
		case "node":
			if _, err := m.DB.GetNode(mem.ID); err != nil {
				return validation.New("members", "成员节点 %d 不存在", mem.ID)
			}
		case "group":
			if mem.ID == selfID {
				return validation.New("members", "代理组不能嵌套自身")
			}
			if _, err := m.DB.GetProxyGroup(mem.ID); err != nil {
				return validation.New("members", "成员代理组 %d 不存在", mem.ID)
			}
		default:
			return validation.New("members", "未知成员类型 %q", mem.Type)
		}
	}
	return m.checkCycle(g, selfID)
}

// checkCycle 沿嵌套组引用做 DFS，检测环。
func (m *Manager) checkCycle(g *config.ProxyGroup, selfID int64) error {
	groups, err := m.DB.ListProxyGroups()
	if err != nil {
		return err
	}
	byID := map[int64]*config.ProxyGroup{}
	for _, o := range groups {
		byID[o.ID] = o
	}
	if g.ID != 0 && g.ID != selfID {
		byID[g.ID] = g // 更新场景用新成员覆盖
	} else if selfID != 0 {
		byID[selfID] = g
	}
	visiting := map[int64]bool{}
	visited := map[int64]bool{}
	var dfs func(id int64) error
	dfs = func(id int64) error {
		if visiting[id] {
			return fmt.Errorf("代理组嵌套形成环: %s", g.Name)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		cur := byID[id]
		if cur != nil {
			for _, mem := range cur.Members {
				if mem.Type == "group" {
					if err := dfs(mem.ID); err != nil {
						return err
					}
				}
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for id := range byID {
		if err := dfs(id); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) memberExists(g *config.ProxyGroup, memberKey string) bool {
	for _, mem := range g.Members {
		if mem.MemberKey() == memberKey {
			return true
		}
	}
	return false
}

func (m *Manager) selectionExists(g *config.ProxyGroup, memberKey string) bool {
	if m.memberExists(g, memberKey) {
		return true
	}
	return m.hasAllMember(g) && m.checkAllGroupNode(g, memberKey) == nil
}

// memberDisplayKey 选中键的简短展示。
func memberDisplayKey(memberKey string) string {
	if memberKey == "" {
		return "（未选择）"
	}
	return memberKey
}
