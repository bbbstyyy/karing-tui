package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/storage"
	"github.com/bbbstyyy/karing-tui/internal/subscription"
)

// clashRuleTypes Clash 规则类型 → 内部规则类型。
var clashRuleTypes = map[string]string{
	"DOMAIN": "domain", "DOMAIN-SUFFIX": "domain_suffix", "DOMAIN-KEYWORD": "domain_keyword",
	"DOMAIN-REGEX": "domain_regex",
	"IP-CIDR":      "ip_cidr", "IP-CIDR6": "ip_cidr", "GEOIP": "geoip", "GEOSITE": "geosite",
}

// clashDoc 是 Clash YAML 中被导入的部分。
type clashDoc struct {
	Proxies     []map[string]any `yaml:"proxies"`
	ProxyGroups []struct {
		Name     string   `yaml:"name"`
		Type     string   `yaml:"type"`
		Proxies  []string `yaml:"proxies"`
		URL      string   `yaml:"url"`
		Interval int      `yaml:"interval"`
		Use      []string `yaml:"use"`
	} `yaml:"proxy-groups"`
	Rules []string `yaml:"rules"`
}

// ImportClash 从 Clash 配置 YAML 导入：proxies → 节点、proxy-groups → 代理组
// （fallback/load-balance 映射为 urltest）、rules → 分流组（按目标分组，
// MATCH → final 组）。RULE-SET 规则与 use 引用不支持，跳过并记录。
// 全部写入在单个事务中完成（C14-AUDIT）：中途失败整体回滚。
func ImportClash(db *storage.DB, content string) (rep Report, retErr error) {
	var doc clashDoc
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return Report{}, fmt.Errorf("解析 Clash YAML 失败: %w", err)
	}
	// 解析节点：纯内存计算，放在事务外，失败不触库。
	nodes, err := subscription.ParseClashProxies(content)
	if err != nil {
		return Report{}, fmt.Errorf("解析节点失败: %w", err)
	}
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return importClashTx(db, tx, &doc, nodes, &rep)
	}); err != nil {
		return rep, err
	}
	return rep, nil
}

