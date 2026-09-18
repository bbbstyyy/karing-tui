package subscription

import (
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// 身份指纹的单元判据。对应端到端判决见 update_identity_test.go：
// 那三条用例在旧实现（nodeKey = protocol|server|port + 单值索引）下必然变红，
// 是「测试真的能拦住回归」的证据；本文件则钉住指纹本身的等价/区分边界。

func mustFingerprint(t *testing.T, n *config.Node) string {
	t.Helper()
	fp, err := nodeFingerprint(n)
	if err != nil {
		t.Fatalf("nodeFingerprint(%q): %v", n.Name, err)
	}
	return fp
}

func TestNodeFingerprintIgnoresDisplayName(t *testing.T) {
	a := &config.Node{Name: "old remark", Protocol: "Shadowsocks", Server: "Node.Example", Port: 443,
		Metadata: map[string]any{"method": "aes-256-gcm", "password": "pw"}}
	b := &config.Node{Name: "new remark", Protocol: "shadowsocks", Server: "node.example", Port: 443,
		Metadata: map[string]any{"method": "aes-256-gcm", "password": "pw"}}
	if mustFingerprint(t, a) != mustFingerprint(t, b) {
		t.Fatal("仅展示名/大小写/首尾空白不同不应改变身份指纹")
	}
}

func TestNodeFingerprintSeparatesCredentials(t *testing.T) {
	vless := func(uuid string) *config.Node {
		return &config.Node{Name: "v", Protocol: "vless", Server: "example.com", Port: 443, TLS: true,
			Metadata: map[string]any{"uuid": uuid, "sni": "example.com"}}
	}
	trojan := func(password string) *config.Node {
		return &config.Node{Name: "t", Protocol: "trojan", Server: "example.com", Port: 443, TLS: true,
			Metadata: map[string]any{"password": password, "sni": "example.com"}}
	}
	ss := func(password string) *config.Node {
		return &config.Node{Name: "s", Protocol: "shadowsocks", Server: "example.com", Port: 443,
			Metadata: map[string]any{"method": "aes-256-gcm", "password": password}}
	}
	cases := []struct {
		name string
		a, b *config.Node
	}{
		{"vless uuid", vless("uuid-a"), vless("uuid-b")},
		{"trojan password", trojan("pw-a"), trojan("pw-b")},
		{"shadowsocks password", ss("pw-a"), ss("pw-b")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if mustFingerprint(t, c.a) == mustFingerprint(t, c.b) {
				t.Fatalf("同 endpoint 不同凭据必须产生不同指纹（%s）", c.name)
			}
		})
	}
}

func TestNodeFingerprintIncludesTransportSemantics(t *testing.T) {
	withTransport := func(transport, sni, path string) *config.Node {
		md := map[string]any{"uuid": "uuid-a", "sni": sni}
		if path != "" {
			md["path"] = path
		}
		return &config.Node{Name: "n", Protocol: "vless", Server: "example.com", Port: 443, TLS: true,
			Transport: transport, Metadata: md}
	}
	cases := []struct {
		name string
		a, b *config.Node
	}{
		{"ws path", withTransport("ws", "example.com", "/a"), withTransport("ws", "example.com", "/b")},
		{"tls sni", withTransport("", "a.example", ""), withTransport("", "b.example", "")},
		{"是否为 ws", withTransport("ws", "example.com", "/a"), withTransport("", "example.com", "")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if mustFingerprint(t, c.a) == mustFingerprint(t, c.b) {
				t.Fatalf("传输/TLS 语义不同必须产生不同指纹（%s）", c.name)
			}
		})
	}
}

// 指纹算不出来（节点本身不合法）必须返回错误，让订阅刷新在写库前失败。
// 静默降级成低精度身份正是本条目要修的故障，所以这里断言的是「有错」，
// 而不是「返回了某个兜底值」。
func TestNodeFingerprintRejectsInvalidNode(t *testing.T) {
	if _, err := nodeFingerprint(nil); err == nil {
		t.Error("nil 节点应报错")
	}
	bad := &config.Node{Name: "缺凭据", Protocol: "vless", Server: "example.com", Port: 443}
	if _, err := nodeFingerprint(bad); err == nil {
		t.Error("vless 缺 uuid 应报错，不得降级")
	}
	badPort := &config.Node{Name: "端口非法", Protocol: "trojan", Server: "example.com", Port: 0,
		Metadata: map[string]any{"password": "pw"}}
	if _, err := nodeFingerprint(badPort); err == nil {
		t.Error("端口非法应报错，不得降级")
	}
}

// 指纹不得把凭据写进返回值以外的任何地方：这里只断言返回的是定长 hex，
// 从而保证它可以直接作为 map key 与日志字段而不泄露 UUID/密码。
func TestNodeFingerprintReturnsOpaqueDigest(t *testing.T) {
	n := &config.Node{Name: "n", Protocol: "vless", Server: "example.com", Port: 443, TLS: true,
		Metadata: map[string]any{"uuid": "SECRET-UUID-VALUE"}}
	fp := mustFingerprint(t, n)
	if len(fp) != 64 {
		t.Fatalf("指纹应为 64 位 hex，得到 %d 位: %q", len(fp), fp)
	}
	if strings.Contains(fp, "SECRET") || strings.Contains(fp, "example") {
		t.Fatalf("指纹不得包含原文片段: %q", fp)
	}
}
