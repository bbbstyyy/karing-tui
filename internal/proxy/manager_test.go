package proxy

import (
	"context"
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

func TestSaveManualValidatesProtocolCredentials(t *testing.T) {
	m := newTestManager(t)
	cases := []*config.Node{
		{Protocol: "vmess", Server: "example.com", Port: 443, Metadata: map[string]any{}},
		{Protocol: "vless", Server: "example.com", Port: 443, Metadata: map[string]any{}},
		{Protocol: "trojan", Server: "example.com", Port: 443, Metadata: map[string]any{}},
		{Protocol: "shadowsocks", Server: "example.com", Port: 443, Metadata: map[string]any{"method": "aes-128-gcm"}},
		{Protocol: "unknown", Server: "example.com", Port: 443, Metadata: map[string]any{}},
	}
	for _, n := range cases {
		if err := m.SaveManual(n); err == nil {
			t.Errorf("无效节点 %q 应被拒绝", n.Protocol)
		}
	}

	valid := &config.Node{Protocol: "vmess", Server: "example.com", Port: 443,
		Metadata: map[string]any{"uuid": "00000000-0000-0000-0000-000000000001"}}
	if err := m.SaveManual(valid); err != nil {
		t.Fatalf("合法 vmess 节点不应失败: %v", err)
	}
}

func TestSaveManualRejectsNil(t *testing.T) {
	if err := newTestManager(t).SaveManual(nil); err == nil {
		t.Fatal("nil 节点应被拒绝")
	}
}

// TestTestNodesRequiresDNSLoader 未注入 LoadDNS 时必须报错。
//
// 静默按「没有 DNS」处理会悄悄退回本次修复前的行为：临时核心不带 dns /
// default_domain_resolver，域名型节点又变成批量假失败，而且完全没有报错。
// 「没有配置 DNS 服务器」是另一回事（LoadDNS 正常返回空 DNSConfig），那种情况允许。
func TestTestNodesRequiresDNSLoader(t *testing.T) {
	m := newTestManager(t) // NewManager 不注入 LoadDNS，模拟漏注入
	node := &config.Node{ID: 1, Name: "n", Protocol: "trojan", Server: "node.example.test", Port: 443,
		Enabled: true, Metadata: map[string]any{"password": "p"}}

	_, err := m.testNodes(context.Background(), []*config.Node{node}, "", 0, nil)
	if err == nil {
		t.Fatal("未注入 LoadDNS 时应立即报错，不能静默退回无 DNS 的测速")
	}
	if !strings.Contains(err.Error(), "LoadDNS") {
		t.Errorf("错误文案应指出缺少 loader: %v", err)
	}
}
