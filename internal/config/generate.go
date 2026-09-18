package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/catalog"
)

// Snapshot 是配置生成器的输入：内部模型的某一时刻快照。
type Snapshot struct {
	Settings      Settings
	Nodes         []*Node         // 节点池全部节点；仅启用节点生成出站
	ProxyGroups   []*ProxyGroup   // 全部代理组
	RoutingGroups []*RoutingGroup // 全部分流组（Position 顺序）
	RuleSets      []*RuleSet      // 全部规则集
	DNS           *DNSConfig      // DNS 配置；nil 或无服务器时不生成 dns 段

	// RuleSetCacheDir 内置分类规则集的缓存目录（cache/rulesets）。应用在生成配置前
	// 会优先把编译期嵌入的 .srs 按需安装到这里；分类引用生成 route.rule_set 条目时，
	// 在此查找 <tag>.srs 决定用 local 还是 remote。留空则一律生成 remote，由 sing-box
	// 启动时自行下载。
	RuleSetCacheDir string
}

// defaultFakeIPRange 与 dns.DefaultFakeIPRange 保持一致（config 不能反向依赖 dns）。
const defaultFakeIPRange = "198.18.0.0/15"

// --- sing-box JSON 输出结构 ---
// 使用定序 struct 而非 map，保证同一内部状态多次生成结果字节一致（幂等）。

type sbLog struct {
	Level     string `json:"level"`
	Timestamp bool   `json:"timestamp"`
}

type sbMixedInbound struct {
	Type       string `json:"type"`
	Tag        string `json:"tag"`
	Listen     string `json:"listen"`
	ListenPort int    `json:"listen_port"`
}

type sbDirectOutbound struct {
	Type string `json:"type"`
	Tag  string `json:"tag"`
}

// sbRouteRule 路由规则。geoip/geosite 为 legacy 字段（1.14 仍接受，运行需 geoip.db/geosite.db 资源）。
// Type=logical 时为逻辑规则：Mode 组合 Rules 子规则（子规则仅含匹配字段，无动作）。
type sbRouteRule struct {
	Type          string        `json:"type,omitempty"` // logical
	Mode          string        `json:"mode,omitempty"` // and / or（logical）
	Rules         []sbRouteRule `json:"rules,omitempty"`
	Invert        bool          `json:"invert,omitempty"`
	Port          []int         `json:"port,omitempty"`
	Domain        []string      `json:"domain,omitempty"`
	DomainSuffix  []string      `json:"domain_suffix,omitempty"`
	DomainKeyword []string      `json:"domain_keyword,omitempty"`
	DomainRegex   []string      `json:"domain_regex,omitempty"`
	IPCIDR        []string      `json:"ip_cidr,omitempty"`
	IPIsPrivate   bool          `json:"ip_is_private,omitempty"`
	GeoIP         []string      `json:"geoip,omitempty"`
	Geosite       []string      `json:"geosite,omitempty"`
	RuleSet       []string      `json:"rule_set,omitempty"`
	Action        string        `json:"action,omitempty"` // reject / resolve 等非 route 动作
	Server        string        `json:"server,omitempty"` // resolve 动作指定的 DNS 服务器
	Outbound      string        `json:"outbound,omitempty"`
}

// sbRuleSet 规则集引用；有本地缓存用 local，缺缓存回退 remote。
// 缓存下载由 Go 层使用 Settings.DownloadProxy 完成，不能把 HTTP 代理地址
// 写入 sing-box 已废弃且只接受出站 tag 的 download_detour。
type sbRuleSet struct {
	Type   string `json:"type"`
	Tag    string `json:"tag"`
	Format string `json:"format"`
	Path   string `json:"path,omitempty"`
	URL    string `json:"url,omitempty"`
}

type sbRoute struct {
	Rules                 []sbRouteRule `json:"rules,omitempty"`
	RuleSet               []sbRuleSet   `json:"rule_set,omitempty"`
	Final                 string        `json:"final,omitempty"`
	DefaultDomainResolver string        `json:"default_domain_resolver,omitempty"`
	AutoDetectInterface   bool          `json:"auto_detect_interface,omitempty"`
}

// sbDNSRule DNS 规则（1.12+ 新格式；legacy dns 配置在 1.14 已移除）。
type sbDNSRule struct {
	Domain        []string `json:"domain,omitempty"`
	DomainSuffix  []string `json:"domain_suffix,omitempty"`
	DomainKeyword []string `json:"domain_keyword,omitempty"`
	RuleSet       []string `json:"rule_set,omitempty"`
	QueryType     []string `json:"query_type,omitempty"`
	Server        string   `json:"server,omitempty"`
}

type sbDNS struct {
	Servers  []map[string]any `json:"servers,omitempty"`
	Rules    []sbDNSRule      `json:"rules,omitempty"`
	Final    string           `json:"final,omitempty"`
	Strategy string           `json:"strategy,omitempty"`
}

