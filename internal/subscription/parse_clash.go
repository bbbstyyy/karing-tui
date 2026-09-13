package subscription

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// parseClash 解析 Clash YAML 配置中的 proxies 条目。
// 支持协议：ss / vmess / vless / trojan / hysteria2 / tuic；其余类型跳过并计数。
func parseClash(content string) ([]*config.Node, error) {
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, fmt.Errorf("解析 Clash YAML 失败: %w", err)
	}
	if len(doc.Proxies) == 0 {
		return nil, fmt.Errorf("配置中没有 proxies 条目（Clash）")
	}
	var (
		nodes []*config.Node
		skips []string
	)
	for _, m := range doc.Proxies {
		n, err := clashProxy(m)
		if err != nil {
			skips = append(skips, err.Error())
			continue
		}
		nodes = append(nodes, n)
	}
	if len(nodes) == 0 {
		if len(skips) > 0 {
			return nil, fmt.Errorf("未从 Clash 配置解析出节点（跳过 %d 条），首条: %s", len(skips), skips[0])
		}
		return nil, fmt.Errorf("配置为空（Clash）")
	}
	return nodes, nil
}

// clashProxy 将单个 Clash proxy 条目转换为统一节点。
func clashProxy(m map[string]any) (*config.Node, error) {
	name := mStr(m, "name")
	host := mStr(m, "server")
	port, _ := mInt(m, "port")
	typ := mStr(m, "type")
	if host == "" || port == 0 {
		return nil, fmt.Errorf("节点 %q 缺少 server/port", name)
	}
	var (
		n   *config.Node
		err error
	)
	switch typ {
	case "ss":
		n, err = clashSS(m, name, host, port)
	case "vmess":
		n, err = clashVmess(m, name, host, port)
	case "vless":
		n, err = clashVless(m, name, host, port)
	case "trojan":
		n, err = clashTrojan(m, name, host, port)
	case "hysteria2":
		n, err = clashHysteria2(m, name, host, port)
	case "tuic":
		n, err = clashTUIC(m, name, host, port)
	case "socks5", "socks":
		n, err = clashSimple(m, name, host, port, "socks")
	case "http":
		n, err = clashSimple(m, name, host, port, "http")
	case "ssh":
		n, err = clashSSH(m, name, host, port)
	case "anytls":
		n, err = clashCredential(m, name, host, port, "anytls")
	case "shadowtls":
		n, err = clashCredential(m, name, host, port, "shadowtls")
	case "naive":
		n, err = clashCredential(m, name, host, port, "naive")
	default:
		return nil, fmt.Errorf("节点 %q 类型 %q 暂不支持", name, typ)
	}
	if err != nil {
		return nil, err
	}
	if mBool(m, "udp") {
		n.Metadata["udp"] = true
	}
	return n, nil
}

func clashSimple(m map[string]any, name, host string, port int, protocol string) (*config.Node, error) {
	meta := map[string]any{}
	if v := mStr(m, "username", "user"); v != "" {
		meta["username"] = v
	}
	if v := mStr(m, "password"); v != "" {
		meta["password"] = v
	}
	return &config.Node{Name: nameOrDefault(name, host, port), Protocol: protocol, Server: host, Port: port, Metadata: meta}, nil
}

func clashSSH(m map[string]any, name, host string, port int) (*config.Node, error) {
	meta := map[string]any{"user": mStr(m, "username", "user"), "password": mStr(m, "password")}
	if meta["user"] == "" {
		return nil, fmt.Errorf("ssh 节点 %q 缺少用户", name)
	}
	if v := mStr(m, "private-key", "private_key"); v != "" {
		meta["private_key"] = v
	}
	return &config.Node{Name: nameOrDefault(name, host, port), Protocol: "ssh", Server: host, Port: port, Metadata: meta}, nil
}

func clashCredential(m map[string]any, name, host string, port int, protocol string) (*config.Node, error) {
	meta := map[string]any{}
	for _, pair := range [][2]string{{"username", "username"}, {"password", "password"}, {"sni", "servername"}} {
		if v := mStr(m, pair[1]); v != "" {
			meta[pair[0]] = v
		}
	}
	return &config.Node{Name: nameOrDefault(name, host, port), Protocol: protocol, Server: host, Port: port, TLS: true, Metadata: meta}, nil
}

func clashSS(m map[string]any, name, host string, port int) (*config.Node, error) {
	method := mStr(m, "cipher")
	if method == "" {
		return nil, fmt.Errorf("ss 节点 %q 缺少 cipher", name)
	}
	meta := map[string]any{"method": method, "password": mStr(m, "password")}
	return &config.Node{
		Name: nameOrDefault(name, host, port), Protocol: "shadowsocks",
		Server: host, Port: port, Metadata: meta,
	}, nil
}

func clashVmess(m map[string]any, name, host string, port int) (*config.Node, error) {
	if mStr(m, "uuid") == "" {
		return nil, fmt.Errorf("vmess 节点 %q 缺少 uuid", name)
	}
	tls := mBool(m, "tls")
	meta := map[string]any{
		"uuid":   mStr(m, "uuid"),
		"method": orDefault(mStr(m, "cipher"), "auto"),
	}
	if aid, ok := mInt(m, "alterId", "alter-id"); ok {
		meta["alter_id"] = aid
	}
	applyClashTLS(meta, m, tls)
	applyClashTransport(meta, m)
	return &config.Node{
		Name: nameOrDefault(name, host, port), Protocol: "vmess",
		Server: host, Port: port, TLS: tls, Transport: clashNetwork(m), Metadata: meta,
	}, nil
}

