// Package subscription 实现订阅的下载、格式识别与解析。
// 支持格式：明文分享链接列表、base64 编码的链接列表、Clash YAML 配置、sing-box JSON 配置。
// 解析结果统一为 config.Node；协议专属字段放在 Node.Metadata，
// 键名与 sing-box outbound 字段对应，供配置生成器直接使用。
package subscription

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// Format 订阅内容格式。
type Format string

const (
	FormatLinks   Format = "links"    // 明文分享链接列表
	FormatBase64  Format = "base64"   // base64 编码的分享链接列表
	FormatClash   Format = "clash"    // Clash YAML 配置
	FormatSingBox Format = "sing-box" // sing-box JSON 配置
	FormatUnknown Format = "unknown"
)

// DetectFormat 识别订阅内容格式。
func DetectFormat(content string) Format {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return FormatUnknown
	}
	if strings.HasPrefix(trimmed, "{") {
		var probe struct {
			Outbounds []json.RawMessage `json:"outbounds"`
		}
		if json.Unmarshal([]byte(trimmed), &probe) == nil && len(probe.Outbounds) > 0 {
			return FormatSingBox
		}
		return FormatUnknown
	}
	// Clash YAML 的标志性键（base64 字母表不含冒号，不会误判）
	if strings.Contains(trimmed, "proxies:") {
		return FormatClash
	}
	// 明文链接列表可包含空行和注释，以首个有效 URI 行为准。
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if schemeOf(line) != "" {
			return FormatLinks
		}
		break
	}
	// base64：解码后能识别为已知格式才算
	if plain, ok := b64Decode(trimmed); ok && DetectFormat(plain) != FormatUnknown {
		return FormatBase64
	}
	return FormatUnknown
}

// ParseContent 将订阅内容解析为统一节点列表。
// 节点的 SubscriptionID 由调用方（Manager）填回；Enabled 默认 true。
func ParseContent(content string) ([]*config.Node, error) {
	switch f := DetectFormat(content); f {
	case FormatLinks:
		return parseLinks(content)
	case FormatBase64:
		plain, _ := b64Decode(strings.TrimSpace(content))
		return ParseContent(plain)
	case FormatClash:
		return parseClash(content)
	case FormatSingBox:
		return parseSingBox(content)
	default:
		return nil, fmt.Errorf("无法识别的订阅格式（支持：分享链接列表、base64、Clash YAML、sing-box JSON）")
	}
}

// --- 分享链接解析 ---

func parseLinks(content string) ([]*config.Node, error) {
	var (
		nodes []*config.Node
		errs  []string
	)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		for _, link := range strings.Fields(line) {
			n, err := parseShareLink(link)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			nodes = append(nodes, n)
		}
	}
	if len(nodes) == 0 {
		if len(errs) > 0 {
			return nil, fmt.Errorf("未解析出任何节点，首条错误: %s", errs[0])
		}
		return nil, fmt.Errorf("订阅内容为空")
	}
	return nodes, nil
}

// parseShareLink 解析单条分享链接。
func parseShareLink(raw string) (*config.Node, error) {
	raw = strings.TrimSpace(raw)
	switch schemeOf(raw) {
	case "ss":
		return parseSS(raw)
	case "ssr":
		return parseSSR(raw)
	case "vmess":
		return parseVmess(raw)
	case "vless":
		return parseVless(raw)
	case "trojan":
		return parseTrojan(raw)
	case "hysteria2", "hy2":
		return parseHysteria2(raw)
	case "hysteria":
		return parseHysteria(raw)
	case "tuic":
		return parseTUIC(raw)
	case "socks", "socks5":
		return parseSimpleProxy(raw, "socks")
	case "http", "https":
		n, err := parseSimpleProxy(raw, "http")
		if err == nil && schemeOf(raw) == "https" {
			n.TLS = true
		}
		return n, err
	case "ssh":
		return parseSSH(raw)
	case "anytls":
		return parseAnyTLS(raw)
	case "shadowtls":
		return parseShadowTLS(raw)
	case "naive":
		return parseNaive(raw)
	default:
		return nil, fmt.Errorf("不支持的链接: %s", truncate(raw, 40))
	}
}

