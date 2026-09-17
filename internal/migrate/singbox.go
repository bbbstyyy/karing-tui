package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// singboxProtocols 可导入为节点的 sing-box 出站类型。
var singboxProtocols = map[string]bool{
	"shadowsocks": true, "vmess": true, "vless": true, "trojan": true,
	"hysteria2": true, "hysteria": true, "tuic": true, "ssr": true,
	"socks": true, "http": true, "ssh": true, "anytls": true,
	"shadowtls": true, "naive": true, "tor": true,
}

// ImportSingBox 从 sing-box 配置 JSON 导入（Karing「导出完整配置」即此格式）：
// 出站 → 节点、selector/urltest → 代理组、route.rules → 分流组（多匹配字段
// 的规则映射为 and 逻辑规则）、remote 规则集 → 规则集。dns/inbounds/experimental
// 与 direct/block/dns 出站忽略；含不支持匹配字段的规则跳过并记录。
// 全部写入在单个事务中完成（C14-AUDIT）：中途失败整体回滚。
func ImportSingBox(db *storage.DB, content string) (rep Report, retErr error) {
	var doc struct {
		Outbounds []map[string]any `json:"outbounds"`
		Route     struct {
			Rules   []map[string]any `json:"rules"`
			Final   string           `json:"final"`
			RuleSet []map[string]any `json:"rule_set"`
		} `json:"route"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		return Report{}, fmt.Errorf("sing-box JSON 解析失败: %w", err)
	}
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return importSingBoxTx(db, tx, &doc, &rep)
	}); err != nil {
		return rep, err
	}
	return rep, nil
}

func importSingBoxTx(db *storage.DB, tx *sql.Tx, doc *struct {
	Outbounds []map[string]any `json:"outbounds"`
	Route     struct {
		Rules   []map[string]any `json:"rules"`
		Final   string           `json:"final"`
		RuleSet []map[string]any `json:"rule_set"`
	} `json:"route"`
}, rep *Report) error {
	// 规则集（仅 remote srs/json 可直接复用）
	rsTags := map[string]bool{}
	existingSets, err := db.ListRuleSetsTx(tx)
	if err != nil {
		return err
	}
	for _, existing := range existingSets {
		if existing.Enabled {
			rsTags[existing.Tag] = true
		}
	}
	for _, rs := range doc.Route.RuleSet {
		tag := jStr(rs, "tag")
		typ := jStr(rs, "type")
		format := jStr(rs, "format")
		url := jStr(rs, "url")
		if tag != "" {
			if existing, ok := findRuleSet(existingSets, tag); ok {
				if existing.Enabled {
					rsTags[tag] = true
					rep.skipf("规则集 %q 已存在，复用现有记录", tag)
				} else {
					delete(rsTags, tag)
					rep.skipf("规则集 %q 已存在但已停用，引用未导入", tag)
				}
				continue
			}
		}
		if typ != "remote" || url == "" || (format != "srs" && format != "json") {
			rep.skipf("规则集 %q（type=%s）未导入，仅支持 remote srs/json", tag, typ)
			continue
		}
		item := &config.RuleSet{
			Name: tag, Tag: tag, SourceType: "remote", Format: format, URL: url, Enabled: true,
		}
		if err := probe("CreateRuleSet"); err != nil {
			return err
		}
		if err := db.CreateRuleSetTx(tx, item); err != nil {
			return fmt.Errorf("写入规则集 %q 失败: %w", tag, err)
		}
		rsTags[tag] = true
		rep.RuleSets++
	}

	// 出站 → 节点 / 组
	nodeIDs := map[string]int64{}    // tag → 节点 ID
	infraTags := map[string]string{} // tag → direct/block/dns（基础出站）
	var groupObs []map[string]any
	for _, ob := range doc.Outbounds {
		tag := jStr(ob, "tag")
		typ := jStr(ob, "type")
		switch typ {
		case "selector", "urltest":
			groupObs = append(groupObs, ob)
		case "direct", "block", "dns":
			infraTags[tag] = typ
		case "":
			// 无类型，忽略
		default:
			if !singboxProtocols[typ] {
				rep.skipf("出站 %q 类型 %q 不支持", tag, typ)
				continue
			}
			n, err := singboxOutboundToNode(ob)
			if err != nil {
				rep.skipf("出站 %q 未导入: %v", tag, err)
				continue
			}
			n.SubscriptionID = config.ManualSubscriptionID
			n.Enabled = true
			if err := probe("CreateNode"); err != nil {
				return err
			}
			if err := db.CreateNodeTx(tx, n); err != nil {
				return fmt.Errorf("写入节点 %q 失败: %w", n.Name, err)
			}
			nodeIDs[tag] = n.ID
			rep.Nodes++
		}
	}

	// 组：先建空组拿 ID，再回填成员
	type groupSpec struct {
		ob      map[string]any
		name    string
		typ     string
		members []config.ProxyGroupMember
	}
	specs := make([]*groupSpec, 0, len(groupObs))
	byTag := map[string]*groupSpec{}
	for _, ob := range groupObs {
		tag := jStr(ob, "tag")
		typ := "select"
		if jStr(ob, "type") == "urltest" {
			typ = "urltest"
		}
		spec := &groupSpec{ob: ob, name: tag, typ: typ}
		specs = append(specs, spec)
		byTag[tag] = spec
	}
	existingGroups, err := db.ListProxyGroupsTx(tx)
	if err != nil {
		return err
	}
	taken := map[string]bool{}
	for _, g := range existingGroups {
		taken[g.Name] = true
	}
	groupIDs := map[string]int64{}
	for _, spec := range specs {
		spec.name = uniqueName(taken, spec.name)
		g := &config.ProxyGroup{Name: spec.name, Type: spec.typ}
		if spec.typ == "urltest" {
			g.TestURL = jStr(spec.ob, "url")
			g.IntervalS = int(jDur(spec.ob, "interval").Seconds())
		}
		if err := probe("CreateProxyGroup"); err != nil {
			return err
		}
		if err := db.CreateProxyGroupTx(tx, g); err != nil {
			return fmt.Errorf("写入代理组 %q 失败: %w", spec.name, err)
		}
		groupIDs[jStr(spec.ob, "tag")] = g.ID
		taken[spec.name] = true
	}
	for _, spec := range specs {
		memberTags := jStrSlice(spec.ob["outbounds"])
		for _, m := range memberTags {
			switch {
			case infraTags[m] != "":
				rep.skipf("组 %q 成员 %q 为基础出站，未导入", spec.name, m)
			case nodeIDs[m] != 0:
				spec.members = append(spec.members, config.ProxyGroupMember{Type: "node", ID: nodeIDs[m]})
			case byTag[m] != nil:
				spec.members = append(spec.members, config.ProxyGroupMember{Type: "group", ID: groupIDs[m]})
			default:
				rep.skipf("组 %q 成员 %q 找不到对应节点或组", spec.name, m)
			}
		}
		g := &config.ProxyGroup{
			ID:   groupIDs[jStr(spec.ob, "tag")],
			Name: spec.name, Type: spec.typ,
		}
		if spec.typ == "urltest" {
			g.TestURL = jStr(spec.ob, "url")
			g.IntervalS = int(jDur(spec.ob, "interval").Seconds())
		}
		g.Members = spec.members
		// selector 的 default 持久化
		if spec.typ == "select" {
			if def := jStr(spec.ob, "default"); def != "" {
				if id := nodeIDs[def]; id != 0 {
					g.Selected = "node:" + strconv.FormatInt(id, 10)
				} else if byTag[def] != nil {
					g.Selected = "group:" + strconv.FormatInt(groupIDs[def], 10)
				}
			}
		}
		if err := probe("UpdateProxyGroup"); err != nil {
			return err
		}
		if err := db.UpdateProxyGroupTx(tx, g); err != nil {
			return fmt.Errorf("写入代理组 %q 成员失败: %w", spec.name, err)
		}
		// UpdateProxyGroup 不写 selected 列，selector 默认值单独持久化
		if g.Selected != "" {
			if err := db.SetProxyGroupSelectedTx(tx, g.ID, g.Selected); err != nil {
				return fmt.Errorf("写入代理组 %q 选中项失败: %w", spec.name, err)
			}
		}
		rep.Groups++
	}

	// 路由规则 → 分流组（按目标分组，保持文件顺序）
	// 目标解析：组名 / 节点（节点目标自动包一个 select 组）/ direct 出站→DIRECT /
	// block 出站或 reject 动作→BLOCK
	targetName := func(tag string) (string, error) {
		switch {
		case infraTags[tag] == "direct":
			return "DIRECT", nil
		case infraTags[tag] == "block":
			return "BLOCK", nil
		case byTag[tag] != nil:
			return byTag[tag].name, nil
		case nodeIDs[tag] != 0:
			name, _, err := nodeTargetNameTx(db, tx, taken, tag, nodeIDs[tag])
			return name, err
		}
		return "", nil
	}
	type routeSpec struct {
		target string
		rules  []config.Rule
	}
	// Keep one spec per consecutive target run. Grouping non-consecutive rules
	// by target would move them across other targets and change first-match
	// routing semantics.
	var routeSpecs []*routeSpec
	var finalTarget string
	for _, rule := range doc.Route.Rules {
		action := jStr(rule, "action")
		outbound := jStr(rule, "outbound")
		if action != "" && action != "route" && action != "reject" {
			rep.skipf("路由规则（action=%s）未导入", action)
			continue
		}
		var target string
		if action == "reject" {
			target = "BLOCK"
		} else if outbound != "" {
			t, err := targetName(outbound)
			if err != nil {
				return err
			}
			target = t
			if target == "" {
				rep.skipf("路由规则目标 %q 不可用", outbound)
				continue
			}
		} else {
			rep.skipf("路由规则缺少 outbound/action，未导入")
			continue
		}
		// 匹配字段 → 条件；出现不支持的字段则整条跳过（避免改变语义）
		var conds []config.RuleCondition
		unsupported := ""
		for field, values := range rule {
			switch field {
			case "domain", "domain_suffix", "domain_keyword", "ip_cidr", "geoip", "geosite":
				conds = append(conds, config.RuleCondition{Type: field, Value: strings.Join(jStrSlice(values), ",")})
			case "domain_regex":
				// 内部模型的 domain_regex 是单条正则（值不按逗号拆分）。sing-box 的
				// domain_regex 数组内多条是 or 关系，合并为等价的 (?:a)|(?:b) 单条正则。
				regexes := jStrSlice(values)
				if len(regexes) == 0 {
					continue
				}
				value := regexes[0]
				if len(regexes) > 1 {
					parts := make([]string, 0, len(regexes))
					for _, re := range regexes {
						parts = append(parts, "(?:"+re+")")
					}
					value = strings.Join(parts, "|")
				}
				conds = append(conds, config.RuleCondition{Type: "domain_regex", Value: value})
			case "rule_set":
				tags := jStrSlice(values)
				var usable []string
				for _, t := range tags {
					if rsTags[t] {
						usable = append(usable, t)
					} else {
						rep.skipf("规则引用的规则集 %q 未导入", t)
					}
				}
				if len(usable) > 0 {
					conds = append(conds, config.RuleCondition{Type: "rule_set", Value: strings.Join(usable, ",")})
				}
			case "action", "outbound", "invert":
				// 非匹配字段
			default:
				if unsupported == "" {
					unsupported = field
				}
			}
		}
		if unsupported != "" {
			rep.skipf("路由规则含不支持的条件 %q，未导入", unsupported)
			continue
		}
		if len(conds) == 0 {
			continue
		}
		r := config.Rule{Enabled: true, Invert: jBool(rule, "invert")}
		if len(conds) == 1 {
			r.Type = conds[0].Type
			r.Value = conds[0].Value
			r.Invert = r.Invert != conds[0].Invert
		} else {
			r.Type = "logical"
			r.Mode = "and"
			r.Conditions = conds
		}
		if len(routeSpecs) == 0 || routeSpecs[len(routeSpecs)-1].target != target {
			routeSpecs = append(routeSpecs, &routeSpec{target: target})
		}
		routeSpecs[len(routeSpecs)-1].rules = append(routeSpecs[len(routeSpecs)-1].rules, r)
	}
	if fin := doc.Route.Final; fin != "" {
		t, err := targetName(fin)
		if err != nil {
			return err
		}
		if t != "" {
			finalTarget = t
		} else {
			rep.skipf("route.final %q 不可用", fin)
		}
	}
	rep.Routings = len(routeSpecs)
	if finalTarget != "" {
		rep.Routings++
	}

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
		rg := &config.RoutingGroup{
			Name: uniqueName(takenRoutings, "迁移-"+spec.target), Target: spec.target,
			Position: pos, Enabled: true, Rules: spec.rules,
		}
		if err := probe("CreateRoutingGroup"); err != nil {
			return err
		}
		if err := db.CreateRoutingGroupTx(tx, rg); err != nil {
			return fmt.Errorf("写入分流组 %q 失败: %w", rg.Name, err)
		}
		takenRoutings[rg.Name] = true
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

func findRuleSet(sets []*config.RuleSet, tag string) (*config.RuleSet, bool) {
	for _, rs := range sets {
		if rs.Tag == tag {
			return rs, true
		}
	}
	return nil, false
}

// nodeTargetNameTx 为以节点为目标的规则自动创建一个 select 组（内部模型要求
// 分流目标为代理组），组名基于节点 tag。建组失败向上传播中止导入（C14-AUDIT：
// 原实现吞掉错误把目标置空，静默跳过引用它的规则——在事务化模型下写失败
// 本来就该中止整个操作）。
func nodeTargetNameTx(db *storage.DB, tx *sql.Tx, taken map[string]bool, tag string, nodeID int64) (string, int64, error) {
	name := uniqueName(taken, tag)
	g := &config.ProxyGroup{
		Name: name, Type: "select",
		Members: []config.ProxyGroupMember{{Type: "node", ID: nodeID}},
	}
	if err := probe("CreateProxyGroup"); err != nil {
		return "", 0, err
	}
	if err := db.CreateProxyGroupTx(tx, g); err != nil {
		return "", 0, fmt.Errorf("为节点目标 %q 创建代理组失败: %w", tag, err)
	}
	taken[name] = true
	return name, g.ID, nil
}

// singboxOutboundToNode 把 sing-box 出站转换为统一节点（字段映射与生成器一致）。
func singboxOutboundToNode(ob map[string]any) (*config.Node, error) {
	typ := jStr(ob, "type")
	tag := jStr(ob, "tag")
	server := jStr(ob, "server")
	port, hasPort := singboxPort(ob)
	if tag == "" {
		return nil, fmt.Errorf("缺少 tag")
	}
	if typ != "tor" && (server == "" || !hasPort) {
		return nil, fmt.Errorf("缺少 server/server_port/server_ports")
	}
	meta := map[string]any{}
	if ports := jStrSlice(ob["server_ports"]); len(ports) > 0 {
		meta["server_ports"] = ports
	}
	var tls bool
	transport := ""
	if t, ok := ob["tls"].(map[string]any); ok {
		if jBool(t, "enabled") {
			tls = true
		}
		if v := jStr(t, "server_name"); v != "" {
			meta["sni"] = v
		}
		if jBool(t, "insecure") {
			meta["allow_insecure"] = true
		}
		if alpn := jStrSlice(t["alpn"]); len(alpn) > 0 {
			meta["alpn"] = alpn
		}
		if u, ok := t["utls"].(map[string]any); ok {
			if v := jStr(u, "fingerprint"); v != "" {
				meta["fingerprint"] = v
			}
		}
		if r, ok := t["reality"].(map[string]any); ok {
			if v := jStr(r, "public_key"); v != "" {
				meta["reality_public_key"] = v
				tls = true
			}
			if v := jStr(r, "short_id"); v != "" {
				meta["reality_short_id"] = v
			}
		}
	}
	if tr, ok := ob["transport"].(map[string]any); ok {
		transport = jStr(tr, "type")
		if v := jStr(tr, "path"); v != "" {
			meta["path"] = v
		}
		if v := jStr(tr, "service_name"); v != "" {
			meta["service_name"] = v
		}
		if h, ok := tr["headers"].(map[string]any); ok {
			if v := jStr(h, "Host"); v != "" {
				meta["host"] = v
			}
		}
		if hs := jStrSlice(tr["host"]); len(hs) > 0 {
			meta["host"] = hs[0]
		}
	}
	switch typ {
	case "shadowsocks":
		meta["method"] = jStr(ob, "method")
		meta["password"] = jStr(ob, "password")
	case "vmess":
		meta["uuid"] = jStr(ob, "uuid")
		meta["method"] = orDefault(jStr(ob, "security"), "auto")
		if v := jNum(ob, "alter_id"); v != 0 {
			meta["alter_id"] = int(v)
		}
	case "vless":
		meta["uuid"] = jStr(ob, "uuid")
		meta["encryption"] = "none"
		if v := jStr(ob, "flow"); v != "" {
			meta["flow"] = v
		}
	case "trojan":
		meta["password"] = jStr(ob, "password")
	case "hysteria2":
		meta["password"] = jStr(ob, "password")
		if o, ok := ob["obfs"].(map[string]any); ok {
			meta["obfs"] = jStr(o, "type")
			meta["obfs_password"] = jStr(o, "password")
		}
		if v := jNum(ob, "up_mbps"); v != 0 {
			meta["up_mbps"] = int(v)
		}
		if v := jNum(ob, "down_mbps"); v != 0 {
			meta["down_mbps"] = int(v)
		}
	case "hysteria":
		meta["password"] = jStr(ob, "auth_str")
		if v := jNum(ob, "up_mbps"); v != 0 {
			meta["up_mbps"] = int(v)
		}
		if v := jNum(ob, "down_mbps"); v != 0 {
			meta["down_mbps"] = int(v)
		}
		if v := jStr(ob, "obfs"); v != "" {
			meta["obfs"] = v
		}
	case "tuic":
		meta["uuid"] = jStr(ob, "uuid")
		meta["password"] = jStr(ob, "password")
		if v := jStr(ob, "congestion_control"); v != "" {
			meta["congestion_control"] = v
		}
		if v := jStr(ob, "udp_relay_mode"); v != "" {
			meta["udp_relay_mode"] = v
		}
	case "socks", "http":
		if v := jStr(ob, "username"); v != "" {
			meta["username"] = v
		}
		if v := jStr(ob, "password"); v != "" {
			meta["password"] = v
		}
	case "ssh":
		meta["user"] = jStr(ob, "user")
		meta["password"] = jStr(ob, "password")
		meta["private_key"] = jStr(ob, "private_key")
		meta["private_key_path"] = jStr(ob, "private_key_path")
	case "anytls", "shadowtls":
		meta["password"] = jStr(ob, "password")
		if v := jNum(ob, "version"); v != 0 {
			meta["version"] = int(v)
		}
	case "naive":
		meta["username"] = jStr(ob, "username")
		meta["password"] = jStr(ob, "password")
		if jBool(ob, "quic") {
			meta["quic"] = true
		}
	case "ssr":
		meta["method"] = jStr(ob, "method")
		meta["password"] = jStr(ob, "password")
		meta["protocol"] = jStr(ob, "protocol")
		meta["obfs"] = jStr(ob, "obfs")
		if v := jStr(ob, "protocol_param"); v != "" {
			meta["protocol_param"] = v
		}
		if v := jStr(ob, "obfs_param"); v != "" {
			meta["obfs_param"] = v
		}
	}
	return &config.Node{
		Name: tag, Protocol: typ, Server: server, Port: port,
		TLS: tls, Transport: transport, Metadata: meta,
	}, nil
}

func singboxPort(ob map[string]any) (int, bool) {
	if port := int(jNum(ob, "server_port")); port >= 1 && port <= 65535 {
		return port, true
	}
	for _, raw := range jStrSlice(ob["server_ports"]) {
		part := strings.TrimSpace(raw)
		if i := strings.IndexAny(part, ":-"); i >= 0 {
			part = strings.TrimSpace(part[:i])
		}
		port, err := strconv.Atoi(part)
		if err == nil && port >= 1 && port <= 65535 {
			return port, true
		}
	}
	return 0, false
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// --- JSON map 访问辅助（数字统一 float64）---

func jStr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func jBool(m map[string]any, key string) bool {
	v, ok := m[key].(bool)
	return ok && v
}

func jNum(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

func jStrSlice(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range list {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func jDur(m map[string]any, key string) time.Duration {
	switch v := m[key].(type) {
	case string:
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	case float64:
		return time.Duration(v) * time.Second
	}
	return 0
}
