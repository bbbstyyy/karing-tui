package config

import (
	"fmt"
	"strconv"
	"strings"
)

// NodeToOutbound 将统一节点转换为 sing-box outbound。
// 返回 map 结构；encoding/json 对 map 按键排序输出，同一节点多次生成结果一致（幂等）。
// tag 为出站标签，须在配置内唯一，由调用方决定。
//
// 本函数除配置生成外还被订阅的「节点身份指纹」复用（subscription.nodeFingerprint）。
// 因此**连接语义字段不能遗漏在 outbound 转换之外**：漏掉的字段会同时逃过身份匹配，
// 表现为两个语义不同的节点被判为同一个身份。新增协议字段时请一并更新此处。
func NodeToOutbound(n *Node, tag string) (map[string]any, error) {
	if tag == "" {
		return nil, fmt.Errorf("出站标签为空")
	}
	if n.Protocol != "tor" && (n.Server == "" || n.Port <= 0 || n.Port > 65535) {
		return nil, fmt.Errorf("节点 %q 服务器或端口非法", n.Name)
	}
	m := n.Metadata
	base := map[string]any{
		"type":        n.Protocol,
		"tag":         tag,
		"server":      n.Server,
		"server_port": n.Port,
	}
	switch n.Protocol {
	case "shadowsocks":
		method, password := metaStr(m, "method"), metaStr(m, "password")
		if method == "" || password == "" {
			return nil, fmt.Errorf("节点 %q 缺少加密方法或密码", n.Name)
		}
		base["method"] = method
		base["password"] = password

	case "vmess":
		uuid := metaStr(m, "uuid")
		if uuid == "" {
			return nil, fmt.Errorf("节点 %q 缺少 UUID", n.Name)
		}
		base["uuid"] = uuid
		base["security"] = orDefaultStr(metaStr(m, "method"), "auto")
		if aid, ok := metaInt(m, "alter_id"); ok {
			base["alter_id"] = aid
		}

	case "vless":
		uuid := metaStr(m, "uuid")
		if uuid == "" {
			return nil, fmt.Errorf("节点 %q 缺少 UUID", n.Name)
		}
		base["uuid"] = uuid
		if flow := metaStr(m, "flow"); flow != "" {
			base["flow"] = flow
		}

	case "trojan":
		password := metaStr(m, "password")
		if password == "" {
			return nil, fmt.Errorf("节点 %q 缺少密码", n.Name)
		}
		base["password"] = password

	case "hysteria2":
		password := metaStr(m, "password")
		if password == "" {
			return nil, fmt.Errorf("节点 %q 缺少密码", n.Name)
		}
		base["password"] = password
		if ports := metaStrSlice(m, "server_ports"); len(ports) > 0 {
			base["server_ports"] = ports
			delete(base, "server_port")
		}
		if obfs := metaStr(m, "obfs"); obfs != "" {
			o := map[string]any{"type": obfs}
			if p := metaStr(m, "obfs_password"); p != "" {
				o["password"] = p
			}
			base["obfs"] = o
		}
		if up, ok := metaInt(m, "up_mbps"); ok {
			base["up_mbps"] = up
		}
		if down, ok := metaInt(m, "down_mbps"); ok {
			base["down_mbps"] = down
		}

	case "hysteria":
		auth := metaStr(m, "password")
		// Hysteria v1 的 auth_str 在 sing-box 中是可选的；无认证节点
		// 仍应生成配置，由核心在实际连接阶段处理服务端要求。
		if auth != "" {
			base["auth_str"] = auth
		}
		if ports := metaStrSlice(m, "server_ports"); len(ports) > 0 {
			base["server_ports"] = ports
			delete(base, "server_port")
		}
		if up, ok := metaInt(m, "up_mbps"); ok {
			base["up_mbps"] = up
		}
		if down, ok := metaInt(m, "down_mbps"); ok {
			base["down_mbps"] = down
		}
		if obfs := metaStr(m, "obfs"); obfs != "" {
			base["obfs"] = obfs
		}

	case "ssr":
		return nil, fmt.Errorf("节点 %q 使用 SSR 协议，但 sing-box 不支持 SSR 出站；该节点仅可展示", n.Name)

	case "tuic":
		uuid, password := metaStr(m, "uuid"), metaStr(m, "password")
		if uuid == "" {
			return nil, fmt.Errorf("节点 %q 缺少 UUID", n.Name)
		}
		base["uuid"] = uuid
		if password != "" {
			base["password"] = password
		}
		if cc := metaStr(m, "congestion_control"); cc != "" {
			base["congestion_control"] = cc
		}
		if rm := metaStr(m, "udp_relay_mode"); rm != "" {
			base["udp_relay_mode"] = rm
		}

	case "socks", "socks5":
		base["type"] = "socks"
		if v := metaStr(m, "version"); v != "" && v != "5" {
			base["version"] = v
		}
		if u := metaStr(m, "username"); u != "" {
			base["username"] = u
		}
		if p := metaStr(m, "password"); p != "" {
			base["password"] = p
		}

	case "http":
		base["type"] = "http"
		if u := metaStr(m, "username"); u != "" {
			base["username"] = u
		}
		if p := metaStr(m, "password"); p != "" {
			base["password"] = p
		}
		if path := metaStr(m, "path"); path != "" {
			base["path"] = path
		}

	case "ssh":
		user := metaStr(m, "user")
		if user == "" {
			user = metaStr(m, "username")
		}
		if user == "" {
			return nil, fmt.Errorf("节点 %q 缺少 SSH 用户名", n.Name)
		}
		base["user"] = user
		if p := metaStr(m, "password"); p != "" {
			base["password"] = p
		}
		if key := metaStr(m, "private_key"); key != "" {
			base["private_key"] = key
		}
		if path := metaStr(m, "private_key_path"); path != "" {
			base["private_key_path"] = path
		}

	case "anytls":
		if p := metaStr(m, "password"); p == "" {
			return nil, fmt.Errorf("节点 %q 缺少 AnyTLS 密码", n.Name)
		} else {
			base["password"] = p
		}

	case "shadowtls":
		if p := metaStr(m, "password"); p == "" {
			return nil, fmt.Errorf("节点 %q 缺少 ShadowTLS 密码", n.Name)
		} else {
			base["password"] = p
		}
		if v, ok := metaInt(m, "version"); ok {
			base["version"] = v
		}

	case "naive":
		if u := metaStr(m, "username"); u != "" {
			base["username"] = u
		}
		if p := metaStr(m, "password"); p != "" {
			base["password"] = p
		}
		if metaBool(m, "quic") {
			base["quic"] = true
		}

	case "tor":
		delete(base, "server")
		delete(base, "server_port")
		if p := metaStr(m, "executable_path"); p != "" {
			base["executable_path"] = p
		}
		if d := metaStr(m, "data_directory"); d != "" {
			base["data_directory"] = d
		}
		if args := metaStrSlice(m, "extra_args"); len(args) > 0 {
			base["extra_args"] = args
		}

	default:
		return nil, fmt.Errorf("节点 %q 协议 %q 不支持生成 sing-box 出站", n.Name, n.Protocol)
	}

	if tls := tlsBlock(n); tls != nil {
		base["tls"] = tls
	}
	if tr := transportBlock(n); tr != nil {
		base["transport"] = tr
	}
	return base, nil
}