func parseSimpleProxy(raw, protocol string) (*config.Node, error) {
	rest := raw[strings.Index(raw, "://")+3:]
	name := fragmentName(raw)
	userinfo, hostport, query := splitURI(rest)
	host, port, err := splitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	if port <= 0 || strings.Contains(host, "/") {
		return nil, fmt.Errorf("代理地址缺少合法端口")
	}
	meta := map[string]any{}
	if userinfo != "" {
		parts := strings.SplitN(userinfo, ":", 2)
		meta["username"] = parts[0]
		if len(parts) == 2 {
			meta["password"] = parts[1]
		}
	}
	q := mustParseQuery(query)
	if v := q.Get("username"); v != "" {
		meta["username"] = v
	}
	if v := q.Get("password"); v != "" {
		meta["password"] = v
	}
	return &config.Node{Name: nameOrDefault(name, host, port), Protocol: protocol, Server: host, Port: port, Metadata: meta}, nil
}

func parseSSH(raw string) (*config.Node, error) {
	rest := raw[len("ssh://"):]
	name := fragmentName(raw)
	userinfo, hostport, query := splitURI(rest)
	host, port, err := splitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	meta := map[string]any{}
	if userinfo != "" {
		parts := strings.SplitN(userinfo, ":", 2)
		meta["user"] = parts[0]
		if len(parts) == 2 {
			meta["password"] = parts[1]
		}
	}
	q := mustParseQuery(query)
	if v := q.Get("user"); v != "" {
		meta["user"] = v
	}
	if v := q.Get("private_key"); v != "" {
		meta["private_key"] = v
	}
	if meta["user"] == nil {
		return nil, fmt.Errorf("ssh 链接缺少用户")
	}
	return &config.Node{Name: nameOrDefault(name, host, port), Protocol: "ssh", Server: host, Port: port, Metadata: meta}, nil
}

func parseAnyTLS(raw string) (*config.Node, error) {
	return parseCredentialURI(raw, "anytls")
}

func parseShadowTLS(raw string) (*config.Node, error) {
	n, err := parseCredentialURI(raw, "shadowtls")
	if err != nil {
		return nil, err
	}
	q := mustParseQuery(splitURIValue(raw))
	if v := q.Get("version"); v != "" {
		if i, e := strconv.Atoi(v); e == nil {
			n.Metadata["version"] = i
		}
	}
	return n, nil
}

func parseNaive(raw string) (*config.Node, error) { return parseCredentialURI(raw, "naive") }

func parseCredentialURI(raw, protocol string) (*config.Node, error) {
	prefix := protocol + "://"
	rest := raw[len(prefix):]
	name := fragmentName(raw)
	userinfo, hostport, query := splitURI(rest)
	host, port, err := splitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	meta := map[string]any{}
	if userinfo != "" {
		parts := strings.SplitN(userinfo, ":", 2)
		if protocol == "anytls" || protocol == "shadowtls" {
			if len(parts) == 1 {
				meta["password"] = parts[0]
			} else {
				meta["username"] = parts[0]
				meta["password"] = parts[1]
			}
		} else {
			meta["username"] = parts[0]
			if len(parts) == 2 {
				meta["password"] = parts[1]
			}
		}
	}
	q := mustParseQuery(query)
	for _, k := range []string{"username", "password"} {
		if v := q.Get(k); v != "" {
			meta[k] = v
		}
	}
	applyTLSQuery(meta, q)
	if protocol == "naive" && boolish(firstOf(q, "quic")) {
		meta["quic"] = true
	}
	return &config.Node{Name: nameOrDefault(name, host, port), Protocol: protocol, Server: host, Port: port, TLS: protocol == "naive" || protocol == "anytls" || protocol == "shadowtls", Metadata: meta}, nil
}

