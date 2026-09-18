package proxy

import (
	"context"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// V7-7：全部节点被 skip 时也必须持久化失败结果。
//
// 旧实现里 testNodes 在 `len(plan.Targets) == 0` 时 `return nil, err`，把已经
// 收集到的 `节点ID → -1` 丢掉；TestLatencyProgress 又只在 err == nil 时才写库，
// 于是 last_tested 永远为空。而 AutoClean（DeleteFailedNodesBySubscription）的
// 判据是 `last_tested IS NOT NULL AND latency_ms < 0`，结果就是「每个节点都失败了，
// 清理却报告 0 个」——用户看到自相矛盾的界面。

func TestAllSkippedNodesArePersistedAsFailed(t *testing.T) {
	m := newTestManager(t)
	m.LoadDNS = func() (config.DNSConfig, error) { return config.DNSConfig{}, nil }

	sub := &config.Subscription{Name: "全不可测", URL: "https://example.invalid"}
	if err := m.DB.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	// SSR 在 config.NodeToOutbound 里明确「不支持生成出站」，必然进 Skipped。
	node := &config.Node{Name: "ssr-node", Protocol: "ssr", Server: "1.2.3.4", Port: 443,
		Enabled: true, SubscriptionID: sub.ID, Metadata: map[string]any{"password": "p"}}
	if err := m.DB.CreateNode(node); err != nil {
		t.Fatal(err)
	}

	if _, err := m.TestLatencyProgress(context.Background(), []int64{node.ID}, "", 0, nil); err == nil {
		t.Fatal("全部节点被 skip 时应返回错误")
	}

	got, err := m.DB.GetNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastTested.IsZero() {
		t.Error("全部节点被 skip 后仍必须写入 last_tested，否则 AutoClean 永远删不掉它们")
	}
	if got.LatencyMS != -1 {
		t.Errorf("失败节点应记为 -1，实际 %d", got.LatencyMS)
	}

	removed, err := m.DB.DeleteFailedNodesBySubscription(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("AutoClean 应能删除这个必然失败的节点，实际删除 %d 个", removed)
	}
}
