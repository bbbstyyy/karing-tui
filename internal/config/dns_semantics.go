package config

import "net"

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
//	字面 IPv4/IPv6（含带端口、带 scheme）→ 已经是 IP，不存在「域名→IP」这一步 → 不需要
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
	return net.ParseIP(host) == nil
}

// dnsServerHost 取服务器地址的主机名部分；local 类型没有地址，返回空串。
//
// 按类型分派到**既有**的地址解析逻辑（udp/tcp 用 splitHostPort，TLS 系用
// parseTLSDNSAddress），因此 `1.1.1.1:53`、`[2001:4860::8888]:53`、
// `https://dns.google/dns-query` 这些写法提取出的 host 与生成配置时用的是同一套代码。
// 不要在别处另写一份——那正是本文件要消灭的东西。
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