func clashVless(m map[string]any, name, host string, port int) (*config.Node, error) {
	uuid := mStr(m, "uuid")
	if uuid == "" {
		return nil, fmt.Errorf("vless 节点 %q 缺少 uuid", name)
	}
	tls := mBool(m, "tls")
	meta := map[string]any{"uuid": uuid, "encryption": "none"}
	if flow := mStr(m, "flow"); flow != "" {
		meta["flow"] = flow
	}
	if ro := mSub(m, "reality-opts"); ro != nil {
		meta["reality_public_key"] = mStr(ro, "public-key")
		meta["reality_short_id"] = mStr(ro, "short-id")
		tls = true
	}
	applyClashTLS(meta, m, tls)
	applyClashTransport(meta, m)
	return &config.Node{
		Name: nameOrDefault(name, host, port), Protocol: "vless",
		Server: host, Port: port, TLS: tls, Transport: clashNetwork(m), Metadata: meta,
	}, nil
}

func clashTrojan(m map[string]any, name, host string, port int) (*config.Node, error) {
	meta := map[string]any{"password": mStr(m, "password")}
	applyClashTLS(meta, m, true)
	applyClashTransport(meta, m)
	return &config.Node{
		Name: nameOrDefault(name, host, port), Protocol: "trojan",
		Server: host, Port: port, TLS: true, Transport: clashNetwork(m), Metadata: meta,
	}, nil
}

func clashHysteria2(m map[string]any, name, host string, port int) (*config.Node, error) {
	meta := map[string]any{"password": mStr(m, "password", "auth")}
	applyClashTLS(meta, m, true)
	if v := mStr(m, "obfs"); v != "" {
		meta["obfs"] = v
	}
	if v := mStr(m, "obfs-password"); v != "" {
		meta["obfs_password"] = v
	}
	if v, ok := mInt(m, "up"); ok {
		meta["up_mbps"] = v
	} else if s := mStr(m, "up"); s != "" {
		if n, ok := parseMbps(s); ok {
			meta["up_mbps"] = n
		}
	}
	if v, ok := mInt(m, "down"); ok {
		meta["down_mbps"] = v
	} else if s := mStr(m, "down"); s != "" {
		if n, ok := parseMbps(s); ok {
			meta["down_mbps"] = n
		}
	}
	return &config.Node{
		Name: nameOrDefault(name, host, port), Protocol: "hysteria2",
		Server: host, Port: port, TLS: true, Metadata: meta,
	}, nil
}

func clashTUIC(m map[string]any, name, host string, port int) (*config.Node, error) {
	uuid := mStr(m, "uuid")
	if uuid == "" {
		return nil, fmt.Errorf("tuic 节点 %q 缺少 uuid", name)
	}
	meta := map[string]any{"uuid": uuid, "password": mStr(m, "password")}
	applyClashTLS(meta, m, true)
	if v := mStr(m, "congestion-controller", "congestion-control"); v != "" {
		meta["congestion_control"] = v
	}
	if v := mStr(m, "udp-relay-mode"); v != "" {
		meta["udp_relay_mode"] = v
	}
	if alpn := mStrSlice(m, "alpn"); len(alpn) > 0 {
		meta["alpn"] = alpn
	}
	return &config.Node{
		Name: nameOrDefault(name, host, port), Protocol: "tuic",
		Server: host, Port: port, TLS: true, Metadata: meta,
	}, nil
}

// clashNetwork 归一化 Clash 的 network 字段。
func clashNetwork(m map[string]any) string {
	return normalizeTransport(mStr(m, "network"))
}

// applyClashTLS 填充 TLS 元数据：servername、跳过证书校验、uTLS 指纹、ALPN。
func applyClashTLS(meta map[string]any, m map[string]any, tls bool) {
	if !tls {
		return
	}
	if v := mStr(m, "servername"); v != "" {
		meta["sni"] = v
	}
	if mBool(m, "skip-cert-verify") {
		meta["allow_insecure"] = true
	}
	if v := mStr(m, "client-fingerprint"); v != "" {
		meta["fingerprint"] = v
	}
	if alpn := mStrSlice(m, "alpn"); len(alpn) > 0 {
		meta["alpn"] = alpn
	}
}

// applyClashTransport 按 network 填充传输层元数据：ws / grpc / h2 / httpupgrade。
func applyClashTransport(meta map[string]any, m map[string]any) {
	switch clashNetwork(m) {
	case "ws", "httpupgrade":
		if o := mSub(m, "ws-opts"); o != nil {
			if p := mStr(o, "path"); p != "" {
				meta["path"] = p
			}
			if h := mSub(o, "headers"); h != nil {
				if v := headerHost(h); v != "" {
					meta["host"] = v
				}
			}
		}
	case "grpc":
		if o := mSub(m, "grpc-opts"); o != nil {
			if v := mStr(o, "grpc-service-name"); v != "" {
				meta["service_name"] = v
			}
		}
	case "http":
		if o := mSub(m, "h2-opts"); o != nil {
			if p := mStr(o, "path"); p != "" {
				meta["path"] = p
			}
			if hosts := mStrSlice(o, "host"); len(hosts) > 0 {
				meta["host"] = hosts[0]
			}
		}
	}
}

// headerHost 从 ws-opts.headers 中取 Host（值可能是字符串或列表）。
func headerHost(headers map[string]any) string {
	if s := mStr(headers, "Host", "host"); s != "" {
		return s
	}
	if list := mStrSlice(headers, "Host", "host"); len(list) > 0 {
		return list[0]
	}
	return ""
}

// ParseClashProxies 解析 Clash YAML 配置中的 proxies 为统一节点列表
// （供配置迁移复用；与订阅解析同一套协议转换逻辑）。
func ParseClashProxies(content string) ([]*config.Node, error) {
	return parseClash(content)
}
