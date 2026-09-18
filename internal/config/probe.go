package config

import (
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
)

// 独立节点测速（latency probe）配置生成。
//
// 背景：TUI 有两条测速路径——界面的代理组测速走正在运行的主核心
// （clashapi.GroupDelay），独立节点测速则起一个一次性 sing-box 再调
// /proxies/{tag}/delay。后者曾经在 proxy 包内自行拼装「只有 outbounds + clash_api」
// 的配置，于是节点服务器地址是域名时，两条路径落在两套互不相关的域名解析环境里：
// 运行核心用用户保存的 DNS 与 route.default_domain_resolver，临时核心只能靠
// 临时进程碰巧能用的系统解析器。结果是同一节点在一处连通、在另一处批量失败，
// 用户会误判为「节点坏了」。
//
// 这里把测速配置的生成收回 config 包，与 Generate 共用 DNS 语义（同一个
// dnsServerOutbound、同一套「先 local 再纯 IP」的 bootstrap 选择原则），
// 但**不复制**用户完整的 DNS 分流：测速只需要「节点服务器域名 → IP」这一件事，
// 规则集、FakeIP、业务分流统统不需要，带上反而会把 rule_set 依赖拖进临时配置。
//
// 与主配置的三处刻意差异（都不改变代理运行语义）：
//   - 没有 mixed 入站，不占用用户端口；
//   - DNS 只含自举解析链，final 指向该链而非用户 final；
//   - 不含代理组出站（测速对象就是单个节点，组会引入自引用）。

// probeTagPrefix 测速出站 tag 前缀。
//
// 刻意不用节点名：节点可能重名、含中文或特殊字符，甚至叫 "direct"——那正是
// 临时配置里直连出站的保留 tag。用 ID 计算出的 tag 不受这些影响。
const probeTagPrefix = "probe-node-"

// ProbeTag 返回节点 ID 对应的测速出站 tag。
func ProbeTag(id int64) string {
	return fmt.Sprintf("%s%d", probeTagPrefix, id)
}

// LatencyProbe 独立节点测速配置的输入。
type LatencyProbe struct {
	Nodes   []*Node
	DNS     *DNSConfig // 已保存的 DNS 配置；nil 或没有服务器时不生成 dns 段
	APIPort int        // clash API 监听端口（仅回环）
}

// LatencyProbeTarget 一个可测速节点：ID → 临时配置中的出站 tag。
type LatencyProbeTarget struct {
	ID   int64
	Name string
	Tag  string
}

// LatencyProbeSkip 无法生成出站的节点（凭据缺失、协议不支持等）。
type LatencyProbeSkip struct {
	ID   int64
	Name string
	Err  error
}

// LatencyProbePlan 生成结果：配置字节与逐节点映射。
type LatencyProbePlan struct {
	Data    []byte
	Targets []LatencyProbeTarget
	Skipped []LatencyProbeSkip
}

// GenerateLatencyProbe 生成一次性 sing-box 测速配置。
//
// 节点转换仍走 NodeToOutbound——探针不得自行维护协议转换逻辑，否则协议层的
// 改动会在测速路径上静默失效，又变成两条路径不一致。
//
// 节点为 0 个可测速时不报错：调用方需要先把 Skipped 逐个回报给用户，
// 再决定整体失败（见 proxy.testNodes）。
func GenerateLatencyProbe(p LatencyProbe) (*LatencyProbePlan, error) {
	if p.APIPort <= 0 || p.APIPort > 65535 {
		return nil, fmt.Errorf("非法的测速 API 端口: %d", p.APIPort)
	}
	plan := &LatencyProbePlan{}
	outbounds := []any{}
	for _, n := range p.Nodes {
		tag := ProbeTag(n.ID)
		ob, err := NodeToOutbound(n, tag)
		if err != nil {
			plan.Skipped = append(plan.Skipped, LatencyProbeSkip{ID: n.ID, Name: n.Name, Err: err})
			continue
		}
		outbounds = append(outbounds, ob)
		plan.Targets = append(plan.Targets, LatencyProbeTarget{ID: n.ID, Name: n.Name, Tag: tag})
	}

	dnsCfg, resolver, err := buildProbeDNS(p.DNS)
	if err != nil {
		return nil, err
	}

	route := &sbRoute{
		Final:               "direct",
		AutoDetectInterface: true, // 与主配置一致：让直连出站自己选网卡
	}
	if resolver != "" {
		route.DefaultDomainResolver = resolver
	}

	cfg := sbConfig{
		Log:          &sbLog{Level: "warn"},
		DNS:          dnsCfg,
		Outbounds:    append(outbounds, sbDirectOutbound{Type: "direct", Tag: "direct"}),
		Route:        route,
		Experimental: &sbExperimental{ClashAPI: &sbClashAPI{ExternalController: fmt.Sprintf("127.0.0.1:%d", p.APIPort)}},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化测速配置失败: %w", err)
	}
	plan.Data = append(data, '\n')
	return plan, nil
}