func splitURIValue(raw string) string {
	if i := strings.Index(raw, "?"); i >= 0 {
		return raw[i+1:]
	}
	return ""
}

// parseSS 解析 ss:// 链接，兼容 SIP002（userinfo@host:port）与旧版整体 base64 两种形式。
func parseSS(raw string) (*config.Node, error) {
	rest := raw[len("ss://"):]
	name := fragmentName(raw)
	if i := strings.IndexAny(rest, "#?"); i >= 0 {
		rest = rest[:i]
	}
	var method, password, host string
	var port int
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		// SIP002：userinfo@hostport；userinfo 可能是 base64(method:password) 或明文
		userinfo, hostport := rest[:at], rest[at+1:]
		var ok bool
		method, password, ok = decodeSSUserinfo(userinfo)
		if !ok {
			return nil, fmt.Errorf("ss 用户信息无法解码: %s", truncate(userinfo, 30))
		}
		var err error
		if host, port, err = splitHostPort(hostport); err != nil {
			return nil, err
		}
	} else {
		// 旧版：整体 base64(method:password@host:port)
		decoded, ok := b64Decode(rest)
		if !ok {
			return nil, fmt.Errorf("ss 链接无法解码")
		}
		at := strings.LastIndex(decoded, "@")
		if at < 0 {
			return nil, fmt.Errorf("ss 链接格式错误")
		}
		i := strings.Index(decoded[:at], ":")
		if i < 0 {
			return nil, fmt.Errorf("ss 链接缺少加密方法")
		}
		method, password = decoded[:i], decoded[i+1:at]
		var err error
		if host, port, err = splitHostPort(decoded[at+1:]); err != nil {
			return nil, err
		}
	}
	if method == "" || password == "" || host == "" || port == 0 {
		return nil, fmt.Errorf("ss 链接字段不完整")
	}
	return &config.Node{
		Name:     nameOrDefault(name, host, port),
		Protocol: "shadowsocks",
		Server:   host,
		Port:     port,
		Metadata: map[string]any{"method": method, "password": password},
	}, nil
}

// parseSSR 解析 ssr:// 链接。sing-box 不支持 SSR，仅解析入池展示。
func parseSSR(raw string) (*config.Node, error) {
	b64 := raw[len("ssr://"):]
	if i := strings.IndexAny(b64, "#"); i >= 0 {
		b64 = b64[:i]
	}
	decoded, ok := b64Decode(b64)
	if !ok {
		return nil, fmt.Errorf("ssr 链接无法解码")
	}
	// host:port:protocol:method:obfs:base64(password)/?obfsparam=..&protoparam=..&remarks=..
	main, paramsStr, _ := strings.Cut(decoded, "/")
	parts := strings.Split(main, ":")
	if len(parts) < 6 {
		return nil, fmt.Errorf("ssr 链接格式错误")
	}
	// IPv6 裸地址本身含冒号，从右侧取固定 5 段
	host := strings.Join(parts[:len(parts)-5], ":")
	port, err := strconv.Atoi(parts[len(parts)-5])
	if err != nil {
		return nil, fmt.Errorf("ssr 端口非法: %s", parts[len(parts)-5])
	}
	protocol, method, obfs := parts[len(parts)-4], parts[len(parts)-3], parts[len(parts)-2]
	password, ok := b64Decode(parts[len(parts)-1])
	if !ok {
		return nil, fmt.Errorf("ssr 密码无法解码")
	}
	meta := map[string]any{"method": method, "password": password, "protocol": protocol, "obfs": obfs}
	if v, ok := b64QueryGet(paramsStr, "protoparam"); ok {
		meta["protocol_param"] = v
	}
	if v, ok := b64QueryGet(paramsStr, "obfsparam"); ok {
		meta["obfs_param"] = v
	}
	name := fragmentName(raw)
	if name == "" {
		if v, ok := b64QueryGet(paramsStr, "remarks"); ok {
			name = v
		}
	}
	return &config.Node{
		Name:     nameOrDefault(name, host, port),
		Protocol: "ssr",
		Server:   host,
		Port:     port,
		Metadata: meta,
	}, nil
}

