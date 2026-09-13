package proxy

import (
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