type sbConfig struct {
	Log          *sbLog          `json:"log,omitempty"`
	DNS          *sbDNS          `json:"dns,omitempty"`
	Inbounds     []any           `json:"inbounds,omitempty"`
	Outbounds    []any           `json:"outbounds,omitempty"`
	Route        *sbRoute        `json:"route,omitempty"`
	Experimental *sbExperimental `json:"experimental,omitempty"`
}

// sbExperimental clash API：Dashboard 查询流量/组状态/延迟的数据源。
type sbExperimental struct {
	ClashAPI *sbClashAPI `json:"clash_api,omitempty"`
}

type sbClashAPI struct {
	ExternalController string `json:"external_controller,omitempty"`
	Secret             string `json:"secret,omitempty"`
}

// Generate 将内部模型快照转换为 sing-box 配置 JSON（目标版本 1.14）。
// 生成是纯函数：同一快照多次调用输出字节一致。
func Generate(snap Snapshot) ([]byte, error) {
	if snap.Settings.MixedPort <= 0 || snap.Settings.MixedPort > 65535 {
		return nil, fmt.Errorf("非法的 mixed 端口: %d", snap.Settings.MixedPort)
	}
	logLevel := snap.Settings.LogLevel
	if logLevel == "" {
		logLevel = "info"
	}
	listen := "127.0.0.1"
	if snap.Settings.AllowLAN {
		listen = "0.0.0.0"
	}

	g := &generator{snap: snap}
	g.init()

	outbounds, err := g.buildOutbounds()
	if err != nil {
		return nil, err
	}
	route, err := g.buildRoute()
	if err != nil {
		return nil, err
	}
	// 兜底不变量：route 引用的出站必须真实存在（见 validateOutboundRefs）。
	if err := validateOutboundRefs(outbounds, route); err != nil {
		return nil, err
	}
	// buildDNS 也会标记它引用的规则集，必须先于 buildRuleSets——否则只被 DNS 规则
	// 引用的规则集不会生成 route.rule_set 条目，sing-box 启动时报 rule-set not found。
	dnsCfg, err := g.buildDNS(route)
	if err != nil {
		return nil, err
	}
	if err := g.buildRuleSets(route); err != nil {
		return nil, err
	}

	inbounds := []any{
		sbMixedInbound{Type: "mixed", Tag: "mixed-in", Listen: listen, ListenPort: snap.Settings.MixedPort},
	}

	cfg := sbConfig{
		Log:          &sbLog{Level: logLevel, Timestamp: true},
		DNS:          dnsCfg,
		Inbounds:     inbounds,
		Outbounds:    outbounds,
		Route:        route,
		Experimental: g.buildExperimental(),
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化 sing-box 配置失败: %w", err)
	}
	return append(out, '\n'), nil
}

// buildExperimental 生成 experimental 段：ClashAPIPort > 0 时开启 clash API。
// API 仅监听回环地址，避免 AllowLAN 同时暴露未鉴权的管理接口。
func (g *generator) buildExperimental() *sbExperimental {
	port := g.snap.Settings.ClashAPIPort
	if port <= 0 || port > 65535 {
		return nil
	}
	return &sbExperimental{
		ClashAPI: &sbClashAPI{
			ExternalController: fmt.Sprintf("127.0.0.1:%d", port),
			Secret:             g.snap.Settings.ClashAPISecret,
		},
	}
}

// --- 生成器内部状态 ---

type generator struct {
	snap        Snapshot
	nodeTag     map[int64]string       // 启用节点 ID → 出站 tag
	enabledByID map[int64]*Node        // 启用节点索引
	groupByID   map[int64]*ProxyGroup  // 代理组索引
	usedTags    map[string]bool        // 已占用的出站 tag（组名优先）
	rsByTag     map[string]*RuleSet    // 规则集索引
	needRS      map[string]bool        // 被路由/DNS 规则引用的规则集 tag
	catRefs     map[string]catalog.Ref // 被引用的内置分类：派生 tag → 分类引用
}

func (g *generator) init() {
	g.nodeTag = map[int64]string{}
	g.enabledByID = map[int64]*Node{}
	g.groupByID = map[int64]*ProxyGroup{}
	g.usedTags = map[string]bool{"": true, "direct": true}
	g.rsByTag = map[string]*RuleSet{}
	g.needRS = map[string]bool{}
	g.catRefs = map[string]catalog.Ref{}
	for _, n := range g.snap.Nodes {
		if n.Enabled {
			g.enabledByID[n.ID] = n
		}
	}
	for _, gp := range g.snap.ProxyGroups {
		g.groupByID[gp.ID] = gp
		g.usedTags[gp.Name] = true // 组名优先占用 tag；节点重名时加后缀
	}
	for _, rs := range g.snap.RuleSets {
		g.rsByTag[rs.Tag] = rs
	}
}

// --- 出站 ---

// ErrNoUsableNodes 表示全局没有任何可用代理节点：典型场景是首次安装、尚未添加订阅/节点，
// 或全部节点都被禁用。这是 onboarding 状态而不是「用户配置损坏」，因此必须与
// ErrEmptyProxyGroup 分开——两者的修法完全不同。
//
// 文案直接写在错误上：TUI 的 Ctrl+A / 任务回执与 CLI 各子命令都会原样把它显示给用户，
// 无需调用方按类型分支挑文案。
type ErrNoUsableNodes struct{}