// tlsBlock 生成 sing-box TLS 配置；非 TLS 节点返回 nil。
func tlsBlock(n *Node) map[string]any {
	m := n.Metadata
	realityKey := metaStr(m, "reality_public_key")
	if !n.TLS && realityKey == "" {
		return nil
	}
	tls := map[string]any{"enabled": true}
	sni := metaStr(m, "sni")
	if sni == "" {
		sni = n.Server // 缺省 SNI 用服务器地址
	}
	tls["server_name"] = sni
	if alpn := metaStrSlice(m, "alpn"); len(alpn) > 0 {
		tls["alpn"] = alpn
	}
	if metaBool(m, "allow_insecure") {
		tls["insecure"] = true
	}
	if fp := metaStr(m, "fingerprint"); fp != "" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
	}
	if realityKey != "" {
		r := map[string]any{"enabled": true, "public_key": realityKey}
		if sid := metaStr(m, "reality_short_id"); sid != "" {
			r["short_id"] = sid
		}
		tls["reality"] = r
	}
	return tls
}

// transportBlock 生成 sing-box 传输层配置；纯 TCP 返回 nil。
func transportBlock(n *Node) map[string]any {
	m := n.Metadata
	switch n.Transport {
	case "ws":
		t := map[string]any{"type": "ws", "path": orDefaultStr(metaStr(m, "path"), "/")}
		if h := metaStr(m, "host"); h != "" {
			t["headers"] = map[string]any{"Host": h}
		}
		return t
	case "http":
		t := map[string]any{"type": "http"}
		if h := metaStr(m, "host"); h != "" {
			t["host"] = []string{h}
		}
		if p := metaStr(m, "path"); p != "" {
			t["path"] = p
		}
		return t
	case "grpc":
		t := map[string]any{"type": "grpc"}
		if s := metaStr(m, "service_name"); s != "" {
			t["service_name"] = s
		}
		return t
	case "httpupgrade":
		t := map[string]any{"type": "httpupgrade"}
		if h := metaStr(m, "host"); h != "" {
			t["host"] = h
		}
		if p := metaStr(m, "path"); p != "" {
			t["path"] = p
		}
		return t
	case "quic":
		return map[string]any{"type": "quic"}
	}
	return nil
}

// --- Metadata 访问辅助 ---

func metaStr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func metaInt(m map[string]any, key string) (int, bool) {
	switch v := m[key].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		// 数字字符串也接受，容错手动录入
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func metaBool(m map[string]any, key string) bool {
	v, ok := m[key].(bool)
	return ok && v
}

func metaStrSlice(m map[string]any, key string) []string {
	switch v := m[key].(type) {
	case []string:
		return v
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