// parseVmess 解析 vmess:// 链接（base64 编码的 JSON）。
func parseVmess(raw string) (*config.Node, error) {
	b64 := raw[len("vmess://"):]
	if i := strings.IndexAny(b64, "#"); i >= 0 {
		b64 = b64[:i]
	}
	decoded, ok := b64Decode(b64)
	if !ok {
		return nil, fmt.Errorf("vmess 链接无法解码")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(decoded), &m); err != nil {
		return nil, fmt.Errorf("vmess 链接 JSON 解析失败: %w", err)
	}
	host := mStr(m, "add")
	port, _ := mInt(m, "port")
	if host == "" || port == 0 {
		return nil, fmt.Errorf("vmess 链接缺少服务器或端口")
	}
	transport := normalizeTransport(mStr(m, "net"))
	tls := mStr(m, "tls") == "tls" || mStr(m, "tls") == "reality"
	meta := map[string]any{
		"uuid":   mStr(m, "id"),
		"method": orDefault(mStr(m, "scy"), "auto"),
	}
	if aid, ok := mInt(m, "aid"); ok {
		meta["alter_id"] = aid
	}
	applyTransportMeta(meta, transport, mStr(m, "host"), mStr(m, "path"))
	if s := mStr(m, "sni"); s != "" {
		meta["sni"] = s
	} else if tls {
		if h := mStr(m, "host"); h != "" {
			meta["sni"] = h
		}
	}
	if fp := mStr(m, "fp"); fp != "" && fp != "none" {
		meta["fingerprint"] = fp
	}
	if alpn := mStr(m, "alpn"); alpn != "" {
		meta["alpn"] = splitCSV(alpn)
	}
	return &config.Node{
		Name:      nameOrDefault(mStr(m, "ps"), host, port),
		Protocol:  "vmess",
		Server:    host,
		Port:      port,
		TLS:       tls,
		Transport: transport,
		Metadata:  meta,
	}, nil
}

// parseVless 解析 vless://uuid@host:port?params#name 链接。
func parseVless(raw string) (*config.Node, error) {
	rest := raw[len("vless://"):]
	name := fragmentName(raw)
	userinfo, hostport, query := splitURI(rest)
	if userinfo == "" {
		return nil, fmt.Errorf("vless 链接缺少 UUID")
	}
	host, port, err := splitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	if host == "" || port == 0 {
		return nil, fmt.Errorf("vless 链接缺少服务器或端口")
	}
	q := mustParseQuery(query)
	security := q.Get("security")
	tls := security == "tls" || security == "reality"
	meta := map[string]any{
		"uuid":       userinfo,
		"encryption": orDefault(q.Get("encryption"), "none"),
	}
	if flow := q.Get("flow"); flow != "" {
		meta["flow"] = flow
	}
	applyTLSQuery(meta, q)
	if security == "reality" {
		meta["reality_public_key"] = q.Get("pbk")
		meta["reality_short_id"] = q.Get("sid")
	}
	transport := normalizeTransport(orDefault(q.Get("type"), "tcp"))
	applyTransportQuery(meta, transport, q)
	return &config.Node{
		Name:      nameOrDefault(name, host, port),
		Protocol:  "vless",
		Server:    host,
		Port:      port,
		TLS:       tls,
		Transport: transport,
		Metadata:  meta,
	}, nil
}