func (ErrNoUsableNodes) Error() string {
	return "当前没有可用代理节点。请先添加订阅或节点（或启用已被禁用的节点），然后再应用配置。"
}

// ErrEmptyProxyGroup 表示全局存在可用节点，但该**顶层**代理组解析后成员为空：
// 成员可能引用了已删除/已禁用的节点或代理组、子组失效，或筛选条件为空。
// Group 是被判定为空的组名，供文案与测试定位。
type ErrEmptyProxyGroup struct {
	Group string
}

func (e ErrEmptyProxyGroup) Error() string {
	return fmt.Sprintf("代理组 %q 没有可用成员：成员可能引用了已删除或已禁用的节点/代理组。请修复该组成员后重新应用配置。", e.Group)
}

func (g *generator) buildOutbounds() ([]any, error) {
	out := []any{}

	// 节点出站：tag 用节点名，与组名或其他节点冲突时加序号后缀
	for _, n := range g.snap.Nodes {
		if !n.Enabled {
			continue
		}
		tag, err := g.assignNodeTag(n)
		if err != nil {
			return nil, err
		}
		ob, err := NodeToOutbound(n, tag)
		if err != nil {
			return nil, err
		}
		out = append(out, ob)
	}

	// 代理组出站。
	//
	// fail-closed（C15）：旧实现让 resolveMembers 在空组时注入 "direct"，于是「尚无节点」
	// 或「组成员全失效」都会生成一份看起来正常、实际全部直连的配置——用户以为代理已生效。
	// 现在改为报错，并区分两类：全局没有可用节点（onboarding）与某个顶层组为空（组配置问题）。
	if len(g.snap.ProxyGroups) > 0 && len(g.enabledByID) == 0 {
		// 前置条件 len(ProxyGroups) > 0：没有任何代理组时（config 包的纯骨架用法，
		// 如 core 集成测试与 golden 骨架）不存在「组为空」这件事，仍应正常生成。
		//
		// 可用节点的判定与组解析保持一致：enabledByID 在 init() 里按 Enabled 装入。
		// 不可生成的节点（协议不支持等）会在上面的节点循环里直接报错，不会走到这里。
		return nil, ErrNoUsableNodes{}
	}
	seen := map[string]bool{}
	for _, gp := range g.snap.ProxyGroups {
		if seen[gp.Name] {
			return nil, fmt.Errorf("代理组重名: %q", gp.Name)
		}
		seen[gp.Name] = true
		members, err := g.resolveMembers(gp, map[int64]bool{})
		if err != nil {
			return nil, err
		}
		if len(members) == 0 {
			// 不复用「跳过不生成」的写法：usedTags 在 init() 里已按组名占位，路由规则
			// 也可能指向该组。少生成一个 outbound 会造出悬空 tag，把错误推迟到
			// sing-box check 阶段。这里直接失败，保证引用集合与实际出站集合一致。
			return nil, ErrEmptyProxyGroup{Group: gp.Name}
		}
		out = append(out, g.groupOutbound(gp, members))
	}

	out = append(out, sbDirectOutbound{Type: "direct", Tag: "direct"})
	return out, nil
}

func (g *generator) assignNodeTag(n *Node) (string, error) {
	if t, ok := g.nodeTag[n.ID]; ok {
		return t, nil
	}
	base := strings.TrimSpace(n.Name)
	if base == "" {
		base = fmt.Sprintf("node-%d", n.ID)
	}
	tag := base
	for i := 2; g.usedTags[tag]; i++ {
		tag = fmt.Sprintf("%s-%d", base, i)
	}
	g.usedTags[tag] = true
	g.nodeTag[n.ID] = tag
	return tag, nil
}

