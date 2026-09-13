package routing

import (
	"fmt"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// DefaultGroupAI 默认分流方案使用的 AI 代理组名。
const DefaultGroupAI = "AI"

// EnsureDefaultRouting 首次使用（无任何分流组）时初始化默认分流方案：
// 中国大陆 → DIRECT、Telegram → Auto、AI → AI 组、Final → Auto。
// 依赖内置规则集与默认代理组（Auto）已就绪。
func EnsureDefaultRouting(db *storage.DB) error {
	var n int
	n, err := db.TableCount("routing_groups")
	if err != nil {
		return fmt.Errorf("统计分流组数量失败: %w", err)
	}
	if n > 0 {
		return nil
	}

	// 确保 AI 代理组存在
	if err := ensureAIProxyGroup(db); err != nil {
		return err
	}

	// 目标与规则。规则集以内置分类引用书写（见 internal/catalog）：无需预先把分类
	// 写进 rulesets 表，生成配置时按需产出 rule_set 条目并按需下载缓存。
	finalTarget := storage.DefaultGroupAuto
	aiTarget := DefaultGroupAI
	scheme := []struct {
		name    string
		target  string
		tagList []string
	}{
		{"中国大陆", "DIRECT", []string{"geosite:cn", "geoip:cn"}},
		{"Telegram", storage.DefaultGroupAuto, []string{"geosite:telegram", "geoip:telegram"}},
		{"AI", aiTarget, []string{"geosite:category-ai-!cn"}},
		{"Final", finalTarget, nil}, // final 规则
	}
	for _, s := range scheme {
		g := &config.RoutingGroup{
			Name:    s.name,
			Target:  s.target,
			Enabled: true,
		}
		if s.tagList == nil {
			g.Rules = []config.Rule{{Type: "final", Value: "", Enabled: true}}
		} else {
			for _, tag := range s.tagList {
				g.Rules = append(g.Rules, config.Rule{Type: "rule_set", Value: tag, Enabled: true})
			}
		}
		if err := db.CreateRoutingGroup(g); err != nil {
			return err
		}
	}
	return nil
}

// ensureAIProxyGroup 确保默认 AI 代理组存在（select，全部节点）。
func ensureAIProxyGroup(db *storage.DB) error {
	groups, err := db.ListProxyGroups()
	if err != nil {
		return err
	}
	for _, g := range groups {
		if g.Name == DefaultGroupAI {
			return nil
		}
	}
	ai := &config.ProxyGroup{
		Name:    DefaultGroupAI,
		Type:    "select",
		Members: []config.ProxyGroupMember{{Type: "all"}},
	}
	return db.CreateProxyGroup(ai)
}