// parseTrojan 解析 trojan://password@host:port?params#name 链接。
func parseTrojan(raw string) (*config.Node, error) {
	rest := raw[len("trojan://"):]
	name := fragmentName(raw)
	userinfo, hostport, query := splitURI(rest)
	if userinfo == "" {
		return nil, fmt.Errorf("trojan 链接缺少密码")
	}
	host, port, err := splitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	if host == "" || port == 0 {
		return nil, fmt.Errorf("trojan 链接缺少服务器或端口")
	}
	q := mustParseQuery(query)
	meta := map[string]any{"password": userinfo}
	applyTLSQuery(meta, q)
	transport := normalizeTransport(orDefault(q.Get("type"), "tcp"))
	applyTransportQuery(meta, transport, q)
	return &config.Node{
		Name:      nameOrDefault(name, host, port),
		Protocol:  "trojan",
		Server:    host,
		Port:      port,
		TLS:       true, // trojan 必为 TLS
		Transport: transport,
		Metadata:  meta,
	}, nil
}

// parseHysteria2 解析 hysteria2://（或 hy2://）password@host:port?params#name 链接。
// sing-box hysteria2 outbound 只支持一个 server；多端点 URI 明确拒绝，避免静默丢失。
func parseHysteria2(raw string) (*config.Node, error) {
	scheme := "hysteria2"
	if strings.HasPrefix(strings.ToLower(raw), "hy2://") {
		scheme = "hy2"
	}
	rest := raw[len(scheme)+3:]
	name := fragmentName(raw)
	userinfo, hostport, query := splitURI(rest)
	if userinfo == "" {
		return nil, fmt.Errorf("hysteria2 链接缺少密码")
	}
	endpoints := strings.Split(hostport, ",")
	if len(endpoints) > 1 {
		return nil, fmt.Errorf("hysteria2 链接包含多个服务器端点，sing-box 仅支持单个 server，请拆分为多个节点")
	}
	firstEndpoint := strings.TrimSpace(endpoints[0])
	var host string
	var port int
	var err error
	if i := strings.LastIndex(firstEndpoint, ":"); i >= 0 && strings.Contains(firstEndpoint[i+1:], "-") {
		portSpec := firstEndpoint[i+1:]
		parts := strings.SplitN(portSpec, "-", 2)
		lo, e1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		hi, e2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if e1 != nil || e2 != nil || lo < 1 || hi < lo || hi > 65535 {
			return nil, fmt.Errorf("端口范围非法: %s", portSpec)
		}
		host = strings.TrimSuffix(firstEndpoint[:i], "]")
		host = strings.TrimPrefix(host, "[")
		port = lo
	} else {
		host, port, err = splitHostPort(firstEndpoint)
		if err != nil {
			return nil, err
		}
	}
	if host == "" {
		return nil, fmt.Errorf("hysteria2 链接缺少服务器")
	}
	if port == 0 {
		port = 443
	}
	q := mustParseQuery(query)
	meta := map[string]any{"password": userinfo}
	if i := strings.LastIndex(firstEndpoint, ":"); i >= 0 && strings.Contains(firstEndpoint[i+1:], "-") {
		parts := strings.SplitN(firstEndpoint[i+1:], "-", 2)
		if len(parts) == 2 {
			if lo, e1 := strconv.Atoi(strings.TrimSpace(parts[0])); e1 == nil {
				if hi, e2 := strconv.Atoi(strings.TrimSpace(parts[1])); e2 == nil && lo > 0 && hi >= lo && hi <= 65535 {
					meta["server_ports"] = []string{fmt.Sprintf("%d:%d", lo, hi)}
					port = lo
				}
			}
		}
	}
	applyTLSQuery(meta, q)
	if v := q.Get("obfs"); v != "" {
		meta["obfs"] = v
	}
	if v := firstOf(q, "obfs-password", "obfsParam"); v != "" {
		meta["obfs_password"] = v
	}
	if v := firstOf(q, "up", "upmbps"); v != "" {
		if n, ok := parseMbps(v); ok {
			meta["up_mbps"] = n
		}
	}
	if v := firstOf(q, "down", "downmbps"); v != "" {
		if n, ok := parseMbps(v); ok {
			meta["down_mbps"] = n
		}
	}
	return &config.Node{
		Name:      nameOrDefault(name, host, port),
		Protocol:  "hysteria2",
		Server:    host,
		Port:      port,
		TLS:       true, // hysteria2 必为 TLS
		Transport: "",
		Metadata:  meta,
	}, nil
}