// resolveMembers 解析代理组成员为出站 tag 列表（保序去重）。
// 失效引用（节点禁用/已删除、嵌套组已删除）静默跳过；**递归层允许返回空切片**。
//
// 不要在这里补回 `len(out) == 0 → []string{"direct"}`：那正是 C15 删掉的隐式直连。
// 空组由 buildOutbounds 在顶层判定并报错（ErrNoUsableNodes / ErrEmptyProxyGroup）——
// 父组拿到空子组时只应少一个成员，而不是多出一个 direct。
func (g *generator) resolveMembers(gp *ProxyGroup, visiting map[int64]bool) ([]string, error) {
	if visiting[gp.ID] {
		return nil, fmt.Errorf("代理组 %q 存在循环成员引用", gp.Name)
	}
	visiting[gp.ID] = true
	defer delete(visiting, gp.ID)

	var out []string
	seen := map[string]bool{}
	add := func(tag string) {
		if tag != "" && !seen[tag] {
			seen[tag] = true
			out = append(out, tag)
		}
	}
	for _, m := range gp.Members {
		switch m.Type {
		case "node":
			if n := g.enabledByID[m.ID]; n != nil {
				add(g.nodeTag[n.ID])
			}
		case "group":
			sub := g.groupByID[m.ID]
			if sub == nil {
				continue
			}
			tags, err := g.resolveMembers(sub, visiting)
			if err != nil {
				return nil, err
			}
			for _, t := range tags {
				add(t)
			}
		case "all":
			for _, n := range g.snap.Nodes {
				if n.Enabled {
					add(g.nodeTag[n.ID])
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (g *generator) groupOutbound(gp *ProxyGroup, members []string) map[string]any {
	// 模型类型 → sing-box 出站类型（select → selector）
	typ := gp.Type
	if typ == "select" {
		typ = "selector"
	}
	ob := map[string]any{"type": typ, "tag": gp.Name, "outbounds": members}
	switch gp.Type {
	case "select":
		if def := g.resolveSelected(gp, members); def != "" {
			ob["default"] = def
		}
	case "urltest":
		testURL := gp.TestURL
		if testURL == "" {
			testURL = DefaultTestURL
		}
		ob["url"] = testURL
		if gp.IntervalS > 0 {
			ob["interval"] = fmt.Sprintf("%ds", gp.IntervalS)
		}
	}
	return ob
}

// resolveSelected 把持久化的选中项（"node:<id>"/"group:<id>"）解析为出站 tag；
// 选中项已失效（不在当前成员列表）时返回空，由 sing-box 取第一个成员。
func (g *generator) resolveSelected(gp *ProxyGroup, members []string) string {
	if gp.Selected == "" {
		return ""
	}
	var tag string
	if rest, ok := strings.CutPrefix(gp.Selected, "node:"); ok {
		if id, err := strconv.ParseInt(rest, 10, 64); err == nil {
			if n := g.enabledByID[id]; n != nil {
				tag = g.nodeTag[n.ID]
			}
		}
	} else if rest, ok := strings.CutPrefix(gp.Selected, "group:"); ok {
		if id, err := strconv.ParseInt(rest, 10, 64); err == nil {
			if sub := g.groupByID[id]; sub != nil {
				tag = sub.Name
			}
		}
	} else {
		return ""
	}
	for _, m := range members {
		if m == tag {
			return tag
		}
	}
	return ""
}

// --- 路由 ---

// validateOutboundRefs 校验 route 里所有动作型引用（final 与各规则的 outbound）都指向
// 真实生成的出站 tag。生成器自己就用同一份模型算出这些引用，正常路径不可能失败；
// 它的价值是**挡住未来的回归**：一旦有人为了「跳过空组」之类的理由少生成某个出站，
// 引用会立刻在这里暴露，而不是拖延到 sing-box check / 运行期才报 unknown outbound。
func validateOutboundRefs(out []any, route *sbRoute) error {
	tags := make(map[string]bool, len(out))
	for _, ob := range out {
		switch v := ob.(type) {
		case sbDirectOutbound:
			tags[v.Tag] = true
		case map[string]any:
			if tag, ok := v["tag"].(string); ok {
				tags[tag] = true
			}
		}
	}
	check := func(tag, where string) error {
		if tag == "" || tags[tag] {
			return nil
		}
		return fmt.Errorf("%s引用了不存在的出站 %q", where, tag)
	}
	if err := check(route.Final, "route.final "); err != nil {
		return err
	}
	for i := range route.Rules {
		if err := check(route.Rules[i].Outbound, fmt.Sprintf("第 %d 条路由规则", i+1)); err != nil {
			return err
		}
	}
	return nil
}

func (g *generator) buildRoute() (*sbRoute, error) {
	route := &sbRoute{AutoDetectInterface: true}

	if g.snap.DNS != nil && len(g.snap.DNS.Servers) > 0 {
		// 系统 DNS 查询统一交给内置 DNS 模块（FakeIP 与 DNS 分流的前提）
		route.Rules = append(route.Rules, sbRouteRule{Port: []int{53}, Action: "hijack-dns"})
	}
	// 嗅探还原连接域名：IP 直连与 FakeIP 场景下域名分流的前提
	//（入站 sniff 字段自 sing-box 1.13 起被拒绝，必须用规则动作）
	route.Rules = append(route.Rules, sbRouteRule{Action: "sniff"})

	// 内网直连：目标为私有地址（10/8、192.168/16、127/8、链路本地等）时直连，置于用户规则之前。
	// 客户端直接以 IP 发起的连接天然命中；域名形式的内网地址（如 nas.local）需先 resolve
	// 才能判定，若有此需求可另建 domain_suffix 规则指向 DIRECT。
	if g.snap.Settings.PrivateDirect {
		route.Rules = append(route.Rules, sbRouteRule{IPIsPrivate: true, Outbound: "direct"})
	}

	// 用户规则先收集到独立切片：resolve 规则要插到第一条 IP 类规则之前，
	// 位置依赖全部规则的相对顺序。
	var userRules []sbRouteRule
	firstIPRule := -1 // 第一条含 IP 类条件的用户规则下标；-1 表示没有
	finalOutbound, finalAction := "", ""
	finalSeen := false
	for _, rg := range g.snap.RoutingGroups {
		if !rg.Enabled {
			continue
		}
		for _, r := range rg.Rules {
			if !r.Enabled {
				continue
			}
			if r.Type == "final" {
				if finalSeen {
					return nil, fmt.Errorf("活动 final 分流组超过一个（重复项: %q）", rg.Name)
				}
				finalSeen = true
				var err error
				finalOutbound, finalAction, err = g.mapTarget(rg.Target)
				if err != nil {
					return nil, fmt.Errorf("分流组 %q: %w", rg.Name, err)
				}
				continue
			}
			rule, err := g.buildRouteRule(rg, &r)
			if err != nil {
				return nil, err
			}
			if firstIPRule < 0 && ruleNeedsResolve(&r) {
				firstIPRule = len(userRules)
			}
			userRules = append(userRules, *rule)
		}
	}

	// resolve：IP 类条件只能匹配已知 IP——目标是域名且未解析时，sing-box 直接判定不匹配
	// （route/rule/rule_item_cidr.go）。插入 resolve 动作把目标域名解析出 IP，IP 类条件方能生效。
	// 位置取第一条 IP 类规则之前：其前的域名类规则先行终结匹配，尽量减少被 resolve 影响的连接。
	// 代价：resolve 之后未终结的连接会带着解析结果拨号（route/conn.go），走代理时代理侧收到 IP
	// 而非域名，失去代理侧解析——故此项默认关闭，由用户显式开启。
	if g.snap.Settings.ResolveIPRules && firstIPRule >= 0 {
		resolve := sbRouteRule{Action: "resolve", Server: g.resolveDNSServer()}
		userRules = slices.Insert(userRules, firstIPRule, resolve)
	}
	route.Rules = append(route.Rules, userRules...)

	switch {
	case finalAction != "": // final 指向 BLOCK：兜底拒绝规则
		route.Rules = append(route.Rules, sbRouteRule{Action: finalAction})
	case finalOutbound != "":
		route.Final = finalOutbound
	default:
		route.Final = "direct" // 无 final 规则时未匹配流量直连，避免落到第一个出站
	}
	return route, nil
}

// ruleNeedsResolve 报告规则是否含 IP 类条件——这类条件对域名目标必须先 resolve 才能匹配。
func ruleNeedsResolve(r *Rule) bool {
	if r.Type == "logical" {
		for i := range r.Conditions {
			if condNeedsResolve(&r.Conditions[i]) {
				return true
			}
		}
		return false
	}
	return condNeedsResolve(&RuleCondition{Type: r.Type, Value: r.Value})
}

// condNeedsResolve 报告单个条件是否为 IP 类。
// rule_set 条件：内置分类按实测判定（geoip 全部是、geosite 全部不是、acl 混合，
// 见 catalog.Ref.NeedsResolve）；自定义规则集内部是否含 ip_cidr 无法静态识别，
// 退回按 tag 含 "geoip" 的命名约定判断，此类规则集需用户自行开启 resolve
// 或改用显式 ip_cidr 条件。
func condNeedsResolve(c *RuleCondition) bool {
	switch c.Type {
	case "ip_cidr", "geoip":
		return true
	case "rule_set":
		for _, tag := range splitValues(c.Value) {
			if ref, ok := catalog.Parse(tag); ok {
				if ref.NeedsResolve() {
					return true
				}
				continue
			}
			if strings.Contains(tag, "geoip") {
				return true
			}
		}
	}
	return false
}

// resolveDNSServer 返回 resolve 动作要显式指定的 DNS 服务器 tag，不需要指定时返回空。
// 仅 FakeIP 开启时需要：未指定 server 的 resolve 会走 DNS 规则匹配（见 sing-box 1.14 迁移说明），
// 命中生成器追加的 FakeIP 规则后拿回的仍是 FakeIP 段地址，IP 类条件照样匹配不上。
// 指定 server 会绕过 DNS 规则，代价是国内域名不再经 DNS 分流解析，故只在必要时使用。
func (g *generator) resolveDNSServer() string {
	cfg := g.snap.DNS
	if cfg == nil || !cfg.FakeIPEnabled || len(cfg.Servers) == 0 {
		return ""
	}
	for i := range cfg.Servers {
		if cfg.Servers[i].Tag == cfg.Final {
			return cfg.Servers[i].Tag
		}
	}
	return cfg.Servers[0].Tag
}

func (g *generator) buildRouteRule(rg *RoutingGroup, r *Rule) (*sbRouteRule, error) {
	rule := &sbRouteRule{}
	if r.Type == "logical" {
		if r.Mode != "and" && r.Mode != "or" {
			return nil, fmt.Errorf("分流组 %q 的逻辑规则组合方式非法: %q", rg.Name, r.Mode)
		}
		if len(r.Conditions) == 0 {
			return nil, fmt.Errorf("分流组 %q 的逻辑规则缺少子条件", rg.Name)
		}
		rule.Type = "logical"
		rule.Mode = r.Mode
		for i := range r.Conditions {
			sub, err := g.buildCondition(rg, &r.Conditions[i], true)
			if err != nil {
				return nil, err
			}
			rule.Rules = append(rule.Rules, *sub)
		}
	} else {
		cond := RuleCondition{Type: r.Type, Value: r.Value, Invert: r.Invert}
		sub, err := g.buildCondition(rg, &cond, false)
		if err != nil {
			return nil, err
		}
		*rule = *sub
	}
	if r.Invert {
		rule.Invert = true
	}
	outbound, action, err := g.mapTarget(rg.Target)
	if err != nil {
		return nil, fmt.Errorf("分流组 %q: %w", rg.Name, err)
	}
	rule.Action = action
	rule.Outbound = outbound
	return rule, nil
}

// buildCondition 构建单个匹配条件（普通规则或逻辑规则的子条件）。
// isSub 为 true 时按逻辑子条件报错定位；rule_set 条件校验规则集存在且启用。
func (g *generator) buildCondition(rg *RoutingGroup, c *RuleCondition, isSub bool) (*sbRouteRule, error) {
	rule := &sbRouteRule{}
	values := splitValues(c.Value)
	what := "规则"
	if isSub {
		what = "逻辑条件"
	}
	switch c.Type {
	case "domain":
		rule.Domain = values
	case "domain_suffix":
		rule.DomainSuffix = values
	case "domain_keyword":
		rule.DomainKeyword = values
	case "domain_regex":
		// 正则本身可能含逗号（如 a{1,3}），故不做多值拆分：整个值就是一条正则。
		// 需要多个分支时用正则的 | 表达（如 ^(a|b)\.com$）。
		if v := strings.TrimSpace(c.Value); v != "" {
			rule.DomainRegex = []string{v}
		}
	case "ip_cidr":
		rule.IPCIDR = values
	case "geoip":
		rule.GeoIP = values
	case "geosite":
		rule.Geosite = values
	case "rule_set":
		tags, err := g.resolveRuleSetRefs(values, fmt.Sprintf("分流组 %q 的%s", rg.Name, what))
		if err != nil {
			return nil, err
		}
		rule.RuleSet = tags
	default:
		return nil, fmt.Errorf("分流组 %q 含未知%s类型 %q", rg.Name, what, c.Type)
	}
	if c.Invert {
		rule.Invert = true
	}
	return rule, nil
}

// mapTarget 把分流目标解析为出站 tag 或规则动作（DIRECT/BLOCK/代理组/节点）。
func (g *generator) mapTarget(target string) (outbound, action string, err error) {
	switch {
	case strings.EqualFold(target, "DIRECT"):
		return "direct", "", nil
	case strings.EqualFold(target, "BLOCK"):
		return "", "reject", nil // 1.14 已移除 block 出站，用 reject 动作表达
	}
	for _, gp := range g.snap.ProxyGroups {
		if gp.Name == target {
			return target, "", nil
		}
	}
	for _, n := range g.snap.Nodes {
		if n.Enabled && g.nodeTag[n.ID] == target {
			return target, "", nil
		}
	}
	return "", "", fmt.Errorf("分流目标 %q 不是代理组、节点、DIRECT 或 BLOCK", target)
}

// --- 规则集 ---

// resolveRuleSetRefs 把 rule_set 条件的值解析为 sing-box rule_set tag 列表。
// 每个值先按 rulesets 表里的自定义规则集 tag 查找，查不到再按内置分类引用
// （"geosite:cn" 或派生 tag "geosite-cn"）解析。自定义规则集优先，保证用户
// 自建的同名 tag 不会被分类库遮蔽。where 用于错误定位。
func (g *generator) resolveRuleSetRefs(values []string, where string) ([]string, error) {
	tags := make([]string, 0, len(values))
	for _, v := range values {
		if rs := g.rsByTag[v]; rs != nil {
			if !rs.Enabled {
				return nil, fmt.Errorf("%s引用了未启用的规则集 %q", where, v)
			}
			g.needRS[v] = true
			tags = append(tags, v)
			continue
		}
		ref, ok := catalog.Parse(v)
		if !ok {
			return nil, fmt.Errorf("%s引用了不存在或未启用的规则集 %q", where, v)
		}
		tag := ref.Tag()
		// 派生 tag 命中 rulesets 表（老库遗留的内置条目，或用户自建的同名规则集）：
		// 优先用表里的记录——它携带用户可能改过的 URL 与实际缓存路径。
		// 不这样做的话，两条生成路径会互相跳过，rule_set 条目整个丢失。
		if rs := g.rsByTag[tag]; rs != nil {
			if !rs.Enabled {
				return nil, fmt.Errorf("%s引用了未启用的规则集 %q", where, tag)
			}
			g.needRS[tag] = true
			tags = append(tags, tag)
			continue
		}
		g.catRefs[tag] = ref
		tags = append(tags, tag)
	}
	return tags, nil
}

// buildRuleSets 生成 route.rule_set 条目：rulesets 表中被引用且启用的自定义规则集，
// 加上被规则引用到的内置分类（按需生成，而非把整个分类库都写进配置）。
func (g *generator) buildRuleSets(route *sbRoute) error {
	for _, rs := range g.snap.RuleSets {
		if !rs.Enabled || !g.needRS[rs.Tag] {
			continue
		}
		item := sbRuleSet{Tag: rs.Tag, Format: sbRuleSetFormat(rs.Format)}
		switch {
		case rs.SourceType == "local":
			if rs.CachedPath == "" {
				return fmt.Errorf("本地规则集 %q 未配置文件路径", rs.Name)
			}
			item.Type = "local"
			item.Path = rs.CachedPath
		case rs.CachedPath != "" && fileExists(rs.CachedPath):
			item.Type = "local"
			item.Path = rs.CachedPath
		case rs.URL != "":
			item.Type = "remote"
			item.URL = rs.URL
		default:
			return fmt.Errorf("规则集 %q 既无本地缓存也无下载 URL", rs.Name)
		}
		route.RuleSet = append(route.RuleSet, item)
	}

	// 内置分类：有缓存走 local，缺缓存回退 remote 由 sing-box 启动时下载。
	// 按 tag 排序，保证同一状态多次生成结果字节一致（幂等）。
	catTags := make([]string, 0, len(g.catRefs))
	for tag := range g.catRefs {
		// resolveRuleSetRefs 已保证派生 tag 不与 rulesets 表重合；此处兜底，
		// 重复 tag 会让 sing-box 直接报 duplicate rule-set tag。
		if g.rsByTag[tag] != nil {
			continue
		}
		catTags = append(catTags, tag)
	}
	slices.Sort(catTags)
	for _, tag := range catTags {
		ref := g.catRefs[tag]
		item := sbRuleSet{Tag: tag, Format: "binary"} // 分类库一律 .srs
		if path := g.catalogCachePath(tag); path != "" && fileExists(path) {
			item.Type = "local"
			item.Path = path
		} else {
			item.Type = "remote"
			item.URL = ref.URL()
		}
		route.RuleSet = append(route.RuleSet, item)
	}
	return nil
}

// catalogCachePath 返回内置分类的本地缓存路径；未配置缓存目录时返回空。
func (g *generator) catalogCachePath(tag string) string {
	if g.snap.RuleSetCacheDir == "" {
		return ""
	}
	return filepath.Join(g.snap.RuleSetCacheDir, tag+".srs")
}

// sbRuleSetFormat 内部格式名 → sing-box 格式名。
func sbRuleSetFormat(f string) string {
	switch f {
	case "srs":
		return "binary"
	case "json":
		return "source"
	}
	return f
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// --- DNS ---

func (g *generator) buildDNS(route *sbRoute) (*sbDNS, error) {
	cfg := g.snap.DNS
	if cfg == nil || len(cfg.Servers) == 0 {
		return nil, nil
	}
	dns := &sbDNS{Strategy: cfg.Strategy}

	tags := map[string]bool{}
	for _, s := range cfg.Servers {
		if s.Type != "local" && strings.TrimSpace(s.Address) == "" {
			return nil, fmt.Errorf("DNS 服务器 %q 缺少地址", s.Tag)
		}
		if tags[s.Tag] {
			return nil, fmt.Errorf("DNS 服务器 Tag 重复: %q", s.Tag)
		}
		tags[s.Tag] = true
		dns.Servers = append(dns.Servers, dnsServerOutbound(&s))
	}
	for _, s := range cfg.Servers {
		if s.AddressResolver != "" && !tags[s.AddressResolver] {
			return nil, fmt.Errorf("DNS 服务器 %q 的 AddressResolver %q 不是已启用的 DNS 服务器", s.Tag, s.AddressResolver)
		}
		// 用归一后的 detour 做引用校验：直连写法（direct/DIRECT）等价于不写 detour，
		// 而 usedTags 里只有小写 "direct"。不归一的话 "DIRECT" 会被误判成
		// 「不是已生成的出站」，让整份配置生成失败。
		if detour := normalizeDNSDetour(s.Detour); detour != "" && !g.usedTags[detour] {
			return nil, fmt.Errorf("DNS 服务器 %q 的 Detour %q 不是已生成的出站", s.Tag, s.Detour)
		}
	}
	// final：配置的 final 失效时回退第一个服务器
	dns.Final = cfg.Final
	if !tags[dns.Final] {
		dns.Final = cfg.Servers[0].Tag
	}

	for _, r := range cfg.Rules {
		if !tags[r.Server] {
			return nil, fmt.Errorf("DNS 规则指向不存在的服务器 %q", r.Server)
		}
		rule := sbDNSRule{Server: r.Server}
		values := splitValues(r.Value)
		switch r.Type {
		case "domain":
			rule.Domain = values
		case "domain_suffix":
			rule.DomainSuffix = values
		case "domain_keyword":
			rule.DomainKeyword = values
		case "rule_set":
			tags, err := g.resolveRuleSetRefs(values, "DNS 规则")
			if err != nil {
				return nil, err
			}
			rule.RuleSet = tags
		default:
			return nil, fmt.Errorf("未知 DNS 规则类型 %q", r.Type)
		}
		dns.Rules = append(dns.Rules, rule)
	}

	// FakeIP：1.12+ 以 fakeip 类型 DNS 服务器实现；query_type 规则置于用户规则之后，
	// 命中 DNS 规则的域名（如国内域名）仍解析真实 IP
	if cfg.FakeIPEnabled {
		dns.Servers = append(dns.Servers, map[string]any{
			"type":        "fakeip",
			"tag":         "fakeip",
			"inet4_range": orDefaultStr(cfg.FakeIPRange, defaultFakeIPRange),
			"inet6_range": "fc00::/18",
		})
		dns.Rules = append(dns.Rules, sbDNSRule{QueryType: []string{"A", "AAAA"}, Server: "fakeip"})
	}

	// 出站/DNS 服务器地址为域名时的解析来源：优先本机或纯 IP 服务器，避免自引用循环
	if resolver := pickDNSResolver(cfg.Servers); resolver != "" {
		route.DefaultDomainResolver = resolver
	}
	return dns, nil
}

// dnsServerOutbound 把 DNS 服务器模型转为 1.12+ 新格式 server 对象。
func dnsServerOutbound(s *DNSServer) map[string]any {
	srv := map[string]any{"type": s.Type, "tag": s.Tag}
	switch s.Type {
	case "local":
		// 本机解析，无地址参数
	case "udp", "tcp":
		host, port := splitHostPort(s.Address)
		srv["server"] = host
		if port > 0 {
			srv["server_port"] = port
		}
	case "tls", "https", "quic", "h3":
		host, port, path := parseTLSDNSAddress(s.Address, s.Type)
		srv["server"] = host
		if port > 0 {
			srv["server_port"] = port
		}
		if path != "" {
			srv["path"] = path
		}
		srv["tls"] = map[string]any{"enabled": true, "server_name": host}
	}
	if s.AddressResolver != "" {
		srv["domain_resolver"] = s.AddressResolver
	}
	if detour := normalizeDNSDetour(s.Detour); detour != "" {
		srv["detour"] = detour
	}
	return srv
}

// normalizeDNSDetour 把 DNS 服务器 detour 的「直连」等价写法归一为「不写 detour」。
//
// sing-box 的语义是：不写 detour 就已经使用直连 dialer；而显式指向内置的空 direct
// 出站会被拒绝启动——实测 FATAL: start dns/udp[x]: detour to an empty direct outbound
// makes no sense（不是 check 阶段，是 run 阶段，所以只有真正启动才会暴露）。
//
// 本仓库生成的 direct 出站恒为无设置的 {"type":"direct","tag":"direct"}，且 "direct"
// 由 generator.init() 占位、节点与代理组都不可能占用该 tag，因此 ""/"direct"/"DIRECT"
// 表达的是同一件事，统一成缺省形式既不改语义，也不会让一个等价写法把内核挡在启动之外。
//
// 只归一这一种写法：其他值（代理组 tag）一律原样保留，改它们就是偷改运行语义。
func normalizeDNSDetour(detour string) string {
	if strings.EqualFold(strings.TrimSpace(detour), "direct") {
		return ""
	}
	return detour
}

// splitHostPort 拆分 "host:port"；无端口时端口返回 0。裸 IPv6（多个冒号）整体视为主机。
func splitHostPort(addr string) (string, int) {
	if strings.Contains(addr, "://") {
		if u, err := url.Parse(addr); err == nil && u.Host != "" {
			host := u.Hostname()
			if port, err := strconv.Atoi(u.Port()); err == nil && port > 0 {
				return host, port
			}
			return host, 0
		}
	}
	if host, portStr, err := net.SplitHostPort(addr); err == nil {
		if port, err := strconv.Atoi(portStr); err == nil && port > 0 {
			return host, port
		}
		return host, 0
	}
	return addr, 0
}

// parseTLSDNSAddress 解析 TLS 系 DNS 服务器地址：支持 URL 形式（https://host:port/path）
// 与 host[:port] 形式。path 仅 https/h3 有效。
func parseTLSDNSAddress(addr, typ string) (host string, port int, path string) {
	if strings.Contains(addr, "://") {
		if u, err := url.Parse(addr); err == nil && u.Host != "" {
			host = u.Hostname()
			if p, err := strconv.Atoi(u.Port()); err == nil {
				port = p
			}
			if (typ == "https" || typ == "h3") && u.Path != "" && u.Path != "/" {
				path = u.Path
			}
			return host, port, path
		}
	}
	host, port = splitHostPort(addr)
	return host, port, ""
}

// pickDNSResolver 选出用于解析域名的 DNS 服务器 tag：优先 local，其次纯 IP 的 udp/tcp，
// 避免选中地址本身是域名（需先被解析）的服务器造成循环。
//
// 候选排序复用 dnsCandidateOrder（probe.go）：独立测速的 bootstrap 选择用的是
// 同一套优先级，只是还要额外过 probe-safe 检查。两处各写一份排序，迟早会分叉。
func pickDNSResolver(servers []DNSServer) string {
	order := dnsCandidateOrder(servers)
	if len(order) == 0 {
		return ""
	}
	return servers[order[0]].Tag
}

// splitValues 拆分逗号分隔的多值并去除空白项。
func splitValues(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