// buildProbeDNS 为独立测速挑一个能自举的 DNS 服务器，返回 dns 段与
// route.default_domain_resolver 的 tag；没有可用服务器时返回 (nil, "", nil)。
//
// 没有 DNS 服务器时返回空而不是报错：此时主核心也不生成 dns 段、同样走系统解析器，
// 两边语义一致，属于用户配置现状而不是探针的缺陷。
//
// 候选逐个评估、失败就换下一个，不做「全局预检」：配置里存在与自举无关的问题
// （某条用不到的服务器写错地址、或两条代理型 DNS 互相引用成环）时，本探针依然
// 应该能测速，否则又会造出「界面 1 正常、界面 3 特有失败」的分叉。
func buildProbeDNS(cfg *DNSConfig) (*sbDNS, string, error) {
	if cfg == nil || len(cfg.Servers) == 0 {
		return nil, "", nil
	}
	byTag := make(map[string]*DNSServer, len(cfg.Servers))
	for i := range cfg.Servers {
		byTag[cfg.Servers[i].Tag] = &cfg.Servers[i]
	}

	var rejected []string
	for _, idx := range dnsCandidateOrder(cfg.Servers) {
		s := &cfg.Servers[idx]
		chain, reason := probeResolverChain(s, byTag)
		if reason != "" {
			rejected = append(rejected, fmt.Sprintf("%s（%s）", s.Tag, reason))
			continue
		}
		dns := &sbDNS{Strategy: cfg.Strategy, Final: s.Tag}
		for _, c := range chain {
			dns.Servers = append(dns.Servers, probeDNSServer(c))
		}
		return dns, s.Tag, nil
	}

	// 所有候选都不可用。绝不静默回退系统解析器——那会让「DNS 切换」在测速上失去
	// 判决意义：想知道某个 DNS 能不能解析某节点域名，恰恰要靠这条路径。
	// 逐条给出原因，否则用户只能在「测速全失败」和「配置哪里错」之间猜。
	return nil, "", fmt.Errorf("当前 DNS 配置没有可用于独立节点测速的 bootstrap DNS：%s。"+
		"独立测速核心不包含代理组，无法使用 detour 指向代理组的 DNS；"+
		"请配置 local，或地址为字面 IP 的 udp/tcp DNS 后重试。", strings.Join(rejected, "；"))
}

// probeDNSServer 输出探针用的 DNS server 条目。
//
// 与主配置的差别（V8-1 之后只剩「配置面」的差别，语义判据已统一走
// DNSServerNeedsDomainResolver）：地址不是域名时去掉 domain_resolver。该字段只用于
// 解析「服务器地址本身是域名」的情况，但 sing-box 启动时仍会校验它引用的 tag 是否存在
// （实测：字面 IP + 悬空 domain_resolver → FATAL dependency[ghost] not found for
// server[x]），留着等于为一个用不到的依赖把临时核心拦在启动之外。
func probeDNSServer(s *DNSServer) map[string]any {
	srv := *s
	if !DNSServerNeedsDomainResolver(&srv) {
		srv.AddressResolver = ""
	}
	return dnsServerOutbound(&srv)
}

