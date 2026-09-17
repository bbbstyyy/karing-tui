package pages

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
)

func col(title string, width, priority int, right bool) components.Column {
	return components.Column{Title: title, Width: width, Priority: priority, Right: right}
}

func enabledLabel(enabled bool) string {
	if enabled {
		return "启用"
	}
	return "停用"
}

func latencyLabel(n *config.Node) string {
	if n.LatencyMS >= 0 {
		return fmt.Sprintf("%d ms", n.LatencyMS)
	}
	if !n.LastTested.IsZero() {
		return "失败"
	}
	return "未测"
}

// Profiles 的列定义提到包级变量：它们在每个渲染周期都会被用到，
// 不必每次 SetTable 都重建切片。列切片只读，不共享可变状态。
var (
	profilesSubColumns = []components.Column{
		col("订阅", 18, 0, false), col("状态", 4, 0, false),
		col("节点", 6, 0, true), col("更新时间", 14, 1, false),
	}
	profilesNodeColumns = []components.Column{
		col("节点", 18, 0, false), col("延迟", 8, 0, true), col("状态", 4, 0, false),
		col("协议", 12, 1, false), col("服务器", 22, 2, false),
	}
)

func (g *Groups) table() {
	var rows [][]string
	switch g.mode {
	case groupsList:
		for _, grp := range g.groups {
			rows = append(rows, []string{grp.Name, strconv.Itoa(len(grp.Members)), grp.Type, g.memberLabel(grp.Selected)})
		}
		g.list.SetTable([]components.Column{col("代理组", 16, 0, false), col("成员", 6, 0, true), col("类型", 8, 0, false), col("保存选择", 20, 1, false)}, rows, g.list.Keys)
	case groupsDetail:
		if g.cur == nil {
			return
		}
		nodes := make(map[int64]*config.Node, len(g.nodes))
		for _, node := range g.nodes {
			nodes[node.ID] = node
		}
		coreStatus := g.app.Core.Status()
		runtimeValid := g.runtimeAt.Equal(coreStatus.StartedAt) && g.app.Core.IsRunning()
		for _, member := range g.members {
			name, lat, state := "", "", ""
			if node, ok := nodes[member.ID]; member.Type == "node" && ok {
				name, lat = node.Name, latencyLabel(node)
			} else {
				name = g.memberLabel(member.MemberKey())
			}
			if g.cur.Selected == member.MemberKey() {
				state = "已保存"
			}
			if runtimeValid && g.runtime[g.cur.Name].Now == name {
				state = strings.TrimSpace(state + " 运行中")
			}
			rows = append(rows, []string{name, lat, state, member.Type})
		}
		g.list.SetTable([]components.Column{col("成员", 18, 0, false), col("延迟", 8, 0, true), col("选择状态", 14, 0, false), col("类型", 7, 1, false)}, rows, g.list.Keys)
	}
}

func (r *Rules) table() {
	var rows [][]string
	switch r.mode {
	case rulesGroups:
		// 按层分组渲染：层标题作为不可选中行，层的组数/启用数与层内序号都在这里
		// 统一生成（buildGroupList），避免渲染与键位映射两处各写一遍。
		r.buildGroupList()
	case rulesGroupRl:
		if r.cur == nil {
			return
		}
		for i, rule := range r.cur.Rules {
			value, typ := rule.Value, rule.Type
			if typ == "logical" {
				value = config.FormatLogicalExpr(rule.Mode, rule.Conditions)
				typ = "logical:" + rule.Mode
			}
			if typ == "final" {
				value = "其余全部流量"
			}
			if rule.Invert {
				value = "NOT " + value
			}
			rows = append(rows, []string{value, strconv.Itoa(i + 1), typ, enabledLabel(rule.Enabled)})
		}
		r.list.SetTable([]components.Column{col("条件", 20, 0, false), col("优先级", 6, 0, true), col("类型", 15, 1, false), col("状态", 4, 0, false)}, rows, r.list.Keys)
	case rulesSets:
		for _, rs := range r.sets {
			cached := "未缓存"
			if rs.CachedPath != "" {
				cached = "已缓存"
			}
			rows = append(rows, []string{rs.Name, rs.Tag, cached, enabledLabel(rs.Enabled), rs.Format})
		}
		for _, ref := range r.catRefsInUse {
			cached := "未缓存"
			if r.app.Rules.CatalogCached(ref) {
				cached = "已缓存"
			}
			rows = append(rows, []string{ref.String(), ref.Tag(), cached, "内置", "srs"})
		}
		r.list.SetTable([]components.Column{col("规则集", 18, 0, false), col("标识", 22, 2, false), col("缓存", 6, 0, false), col("状态", 4, 0, false), col("格式", 4, 1, false)}, rows, r.list.Keys)
	case rulesCatalog:
		// reloadCatalog caches the filesystem and reference checks in list.Items.
		for i, ref := range r.catHits {
			cached, inUse, kind := "未缓存", "未引用", "域名"
			if i < len(r.list.Items) && strings.Contains(r.list.Items[i], "已缓存") {
				cached = "已缓存"
			}
			if i < len(r.list.Items) && (strings.HasPrefix(r.list.Items[i], "* ") || strings.Contains(r.list.Items[i], "已引用")) {
				inUse = "已引用"
			}
			if ref.NeedsResolve() {
				kind = "IP 条件"
			}
			rows = append(rows, []string{ref.String(), inUse, cached, kind})
		}
		r.list.SetTable([]components.Column{col("分类", 22, 0, false), col("引用", 6, 0, false), col("缓存", 6, 0, false), col("条件", 7, 1, false)}, rows, r.list.Keys)
	}
}

// table 只把已在 reload() 中读入内存的 d.servers / d.rules 建模成表格行。
//
// 它由 View 每帧调用，因此绝不能回查数据库：改动前这里会执行
// ListServers() / ListRules()，并在失败时改写 d.err——View 里的副作用。
func (d *DNSPage) table() {
	var rows [][]string
	switch d.mode {
	case dnsServers:
		for _, s := range d.servers {
			state := enabledLabel(s.Enabled)
			if s.Tag == d.cfg.Final {
				state += " 默认"
			}
			address := s.Address
			if s.Type == "local" {
				address = "系统解析器"
			}
			rows = append(rows, []string{s.Tag, s.Type, state, address})
		}
		d.list.SetTable([]components.Column{col("DNS 服务器", 16, 0, false), col("类型", 6, 0, false), col("状态", 9, 0, false), col("地址", 26, 1, false)}, rows, d.list.Keys)
	case dnsRules:
		for i, r := range d.rules {
			rows = append(rows, []string{r.Value, strconv.Itoa(i + 1), r.Server, enabledLabel(r.Enabled), r.Type})
		}
		d.list.SetTable([]components.Column{col("DNS 条件", 18, 0, false), col("优先级", 6, 0, true), col("服务器", 12, 0, false), col("状态", 4, 0, false), col("类型", 14, 1, false)}, rows, d.list.Keys)
	}
}
