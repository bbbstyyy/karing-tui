package subscription

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// parseSingBox 解析 sing-box JSON 配置中的 outbounds。
// 支持协议：shadowsocks / vmess / vless / trojan / hysteria / hysteria2 / tuic；
// direct/block/dns/selector/urltest 等非代理出站跳过。
func parseSingBox(content string) ([]*config.Node, error) {
	var doc struct {
		Outbounds []json.RawMessage `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		return nil, fmt.Errorf("sing-box JSON 解析失败: %w", err)
	}
	if len(doc.Outbounds) == 0 {
		return nil, fmt.Errorf("sing-box 配置中没有 outbounds 条目")
	}
	var (
		nodes []*config.Node
		skips []string
	)
	for _, raw := range doc.Outbounds {
		var ob sbOutbound
		if err := json.Unmarshal(raw, &ob); err != nil {
			skips = append(skips, fmt.Sprintf("出站解析失败: %v", err))
			continue
		}
		switch ob.Type {
		case "shadowsocks", "vmess", "vless", "trojan", "hysteria", "hysteria2", "tuic", "socks", "http", "ssh", "anytls", "shadowtls", "naive":
		default:
			skips = append(skips, fmt.Sprintf("类型 %q 暂不支持", ob.Type))
			continue
		}
		if ob.Server == "" {
			skips = append(skips, fmt.Sprintf("出站 %q 缺少 server", ob.Tag))
			continue
		}
		if _, ok := singBoxOutboundPort(&ob); !ok {
			skips = append(skips, fmt.Sprintf("出站 %q 缺少有效 server_port/server_ports", ob.Tag))
			continue
		}
		nodes = append(nodes, sbOutboundToNode(&ob))
	}
	if len(nodes) == 0 {
		if len(skips) > 0 {
			return nil, fmt.Errorf("未从 sing-box 配置解析出节点（跳过 %d 条），首条: %s", len(skips), skips[0])
		}
		return nil, fmt.Errorf("sing-box 配置为空")
	}
	return nodes, nil
}

// sbOutbound sing-box 出站字段的子集，仅覆盖解析所需。
type sbOutbound struct {
	Type        string   `json:"type"`
	Tag         string   `json:"tag"`
	Server      string   `json:"server"`
	ServerPort  int      `json:"server_port"`
	ServerPorts []string `json:"server_ports"`
	// shadowsocks
	Method   string `json:"method"`
	Password string `json:"password"`
	// vmess / vless / tuic
	UUID     string `json:"uuid"`
	Security string `json:"security"`
	AlterID  int    `json:"alter_id"`
	Flow     string `json:"flow"`
	// tuic
	CongestionControl string `json:"congestion_control"`
	UDPRelayMode      string `json:"udp_relay_mode"`
	Username          string `json:"username"`
	User              string `json:"user"`
	PrivateKey        string `json:"private_key"`
	PrivateKeyPath    string `json:"private_key_path"`
	Quic              bool   `json:"quic"`
	Version           int    `json:"version"`
	// hysteria / hysteria2
	AuthString string       `json:"auth_str"`
	UpMbps     int          `json:"up_mbps"`
	DownMbps   int          `json:"down_mbps"`
	Obfs       any          `json:"obfs"` // hysteria2 为对象，hysteria v1 为字符串
	TLS        *sbTLS       `json:"tls"`
	Transport  *sbTransport `json:"transport"`
}

type sbTLS struct {
	Enabled    bool     `json:"enabled"`
	ServerName string   `json:"server_name"`
	Insecure   bool     `json:"insecure"`
	ALPN       []string `json:"alpn"`
	UTLS       *struct {
		Fingerprint string `json:"fingerprint"`
	} `json:"utls"`
	Reality *struct {
		PublicKey string `json:"public_key"`
		ShortID   string `json:"short_id"`
	} `json:"reality"`
}

type sbTransport struct {
	Type        string         `json:"type"`
	Path        string         `json:"path"`
	Host        []string       `json:"host"`    // http 传输
	Headers     map[string]any `json:"headers"` // ws 传输
	ServiceName string         `json:"service_name"`
}

// sbOutboundToNode 将 sing-box 出站转换为统一节点。
func sbOutboundToNode(ob *sbOutbound) *config.Node {
	meta := map[string]any{}
	port, _ := singBoxOutboundPort(ob)
	if len(ob.ServerPorts) > 0 {
		meta["server_ports"] = append([]string(nil), ob.ServerPorts...)
	}
	switch ob.Type {
	case "shadowsocks":
		meta["method"] = ob.Method
		meta["password"] = ob.Password
	case "vmess":
		meta["uuid"] = ob.UUID
		meta["method"] = orDefault(ob.Security, "auto")
		if ob.AlterID != 0 {
			meta["alter_id"] = ob.AlterID
		}
	case "vless":
		meta["uuid"] = ob.UUID
		meta["encryption"] = "none"
		if ob.Flow != "" {
			meta["flow"] = ob.Flow
		}
	case "trojan", "hysteria2":
		meta["password"] = ob.Password
	case "hysteria":
		if ob.AuthString != "" {
			meta["password"] = ob.AuthString
		}
	case "tuic":
		meta["uuid"] = ob.UUID
		meta["password"] = ob.Password
		if ob.CongestionControl != "" {
			meta["congestion_control"] = ob.CongestionControl
		}
		if ob.UDPRelayMode != "" {
			meta["udp_relay_mode"] = ob.UDPRelayMode
		}
	case "socks", "http":
		if ob.Username != "" {
			meta["username"] = ob.Username
		}
		if ob.Password != "" {
			meta["password"] = ob.Password
		}
	case "ssh":
		if ob.User != "" {
			meta["user"] = ob.User
		}
		if ob.Password != "" {
			meta["password"] = ob.Password
		}
		if ob.PrivateKey != "" {
			meta["private_key"] = ob.PrivateKey
		}
		if ob.PrivateKeyPath != "" {
			meta["private_key_path"] = ob.PrivateKeyPath
		}
	case "anytls", "shadowtls":
		if ob.Password != "" {
			meta["password"] = ob.Password
		}
		if ob.Version != 0 {
			meta["version"] = ob.Version
		}
	case "naive":
		if ob.Username != "" {
			meta["username"] = ob.Username
		}
		if ob.Password != "" {
			meta["password"] = ob.Password
		}
		if ob.Quic {
			meta["quic"] = true
		}
	}
	if ob.UpMbps > 0 {
		meta["up_mbps"] = ob.UpMbps
	}
	if ob.DownMbps > 0 {
		meta["down_mbps"] = ob.DownMbps
	}
	switch v := ob.Obfs.(type) {
	case string: // hysteria v1
		if v != "" {
			meta["obfs"] = v
		}
	case map[string]any: // hysteria2
		if t, _ := v["type"].(string); t != "" {
			meta["obfs"] = t
		}
		if p, _ := v["password"].(string); p != "" {
			meta["obfs_password"] = p
		}
	}
	if ob.TLS != nil {
		if ob.TLS.Enabled {
			if ob.TLS.ServerName != "" {
				meta["sni"] = ob.TLS.ServerName
			}
			if ob.TLS.Insecure {
				meta["allow_insecure"] = true
			}
			if len(ob.TLS.ALPN) > 0 {
				meta["alpn"] = ob.TLS.ALPN
			}
			if ob.TLS.UTLS != nil && ob.TLS.UTLS.Fingerprint != "" {
				meta["fingerprint"] = ob.TLS.UTLS.Fingerprint
			}
			if ob.TLS.Reality != nil && ob.TLS.Reality.PublicKey != "" {
				meta["reality_public_key"] = ob.TLS.Reality.PublicKey
				meta["reality_short_id"] = ob.TLS.Reality.ShortID
			}
		}
	}
	transport := ""
	if ob.Transport != nil {
		transport = normalizeTransport(ob.Transport.Type)
		switch transport {
		case "ws", "httpupgrade":
			if ob.Transport.Path != "" {
				meta["path"] = ob.Transport.Path
			}
			if h := wsHost(ob.Transport.Headers); h != "" {
				meta["host"] = h
			}
		case "http":
			if ob.Transport.Path != "" {
				meta["path"] = ob.Transport.Path
			}
			if len(ob.Transport.Host) > 0 {
				meta["host"] = ob.Transport.Host[0]
			}
		case "grpc":
			if ob.Transport.ServiceName != "" {
				meta["service_name"] = ob.Transport.ServiceName
			}
		}
	}
	// trojan/hysteria/hysteria2/tuic 必为 TLS；其余以 tls.enabled 为准
	tls := ob.TLS != nil && ob.TLS.Enabled
	switch ob.Type {
	case "trojan", "hysteria", "hysteria2", "tuic":
		tls = true
	}
	return &config.Node{
		Name:      nameOrDefault(ob.Tag, ob.Server, port),
		Protocol:  ob.Type,
		Server:    ob.Server,
		Port:      port,
		TLS:       tls,
		Transport: transport,
		Metadata:  meta,
	}
}

// singBoxOutboundPort returns the regular port or the first port in a
// server_ports range. The internal node model keeps one display/test port,
// while the original range is preserved in metadata for config generation.
func singBoxOutboundPort(ob *sbOutbound) (int, bool) {
	if ob.ServerPort >= 1 && ob.ServerPort <= 65535 {
		return ob.ServerPort, true
	}
	for _, raw := range ob.ServerPorts {
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

// wsHost 从 ws 传输的 headers 中取 Host。
func wsHost(headers map[string]any) string {
	for _, k := range []string{"Host", "host"} {
		if v, ok := headers[k]; ok {
			switch t := v.(type) {
			case string:
				if t != "" {
					return t
				}
			case []any:
				if len(t) > 0 {
					if s, ok := t[0].(string); ok {
						return s
					}
				}
			}
		}
	}
	return ""
}