// probeResolverChain 收集以 s 为默认解析器时、临时配置里必须一并存在的 DNS 服务器。
//
// 返回顺序为「依赖先、使用者后」：AddressResolver 链上的服务器先进入配置，
// 引用它的条目紧随其后。第二个返回值非空表示 s 不可用，内容是面向用户的原因。
//
// 环检测只覆盖**这条候选链**：链外存在环不影响本探针能否自举，不该让节点测速整体失败。
func probeResolverChain(s *DNSServer, byTag map[string]*DNSServer) ([]*DNSServer, string) {
	var out []*DNSServer
	added := map[string]bool{}
	var path []string
	var walk func(*DNSServer) string
	walk = func(cur *DNSServer) string {
		if slices.Contains(path, cur.Tag) {
			return fmt.Sprintf("AddressResolver 成环: %s -> %s", strings.Join(path, " -> "), cur.Tag)
		}
		if !probeDetourUsable(cur.Detour) {
			return fmt.Sprintf("detour %q 在测速配置里没有对应出站", cur.Detour)
		}
		if cur.Type != "local" && dnsServerHost(cur) == "" {
			return "缺少服务器地址"
		}
		if added[cur.Tag] {
			return ""
		}
		if DNSServerNeedsDomainResolver(cur) {
			if cur.AddressResolver == "" {
				// 唯一的解析来源就是 default_domain_resolver，也就是它自己。
				return "服务器地址是域名但没有 AddressResolver，只能靠 default_domain_resolver 解析自身"
			}
			next, ok := byTag[cur.AddressResolver]
			if !ok {
				return fmt.Sprintf("AddressResolver %q 不是已启用的 DNS 服务器", cur.AddressResolver)
			}
			path = append(path, cur.Tag)
			reason := walk(next)
			path = path[:len(path)-1]
			if reason != "" {
				return reason
			}
		}
		added[cur.Tag] = true
		out = append(out, cur)
		return ""
	}
	if reason := walk(s); reason != "" {
		return nil, reason
	}
	return out, ""
}

// probeDetourUsable 判断 detour 能否在测速配置里落地：临时配置只有节点出站与 direct，
// 指向代理组的 detour（如 Auto）会因为出站不存在而让 sing-box 启动失败
// （实测 FATAL: outbound detour not found: Auto）。
//
// 「直连」的等价写法（direct/DIRECT）在 dnsServerOutbound 里已归一为缺省——显式写
// detour 指向内置的空 direct 出站会被内核拒绝（实测 FATAL: detour to an empty direct
// outbound makes no sense），所以这里只需判断归一之后是否还有值。
//
// 归一之外不做任何改写：把用户的代理型 detour 清空等于偷改运行时 DNS 走向，
// 测速结论就与真实运行无关了。选不中时换下一个候选，全都不行就在上层报错。
func probeDetourUsable(detour string) bool {
	return normalizeDNSDetour(detour) == ""
}

// probeAddressIsLiteralIP 判断服务器地址是否为字面 IP（自举无需任何解析）。
func probeAddressIsLiteralIP(s *DNSServer) bool {
	host := dnsServerHost(s)
	return host != "" && net.ParseIP(host) != nil
}

// dnsCandidateOrder 按「最可能自举」的顺序排列服务器下标：
// local（本机解析）→ 地址为字面 IP 的 udp/tcp → 其余。
// 与主生成器 pickDNSResolver 的优先级一致，只是探针还要额外过 probe-safe 检查。
func dnsCandidateOrder(servers []DNSServer) []int {
	var local, literalIP, rest []int
	for i := range servers {
		switch s := &servers[i]; {
		case s.Type == "local":
			local = append(local, i)
		case (s.Type == "udp" || s.Type == "tcp") && probeAddressIsLiteralIP(s):
			literalIP = append(literalIP, i)
		default:
			rest = append(rest, i)
		}
	}
	return append(append(local, literalIP...), rest...)
}
