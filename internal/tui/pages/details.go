package pages

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/catalog"
	"github.com/bbbstyyy/karing-tui/internal/config"
)

func nodeDetails(n *config.Node) string {
	return fmt.Sprintf("名称: %s\n协议: %s\n服务器: %s\n端口: %d\nTLS: %v\n传输: %s\n启用: %v\n延迟: %d ms\n凭据: 已隐藏（编辑时 Ctrl+R 显示）", n.Name, n.Protocol, n.Server, n.Port, n.TLS, n.Transport, n.Enabled, n.LatencyMS)
}

func subscriptionDetails(s *config.Subscription) string {
	expiry := "未提供"
	if !s.ExpireAt.IsZero() {
		expiry = s.ExpireAt.Format("2006-01-02 15:04")
	}
	updated := "从未更新"
	if !s.LastUpdated.IsZero() {
		updated = s.LastUpdated.Format("2006-01-02 15:04")
	}
	return fmt.Sprintf("名称: %s\nURL: %s\n状态: %s\n节点: %d\n下载通道: %s\n更新时间: %s\n已用流量: %s / %s\n到期: %s", s.Name, s.URL, enabledLabel(s.Enabled), s.NodeCount, s.DownloadStrategy, updated, humanBytes(s.TrafficUpload+s.TrafficDownload), humanBytes(s.TrafficTotal), expiry)
}

func (r *Rules) catalogDetails(ref catalog.Ref) string {
	cache := "未缓存；生成时按需安装，u 可立即下载"
	if r.app.Rules.CatalogCached(ref) {
		cache = "已缓存；u 更新"
	}
	destination := "浏览分类；从分流组规则按 c 进入可添加"
	if r.catBack == rulesGroupRl && r.cur != nil {
		destination = "Enter 添加到 " + r.cur.Name + " → " + r.cur.Target
	}
	return fmt.Sprintf("分类: %s\n标识: %s\n缓存: %s\n包含 IP 条件: %v\n%s", ref.String(), ref.Tag(), cache, ref.NeedsResolve(), destination)
}

func (r *Rules) selectionDetails() string {
	switch r.mode {
	case rulesGroups:
		if g, ok := r.selectedGroup(); ok {
			return fmt.Sprintf("名称: %s\n目标: %s\n规则: %d 条\n状态: %s\nEnter 查看组内规则", g.Name, g.Target, len(g.Rules), enabledLabel(g.Enabled))
		}
	case rulesGroupRl:
		if i, ok := r.selectedRuleIdx(); ok {
			return ruleDetails(r.cur.Rules[i]) + "\n目标: " + r.cur.Target
		}
	case rulesSets:
		if rs, ok := r.selectedSet(); ok {
			return fmt.Sprintf("名称: %s\n标识: %s\n格式: %s\nURL: %s\n缓存: %s\n状态: %s", rs.Name, rs.Tag, rs.Format, rs.URL, orDash(rs.CachedPath, "未缓存"), enabledLabel(rs.Enabled))
		}
		if ref, ok := r.selectedSetCatalog(); ok {
			return r.catalogDetails(ref)
		}
	case rulesCatalog:
		if ref, ok := r.selectedCatalogRef(); ok {
			return r.catalogDetails(ref)
		}
	}
	return ""
}

func (d *DNSPage) selectionDetails() string {
	if d.mode == dnsRules {
		if rules, i, ok := d.selectedRule(); ok {
			r := rules[i]
			return fmt.Sprintf("类型: %s\n条件: %s\n服务器: %s\n优先级: %d\n状态: %s", r.Type, r.Value, r.Server, r.Position+1, enabledLabel(r.Enabled))
		}
	}
	if d.mode == dnsServers {
		if servers, i, ok := d.selectedServer(); ok {
			s := servers[i]
			return fmt.Sprintf("名称: %s\n类型: %s\n地址: %s\n解析 DNS: %s\n出站: %s\n状态: %s", s.Tag, s.Type, orDash(s.Address, "系统解析器"), orDash(s.AddressResolver, "自动"), orDash(s.Detour, "DIRECT"), enabledLabel(s.Enabled))
		}
	}
	return ""
}

func ruleDetails(r config.Rule) string {
	value := r.Value
	if r.Type == "logical" {
		value = config.FormatLogicalExpr(r.Mode, r.Conditions)
	}
	return fmt.Sprintf("类型: %s\n条件: %s\n反转: %v\n启用: %v\n优先级: %d", r.Type, value, r.Invert, r.Enabled, r.Position+1)
}

func (g *Groups) memberDetails() string {
	member, ok := g.curMember()
	if !ok {
		return ""
	}
	if raw, ok := strings.CutPrefix(member.key, "node:"); ok {
		id, _ := strconv.ParseInt(raw, 10, 64)
		for _, n := range g.nodes {
			if n.ID == id {
				return nodeDetails(n)
			}
		}
	}
	return member.label + "\nSpace 设为保存选择；Ctrl+A 应用配置。"
}

func (g *Groups) groupDetails(grp *config.ProxyGroup) string {
	runtime := "未运行"
	if g.app.Core.IsRunning() {
		runtime = "待读取，r 刷新"
		if g.runtimeAt.Equal(g.app.Core.Status().StartedAt) {
			if info, ok := g.runtime[grp.Name]; ok {
				runtime = info.Now
			}
		}
	}
	return fmt.Sprintf("名称: %s\n类型: %s\n保存选择: %s\n运行实际: %s\n成员范围: %d 项\nEnter 查看可用成员", grp.Name, grp.Type, g.memberLabel(grp.Selected), runtime, len(grp.Members))
}