// parseHysteria 解析 hysteria v1 链接：hysteria://host:port?auth=..&peer=..#name。
func parseHysteria(raw string) (*config.Node, error) {
	rest := raw[len("hysteria://"):]
	name := fragmentName(raw)
	_, hostport, query := splitURI(rest)
	host, port, err := splitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	if host == "" || port == 0 {
		return nil, fmt.Errorf("hysteria 链接缺少服务器或端口")
	}
	q := mustParseQuery(query)
	meta := map[string]any{}
	if auth := firstOf(q, "auth", "authstr"); auth != "" {
		meta["password"] = auth
	}
	applyTLSQuery(meta, q)
	if v := firstOf(q, "upmbps", "up"); v != "" {
		if n, ok := parseMbps(v); ok {
			meta["up_mbps"] = n
		}
	}
	if v := firstOf(q, "downmbps", "down"); v != "" {
		if n, ok := parseMbps(v); ok {
			meta["down_mbps"] = n
		}
	}
	if v := firstOf(q, "obfs"); v != "" {
		meta["obfs"] = v
	}
	if v := firstOf(q, "obfsParam"); v != "" {
		meta["obfs_password"] = v
	}
	return &config.Node{
		Name:      nameOrDefault(name, host, port),
		Protocol:  "hysteria",
		Server:    host,
		Port:      port,
		TLS:       true,
		Transport: "",
		Metadata:  meta,
	}, nil
}

// parseTUIC 解析 tuic://uuid:password@host:port?params#name 链接。
func parseTUIC(raw string) (*config.Node, error) {
	rest := raw[len("tuic://"):]
	name := fragmentName(raw)
	userinfo, hostport, query := splitURI(rest)
	uuid, password, _ := strings.Cut(userinfo, ":")
	if uuid == "" {
		return nil, fmt.Errorf("tuic 链接缺少 UUID")
	}
	host, port, err := splitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	if host == "" {
		return nil, fmt.Errorf("tuic 链接缺少服务器")
	}
	if port == 0 {
		port = 443
	}
	q := mustParseQuery(query)
	meta := map[string]any{"uuid": uuid, "password": password}
	applyTLSQuery(meta, q)
	if v := q.Get("congestion_control"); v != "" {
		meta["congestion_control"] = v
	}
	if v := q.Get("udp_relay_mode"); v != "" {
		meta["udp_relay_mode"] = v
	}
	return &config.Node{
		Name:      nameOrDefault(name, host, port),
		Protocol:  "tuic",
		Server:    host,
		Port:      port,
		TLS:       true, // tuic 必为 TLS
		Transport: "",
		Metadata:  meta,
	}, nil
}

// --- 公共辅助 ---

// schemeOf 提取字符串开头的 URI scheme（小写）；无合法 scheme 返回空。
func schemeOf(raw string) string {
	i := strings.Index(raw, "://")
	if i <= 0 {
		return ""
	}
	scheme := raw[:i]
	for j, r := range scheme {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(j > 0 && ((r >= '0' && r <= '9') || r == '+' || r == '-' || r == '.')) {
			continue
		}
		return ""
	}
	return strings.ToLower(scheme)
}

// splitURI 拆分 scheme:// 之后的 userinfo@hostport?query 三段；fragment 在此剥离。
func splitURI(rest string) (userinfo, hostport, query string) {
	if i := strings.Index(rest, "#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.Index(rest, "?"); i >= 0 {
		query = rest[i+1:]
		rest = rest[:i]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		userinfo = percentDecode(rest[:at])
		rest = rest[at+1:]
	}
	return userinfo, rest, query
}

