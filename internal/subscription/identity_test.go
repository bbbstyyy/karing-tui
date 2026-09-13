package subscription

import (
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

func TestNodeKeyIgnoresDisplayName(t *testing.T) {
	a := &config.Node{Name: "old remark", Protocol: "Shadowsocks", Server: "Node.Example", Port: 443}
	b := &config.Node{Name: "new remark", Protocol: "shadowsocks", Server: "node.example", Port: 443}
	if nodeKey(a) != nodeKey(b) {
		t.Fatalf("同一节点备注变化不应改变身份键: %q != %q", nodeKey(a), nodeKey(b))
	}
}
