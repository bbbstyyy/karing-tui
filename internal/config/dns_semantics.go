package config

import "net/netip"

// DNS 服务器「是否需要 domain_resolver」的唯一判据（V8-1）。
//
// 为什么必须有唯一判据：正式配置生成（generate.go）、独立测速（probe.go）、
// 环检测（dns_graph.go）、以及 dns.Manager 的写入规范化/依赖阻断/界面展示
// 共五处都要回答同一个问题。任何一处自己写 IP/域名判断，迟早分叉出
// 「正式配置与测速语义不一致」或「环检测误报」这类问题——本轮修的正是前者：
// probe 早就按正确语义去掉字面 IP 的 domain_resolver，而正式配置还在输出它。

// DNSServerNeedsDomainResolver 判断这条 DNS 服务器的**服务器地址**是否需要靠
// domain_resolver（模型层字段名 AddressResolver）解析。
//
// 语义只有一条：地址是**域名**才需要解析。
//
//	local                              → 本机解析器，没有地址            → 不需要
//	字面 IPv4/IPv6（含端口、scheme、zone）→ 已经是 IP，不存在「域名→IP」这一步 → 不需要
//	真正的域名                          → 需要
//
// 为什么值得显式判：默认 DNS 长期是 remote = https + 8.8.8.8 + AddressResolver=local，
// 于是「8.8.8.8 要靠 local 解析」这条无意义依赖被写进正式配置。后果不止多一个字段：
//   - local 会因为这条残留引用而无法被停用或删除（用户从未表达过这种依赖）；
//   - 环检测会沿它走链，把两条都只需要字面 IP 的服务器误判成环；
//   - 独立测速路径早已忽略它，两条路径语义分叉。
func DNSServerNeedsDomainResolver(s *DNSServer) bool {
	if s == nil || s.Type == "local" {
		return false
	}
	host := dnsServerHost(s)
	if host == "" {
		return false
	}
	return !isLiteralIPHost(host)
}

// isLiteralIPHost 是全仓**唯一**的「这个 host 是字面 IP 吗」判据。
//
// 用 `net/netip` 而不是 `net.ParseIP`（V9-4）：`net.ParseIP` 认不出带 zone 的 IPv6
// （`fe80::1%en0`、`::1%lo0`），而 zone 对链路本地地址是**必需**的一部分，不是笔误。
// 认不出就会被当成域名，于是这类 DNS 服务器会：
//   - 被要求填 domain_resolver（一条用户从未表达过的依赖）；
//   - 参与环检测与依赖阻断，把别的服务器锁死；
//   - **被独立测速直接拒绝为 bootstrap 候选**（probe.go 报「服务器地址是域名但没有
//     AddressResolver，只能靠 default_domain_resolver 解析自身」）——若它是用户唯一的
//     DNS，整批节点测速都会失败。这一条是实测出来的，不是排序问题；
//   - 在 bootstrap 优先级排序里落到域名之后。
//
// 注意 `netip.ParseAddr` **不是** `net.ParseIP` 的严格超集：`192.168.1.1%en0`
// （zone 只对 IPv6 合法）两者都认不出，仍按域名处理——与改动前一致，无回归面。
// 对照用例见 `internal/config/dns_zone_ip_test.go`。
func isLiteralIPHost(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}

// dnsServerHostIsLiteralIP 组合 host 提取与字面 IP 判据；host 为空（local、地址缺失）
// 时返回 false。
//
// 存在的意义：`probe.go::dnsCandidateOrder` 也要问「这条服务器能不能不靠解析直连」，
// 它必须问**同一句话**。历史上那里有一份自己的 `net.ParseIP` 判断
// （`probeAddressIsLiteralIP`），于是 V9-4 修 zone 语义时差点只修一半——
// 那份副本已删除，别名也不要再造。
func dnsServerHostIsLiteralIP(s *DNSServer) bool {
	host := dnsServerHost(s)
	return host != "" && isLiteralIPHost(host)
}

// dnsServerHost 取服务器地址的主机名部分；local 类型没有地址，返回空串。
//
// 按类型分派到**既有**的地址解析逻辑（udp/tcp 用 splitHostPort，TLS 系用
// parseTLSDNSAddress），因此 `1.1.1.1:53`、`[2001:4860::8888]:53`、
// `https://dns.google/dns-query` 这些写法提取出的 host 与生成配置时用的是同一套代码。
// 不要在别处另写一份——那正是本文件要消灭的东西。
//
// 已知残留（本轮不修）：`%25` 转义只在 URL 路径被解开。`net.SplitHostPort` **不**反转义，
// 所以 `[fe80::1%25en0]:53` 取出的 host 是 `fe80::1%25en0`（zone 会被读成 `25en0`），
// 而 `https://[fe80::1%25lo0]/dns-query` 取出 `fe80::1%lo0`。修它要动 splitHostPort、
// 进而动生成产物，属独立变更。
func dnsServerHost(s *DNSServer) string {
	if s == nil {
		return ""
	}
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