// splitHostPort 拆分主机与端口，支持 [IPv6]:port；无端口时返回 0。
func splitHostPort(hostport string) (string, int, error) {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return "", 0, fmt.Errorf("主机为空")
	}
	if strings.HasPrefix(hostport, "[") {
		end := strings.Index(hostport, "]")
		if end < 0 {
			return "", 0, fmt.Errorf("IPv6 地址格式错误: %s", hostport)
		}
		host := hostport[1:end]
		rest := strings.TrimPrefix(hostport[end+1:], ":")
		if rest == "" {
			return host, 0, nil
		}
		port, err := strconv.Atoi(rest)
		if err != nil {
			return "", 0, fmt.Errorf("端口非法: %s", rest)
		}
		return host, port, nil
	}
	if i := strings.LastIndex(hostport, ":"); i >= 0 {
		port, err := strconv.Atoi(hostport[i+1:])
		if err != nil {
			return "", 0, fmt.Errorf("端口非法: %s", hostport[i+1:])
		}
		return hostport[:i], port, nil
	}
	return hostport, 0, nil
}

// decodeSSUserinfo 解码 ss 链接的用户信息：base64(method:password) 或明文（可 percent 编码）。
func decodeSSUserinfo(userinfo string) (method, password string, ok bool) {
	if !strings.Contains(userinfo, ":") {
		if plain, isB64 := b64Decode(userinfo); isB64 && strings.Contains(plain, ":") {
			userinfo = plain
		} else if dec, err := url.QueryUnescape(userinfo); err == nil {
			userinfo = dec
		}
	} else if dec, err := url.QueryUnescape(userinfo); err == nil {
		userinfo = dec
	}
	i := strings.Index(userinfo, ":")
	if i < 0 {
		return "", "", false
	}
	return userinfo[:i], userinfo[i+1:], true
}

// b64Decode 尝试多种 base64 变体（std/url × raw/带填充）解码，结果须为合法 UTF-8。
func b64Decode(s string) (string, bool) {
	s = strings.NewReplacer("\r", "", "\n", "", " ", "").Replace(strings.TrimSpace(s))
	if s == "" {
		return "", false
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			decoded := string(b)
			if utf8.ValidString(decoded) {
				return decoded, true
			}
		}
	}
	return "", false
}

// b64QueryGet 从 ssr 参数串中取 base64 编码的查询参数并解码。
func b64QueryGet(paramsStr, key string) (string, bool) {
	for _, kv := range strings.Split(paramsStr, "&") {
		kv = strings.TrimPrefix(kv, "?") // 首参数带查询串前缀
		if k, v, found := strings.Cut(kv, "="); found && k == key {
			if plain, ok := b64Decode(v); ok {
				return plain, true
			}
			return v, true
		}
	}
	return "", false
}

// fragmentName 提取链接 # 后的名称（percent 解码）。
func fragmentName(raw string) string {
	i := strings.LastIndex(raw, "#")
	if i < 0 {
		return ""
	}
	if name, err := url.PathUnescape(raw[i+1:]); err == nil {
		return name
	}
	return raw[i+1:]
}

// mustParseQuery 解析查询串；失败时返回空 Values（容错）。
func mustParseQuery(query string) url.Values {
	q, err := url.ParseQuery(query)
	if err != nil {
		return url.Values{}
	}
	return q
}

// percentDecode 容错解码 percent 编码。
func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	if dec, err := url.QueryUnescape(s); err == nil {
		return dec
	}
	return s
}

// normalizeTransport 归一化传输层为 sing-box 规范名："" / ws / grpc / http / httpupgrade / quic。
func normalizeTransport(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "tcp", "none", "raw":
		return ""
	case "ws":
		return "ws"
	case "grpc", "gun":
		return "grpc"
	case "h2", "http2", "http":
		return "http"
	case "httpupgrade":
		return "httpupgrade"
	case "quic":
		return "quic"
	default:
		return ""
	}
}