func importClashTx(db *storage.DB, tx *sql.Tx, doc *clashDoc, nodes []*config.Node, rep *Report) error {
	// 节点
	nodeIDs := map[string]int64{}
	for _, n := range nodes {
		n.SubscriptionID = config.ManualSubscriptionID
		n.Enabled = true
		if err := probe("CreateNode"); err != nil {
			return err
		}
		if err := db.CreateNodeTx(tx, n); err != nil {
			return fmt.Errorf("写入节点 %q 失败: %w", n.Name, err)
		}
		nodeIDs[n.Name] = n.ID
	}
	rep.Nodes = len(nodes)

	// 代理组：先建空组拿 ID，再按原文件名回填成员（允许嵌套引用）
	type groupSpec struct {
		origName string
		name     string // 与现有组重名时会改名
		typ      string
		url      string
		intl     int
		raw      []string
		members  []config.ProxyGroupMember
	}
	specs := make([]*groupSpec, 0, len(doc.ProxyGroups))
	byOrig := map[string]*groupSpec{}
	for _, g := range doc.ProxyGroups {
		name := strings.TrimSpace(g.Name)
		if name == "" || byOrig[name] != nil {
			rep.skipf("代理组 %q 名称为空或重复", g.Name)
			continue
		}
		var typ string
		switch strings.ToLower(g.Type) {
		case "select":
			typ = "select"
		case "urltest", "fallback", "load-balance", "loadbalance":
			typ = "urltest"
			if !strings.EqualFold(g.Type, "urltest") {
				rep.skipf("组 %q 类型 %q 映射为 urltest", name, g.Type)
			}
		default:
			rep.skipf("代理组 %q 类型 %q 不支持", name, g.Type)
			continue
		}
		if len(g.Use) > 0 {
			rep.skipf("组 %q 的 use 代理提供者引用未导入", name)
		}
		spec := &groupSpec{origName: name, name: name, typ: typ, url: g.URL, intl: g.Interval, raw: g.Proxies}
		specs = append(specs, spec)
		byOrig[name] = spec
	}

	existingGroups, err := db.ListProxyGroupsTx(tx)
	if err != nil {
		return err
	}
	taken := map[string]bool{}
	for _, g := range existingGroups {
		taken[g.Name] = true
	}
	groupIDs := map[string]int64{}    // 原名 → 新组 ID
	groupNames := map[string]string{} // 原名 → 实际组名
	for _, spec := range specs {
		spec.name = uniqueName(taken, spec.name)
		g := &config.ProxyGroup{Name: spec.name, Type: spec.typ, TestURL: spec.url, IntervalS: spec.intl}
		if err := probe("CreateProxyGroup"); err != nil {
			return err
		}
		if err := db.CreateProxyGroupTx(tx, g); err != nil {
			return fmt.Errorf("写入代理组 %q 失败: %w", spec.name, err)
		}
		groupIDs[spec.origName] = g.ID
		groupNames[spec.origName] = spec.name
		taken[spec.name] = true
	}
	for _, spec := range specs {
		for _, m := range spec.raw {
			switch {
			case m == "DIRECT" || m == "REJECT" || m == "GLOBAL" || m == "PASS":
				rep.skipf("组 %q 成员 %q 为保留值，未导入", spec.name, m)
			case nodeIDs[m] != 0:
				spec.members = append(spec.members, config.ProxyGroupMember{Type: "node", ID: nodeIDs[m]})
			case byOrig[m] != nil:
				spec.members = append(spec.members, config.ProxyGroupMember{Type: "group", ID: groupIDs[m]})
			default:
				rep.skipf("组 %q 成员 %q 找不到对应节点或组", spec.name, m)
			}
		}
		g := &config.ProxyGroup{
			ID:   groupIDs[spec.origName],
			Name: spec.name, Type: spec.typ, TestURL: spec.url, IntervalS: spec.intl,
			Members: spec.members,
		}
		if err := probe("UpdateProxyGroup"); err != nil {
			return err
		}
		if err := db.UpdateProxyGroupTx(tx, g); err != nil {
			return fmt.Errorf("写入代理组 %q 成员失败: %w", spec.name, err)
		}
		rep.Groups++
	}

	// 分流规则：按连续目标段组织。按目标全量分组会把后出现的规则
	// 提前到其他目标规则之前，破坏 Clash 的 first-match 语义。
	targets := groupNames // 规则可用的目标（DIRECT/BLOCK/组名），组名冲突时映射到实际名称
	type routeSpec struct {
		target string
		rules  []config.Rule
	}
	var routeSpecs []*routeSpec
	var finalTarget string
	for _, line := range doc.Rules {
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			rep.skipf("规则 %q 格式非法", line)
			continue
		}
		typ := strings.ToUpper(strings.TrimSpace(parts[0]))
		if typ == "MATCH" {
			if t := resolveClashTarget(parts[1], targets); t != "" {
				if finalTarget == "" {
					finalTarget = t
				}
			} else {
				rep.skipf("MATCH 目标 %q 不可用", parts[1])
			}
			continue
		}
		if typ == "RULE-SET" {
			rep.skipf("规则集引用 %q 未导入（rule-providers 不支持）", line)
			continue
		}
		internal, ok := clashRuleTypes[typ]
		if !ok || len(parts) < 3 {
			rep.skipf("规则 %q 类型不支持", line)
			continue
		}
		value := strings.TrimSpace(parts[1])
		target := resolveClashTarget(parts[2], targets)
		if value == "" || target == "" {
			rep.skipf("规则 %q 值或目标不可用", line)
			continue
		}
		if len(routeSpecs) == 0 || routeSpecs[len(routeSpecs)-1].target != target {
			routeSpecs = append(routeSpecs, &routeSpec{target: target})
		}
		rule := config.Rule{
			Type: internal, Value: value, Enabled: true,
		}
		routeSpecs[len(routeSpecs)-1].rules = append(routeSpecs[len(routeSpecs)-1].rules, rule)
	}
	rep.Routings = len(routeSpecs)
	if finalTarget != "" {
		rep.Routings++
	}

	// 写入分流组（position 递增，排在现有组之后）
	existingRoutings, err := db.ListRoutingGroupsTx(tx)
	if err != nil {
		return err
	}
	pos := 0
	takenRoutings := map[string]bool{}
	for _, g := range existingRoutings {
		takenRoutings[g.Name] = true
		if g.Position > pos {
			pos = g.Position
		}
	}
	for _, spec := range routeSpecs {
		pos++
		name := uniqueName(takenRoutings, "迁移-"+spec.target)
		rg := &config.RoutingGroup{
			Name: name, Target: spec.target, Position: pos, Enabled: true,
			Rules: spec.rules,
		}
		if err := probe("CreateRoutingGroup"); err != nil {
			return err
		}
		if err := db.CreateRoutingGroupTx(tx, rg); err != nil {
			return fmt.Errorf("写入分流组 %q 失败: %w", name, err)
		}
		takenRoutings[name] = true
	}
	if finalTarget != "" {
		pos++
		rg := &config.RoutingGroup{
			Name: uniqueName(takenRoutings, "迁移-Final"), Target: finalTarget,
			Position: pos, Enabled: true,
			Rules: []config.Rule{{Type: "final", Enabled: true}},
		}
		if err := probe("CreateRoutingGroup"); err != nil {
			return err
		}
		if err := db.CreateRoutingGroupTx(tx, rg); err != nil {
			return fmt.Errorf("写入 final 分流组失败: %w", err)
		}
		takenRoutings[rg.Name] = true
	}
	return nil
}

// resolveClashTarget 把规则目标映射为内部目标（DIRECT/BLOCK/组名）。
func resolveClashTarget(s string, groups map[string]string) string {
	t := strings.TrimSpace(s)
	switch strings.ToUpper(t) {
	case "DIRECT":
		return "DIRECT"
	case "REJECT", "REJECT-DROP", "BLOCK":
		return "BLOCK"
	}
	if name := groups[t]; name != "" {
		return name
	}
	return ""
}

// uniqueName 在 taken 集合之外取唯一名（追加 " 2"、" 3"…）。
func uniqueName(taken map[string]bool, base string) string {
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := base + " " + strconv.Itoa(i)
		if !taken[candidate] {
			return candidate
		}
	}
}
