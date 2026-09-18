package config

import (
	"encoding/json"
	"fmt"
	"net"
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
func buildProbeDNS(cfg *DNSConfig) (*sbDNS, string, error) {
	if cfg == nil || len(cfg.Servers) == 0 {
		return nil, "", nil
	}
	if err := validateProbeDNSShape(cfg.Servers); err != nil {
		return nil, "", err
	}
	if tag, ok := findDNSResolverCycle(cfg.Servers); ok {
		return nil, "", fmt.Errorf("DNS 服务器 AddressResolver 存在环: %s；环形引用会让地址解析自身打转，请先修复", tag)
	}

	byTag := make(map[string]*DNSServer, len(cfg.Servers))
	for i := range cfg.Servers {
		byTag[cfg.Servers[i].Tag] = &cfg.Servers[i]
	}

	for _, idx := range dnsCandidateOrder(cfg.Servers) {
		chain, ok := probeResolverChain(&cfg.Servers[idx], byTag)
		if !ok {
			continue
		}
		dns := &sbDNS{Strategy: cfg.Strategy, Final: cfg.Servers[idx].Tag}
		for _, s := range chain {
			dns.Servers = append(dns.Servers, dnsServerOutbound(s))
		}
		return dns, cfg.Servers[idx].Tag, nil
	}

	// 走到这里说明有 DNS 服务器，但没有一个能被独立测速核心使用（典型情况：
	// 服务器地址本身是域名、且唯一的解析来源是走代理组的 DNS）。
	// 此时绝不能静默回退系统解析器——那会让「DNS 切换」在测速上失去判决意义：
	// 想知道某个 DNS 能不能解析某节点域名，恰恰要靠这条路径。
	return nil, "", fmt.Errorf("当前 DNS 配置没有可用于独立节点测速的 bootstrap DNS。" +
		"独立测速核心不包含代理组，无法使用 detour 指向代理组的 DNS，" +
		"也无法用它解析自身地址。请配置 local 或可直连（地址为 IP、detour 为 direct）的 DNS，" +
		"或改用运行中代理组测速。")
}

// probeResolverChain 收集以 s 为默认解析器时、临时配置里必须一并存在的 DNS 服务器。
//
// 返回顺序为「依赖先、使用者后」：AddressResolver 链上的服务器先进入配置，
// 引用它的条目紧随其后。第二个返回值为 false 表示 s 不能被独立测速核心使用。
func probeResolverChain(s *DNSServer, byTag map[string]*DNSServer) ([]*DNSServer, bool) {
	var out []*DNSServer
	seen := map[string]bool{}
	var walk func(*DNSServer) bool
	walk = func(cur *DNSServer) bool {
		if !probeDetourUsable(cur.Detour) {
			return false
		}
		// 地址是域名又没有 AddressResolver 时，唯一的解析来源就是
		// default_domain_resolver，也就是它自己——自引用，必须换别的候选。
		if probeServerHost(cur) != "" && !probeAddressIsLiteralIP(cur) && cur.AddressResolver == "" {
			return false
		}
		if seen[cur.Tag] {
			return true
		}
		seen[cur.Tag] = true
		if cur.AddressResolver != "" {
			next, ok := byTag[cur.AddressResolver]
			if !ok || !walk(next) {
				return false
			}
		}
		out = append(out, cur)
		return true
	}
	if !walk(s) {
		return nil, false
	}
	return out, true
}

// probeDetourUsable 判断 detour 能否在测速配置里落地：临时配置只有节点出站与 direct，
// 指向代理组的 detour（如 Auto）会因为出站不存在而让 sing-box 直接启动失败。
//
// 不在这里「顺手」把用户的 detour 清空：那等于把运行时的 DNS 走向偷偷改成直连，
// 测速结论就与真实运行无关了。选不中时换下一个候选，全都不行就在上层报错。
func probeDetourUsable(detour string) bool {
	return detour == "" || strings.EqualFold(detour, "direct")
}

// probeAddressIsLiteralIP 判断服务器地址是否为字面 IP（自举无需任何解析）。
func probeAddressIsLiteralIP(s *DNSServer) bool {
	host := probeServerHost(s)
	return host != "" && net.ParseIP(host) != nil
}

// probeServerHost 取服务器主机名部分：local 类型没有地址，返回空串。
func probeServerHost(s *DNSServer) string {
	switch s.Type {
	case "local":
		return ""
	case "udp", "tcp":
		host, _ := splitHostPort(s.Address)
		return host
	default:
		host, _, _ := parseTLSDNSAddress(s.Address, s.Type)
		return host
	}
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

// validateProbeDNSShape 复刻主生成器 buildDNS 的结构校验：地址缺失、Tag 重复、
// AddressResolver 悬空都属于配置损坏，直接报错而不静默跳过——静默跳过会让探针
// 悄悄少一个解析来源，又变成「同一节点两处结论不同」。
func validateProbeDNSShape(servers []DNSServer) error {
	tags := map[string]bool{}
	for _, s := range servers {
		if s.Type != "local" && strings.TrimSpace(s.Address) == "" {
			return fmt.Errorf("DNS 服务器 %q 缺少地址", s.Tag)
		}
		if tags[s.Tag] {
			return fmt.Errorf("DNS 服务器 Tag 重复: %q", s.Tag)
		}
		tags[s.Tag] = true
	}
	for _, s := range servers {
		if s.AddressResolver != "" && !tags[s.AddressResolver] {
			return fmt.Errorf("DNS 服务器 %q 的 AddressResolver %q 不是已启用的 DNS 服务器", s.Tag, s.AddressResolver)
		}
	}
	return nil
}

// findDNSResolverCycle 检测 AddressResolver 环，返回环上第一个 tag 的路径描述。
//
// 主生成器不做这一步（sing-box 会在解析服务器地址时打转，最终表现为连不上），
// 但在探针里这类配置的症状是「测速全体失败」，极难与节点故障区分，因此在这里
// 就变成一句能读懂的配置错误。
func findDNSResolverCycle(servers []DNSServer) (string, bool) {
	byTag := make(map[string]string, len(servers))
	for _, s := range servers {
		byTag[s.Tag] = s.AddressResolver
	}
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var walk func(tag string, path []string) (string, bool)
	walk = func(tag string, path []string) (string, bool) {
		switch state[tag] {
		case done:
			return "", false
		case visiting:
			cycle := append(path, tag)
			for i, t := range cycle {
				if t == tag {
					cycle = cycle[i:]
					break
				}
			}
			return strings.Join(cycle, " -> "), true
		}
		state[tag] = visiting
		if next := byTag[tag]; next != "" {
			// path 显式复制：append 可能复用底层数组，跨层共享会让环路径串味。
			if desc, ok := walk(next, append(append([]string{}, path...), tag)); ok {
				return desc, true
			}
		}
		state[tag] = done
		return "", false
	}
	for _, s := range servers {
		if desc, ok := walk(s.Tag, nil); ok {
			return desc, true
		}
	}
	return "", false
}