// applyTransportMeta 按 JSON 链接（vmess）的 host/path 填充传输层元数据。
func applyTransportMeta(meta map[string]any, transport, host, path string) {
	switch transport {
	case "ws", "httpupgrade", "http":
		if host != "" {
			meta["host"] = host
		}
		if path != "" {
			meta["path"] = path
		}
	case "grpc":
		if path != "" {
			meta["service_name"] = path
		}
	}
}

// applyTransportQuery 按查询参数填充传输层元数据（vless/trojan 等链接）。
func applyTransportQuery(meta map[string]any, transport string, q url.Values) {
	switch transport {
	case "ws", "httpupgrade", "http":
		if v := q.Get("host"); v != "" {
			meta["host"] = v
		}
		if v := q.Get("path"); v != "" {
			meta["path"] = v
		}
	case "grpc":
		if v := firstOf(q, "serviceName", "path"); v != "" {
			meta["service_name"] = v
		}
	}
}

// applyTLSQuery 填充 TLS 相关元数据：sni、alpn、指纹、跳过证书校验。
func applyTLSQuery(meta map[string]any, q url.Values) {
	if v := firstOf(q, "sni", "peer"); v != "" {
		meta["sni"] = v
	}
	if v := q.Get("alpn"); v != "" {
		meta["alpn"] = splitCSV(v)
	}
	if v := firstOf(q, "fp", "fingerprint"); v != "" && v != "none" {
		meta["fingerprint"] = v
	}
	if boolish(firstOf(q, "allowInsecure", "insecure")) {
		meta["allow_insecure"] = true
	}
}

// --- map 访问辅助（Clash YAML / vmess JSON 共用） ---

// mStr 取第一个非空字符串值。
func mStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// mInt 取第一个可转为整数的值（int/int64/float64/string）。
func mInt(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case int:
			return v, true
		case int64:
			return int(v), true
		case float64:
			return int(v), true
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// mBool 取第一个布尔值（bool 或 "true"/"1" 等字符串）。
func mBool(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		switch v := m[k].(type) {
		case bool:
			return v
		case string:
			if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
				return b
			}
		}
	}
	return false
}

// mSub 取第一个子 map。
func mSub(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if v, ok := m[k].(map[string]any); ok {
			return v
		}
	}
	return nil
}

// mStrSlice 取字符串列表：[]any（元素为 string）或逗号分隔的字符串。
func mStrSlice(m map[string]any, keys ...string) []string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case []any:
			var out []string
			for _, e := range v {
				if s, ok := e.(string); ok && s != "" {
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				return out
			}
		case string:
			if s := splitCSV(v); len(s) > 0 {
				return s
			}
		}
	}
	return nil
}

// firstOf 返回查询参数中第一个非空值。
func firstOf(q url.Values, keys ...string) string {
	for _, k := range keys {
		if v := q.Get(k); v != "" {
			return v
		}
	}
	return ""
}

// splitCSV 按逗号拆分并去掉空白与空项。
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// boolish 判断 "1"/"true"/"yes"。
func boolish(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// parseMbps 解析带宽值："100"、"100 Mbps"、"0.5 Gbps" → Mbps 整数。
func parseMbps(s string) (int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	multiplier := 1.0
	switch {
	case strings.HasSuffix(s, "gbps"):
		multiplier = 1000
		s = strings.TrimSuffix(s, "gbps")
	case strings.HasSuffix(s, "mbps"):
		s = strings.TrimSuffix(s, "mbps")
	case strings.HasSuffix(s, "g"):
		multiplier = 1000
		s = strings.TrimSuffix(s, "g")
	case strings.HasSuffix(s, "m"):
		s = strings.TrimSuffix(s, "m")
	}
	s = strings.TrimSpace(s)
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
		mbps := f * multiplier
		if mbps < 1 {
			return 0, false
		}
		return int(mbps), true
	}
	return 0, false
}

// orDefault 返回 v 或缺省值。
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// nameOrDefault 节点名缺省时用 host:port。
func nameOrDefault(name, host string, port int) string {
	if strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// truncate 截断字符串用于错误信息。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
