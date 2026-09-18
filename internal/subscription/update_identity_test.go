package subscription

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// --- 夹具 ---

func newIdentityTestManager(t *testing.T) (*Manager, *storage.DB) {
	t.Helper()
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewManager(db, nil, nil), db
}

func mustCreateIdentitySubscription(t *testing.T, db *storage.DB) *config.Subscription {
	t.Helper()
	s := &config.Subscription{Name: "身份订阅", URL: "https://example.invalid/sub", Enabled: true}
	if err := db.CreateSubscription(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// vlessNode 造一个「同 endpoint、凭据可区分」的 vless 节点行。
// 只有 uuid 不同，因此两者的「连接语义指纹」必须不同。
func vlessNode(subID int64, name, uuid string) *config.Node {
	return &config.Node{
		Name: name, Protocol: "vless", Server: "example.com", Port: 443, TLS: true, Enabled: true,
		SubscriptionID: subID,
		Metadata:       map[string]any{"uuid": uuid, "sni": "example.com"},
	}
}

func mustCreateIdentityNode(t *testing.T, db *storage.DB, n *config.Node) *config.Node {
	t.Helper()
	if err := db.CreateNode(n); err != nil {
		t.Fatal(err)
	}
	return n
}

// serveSubscription 起一个返回固定订阅正文的 HTTP 服务。
func serveSubscription(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func vlessLink(uuid, remark string) string {
	return fmt.Sprintf("vless://%s@example.com:443?security=tls&sni=example.com#%s", uuid, remark)
}

func identityNodes(t *testing.T, db *storage.DB, subID int64) []*config.Node {
	t.Helper()
	nodes, err := db.ListNodes(subID)
	if err != nil {
		t.Fatal(err)
	}
	return nodes
}

func nodeByUUID(t *testing.T, db *storage.DB, subID int64, uuid string) *config.Node {
	t.Helper()
	for _, n := range identityNodes(t, db, subID) {
		if got, _ := n.Metadata["uuid"].(string); got == uuid {
			return n
		}
	}
	t.Fatalf("未找到 uuid=%s 的节点: %+v", uuid, identityNodes(t, db, subID))
	return nil
}

// --- 判决性用例 ---

// 同 endpoint、不同 UUID 的两个节点：刷新时交换顺序并改备注，各自必须继承自己的 ID。
//
// 旧实现（nodeKey = protocol|server|port + 单值 map）在此必然失败：
// 单值索引只留下最后写入的旧节点，第二个新节点匹配不到，于是拿到新自增 ID。
func TestUpdatePreservesCorrectIDsWhenEndpointCollides(t *testing.T) {
	m, db := newIdentityTestManager(t)
	s := mustCreateIdentitySubscription(t, db)

	tested := time.Now().Add(-time.Hour).Truncate(time.Second)

	// 旧名称排序 A-old < B-old：旧实现的单值索引最终会留下 B（后写入者覆盖）。
	oldA := mustCreateIdentityNode(t, db, vlessNode(s.ID, "A-old", "uuid-a"))
	oldA.Enabled = false
	if err := db.UpdateNode(oldA); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateNodeLatency(oldA.ID, 111, tested); err != nil {
		t.Fatal(err)
	}
	oldB := mustCreateIdentityNode(t, db, vlessNode(s.ID, "B-old", "uuid-b"))
	if err := db.UpdateNodeLatency(oldB.ID, 222, tested); err != nil {
		t.Fatal(err)
	}
	if oldA.ID == oldB.ID {
		t.Fatalf("夹具无效：两个节点应有不同 ID")
	}

	// 刷新：顺序交换（B 在前）且两个备注都改了。
	s.URL = serveSubscription(t, strings.Join([]string{
		vlessLink("uuid-b", "B-new"),
		vlessLink("uuid-a", "A-new"),
	}, "\n"))
	if err := db.UpdateSubscription(s); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Update(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}

	nodes := identityNodes(t, db, s.ID)
	if len(nodes) != 2 {
		t.Fatalf("刷新后应仍是 2 个节点，得到 %d: %+v", len(nodes), nodes)
	}
	gotA := nodeByUUID(t, db, s.ID, "uuid-a")
	gotB := nodeByUUID(t, db, s.ID, "uuid-b")
	if gotA.ID != oldA.ID || gotA.Enabled || gotA.LatencyMS != 111 {
		t.Errorf("uuid-a 应继承 ID=%d/禁用/延迟111，实得 ID=%d enabled=%v latency=%d",
			oldA.ID, gotA.ID, gotA.Enabled, gotA.LatencyMS)
	}
	if gotB.ID != oldB.ID || !gotB.Enabled || gotB.LatencyMS != 222 {
		t.Errorf("uuid-b 应继承 ID=%d/启用/延迟222，实得 ID=%d enabled=%v latency=%d",
			oldB.ID, gotB.ID, gotB.Enabled, gotB.LatencyMS)
	}
}

// 显式代理组的成员/选中项必须在刷新后仍指向同一个语义节点。
//
// 这是本条目最重要的端到端判决：旧实现下组引用会漂到另一个 UUID
// （或整条引用被当作失效成员清掉）。
func TestUpdateKeepsExplicitGroupMemberOnSameSemanticNode(t *testing.T) {
	m, db := newIdentityTestManager(t)
	s := mustCreateIdentitySubscription(t, db)

	oldA := mustCreateIdentityNode(t, db, vlessNode(s.ID, "A-old", "uuid-a"))
	oldB := mustCreateIdentityNode(t, db, vlessNode(s.ID, "B-old", "uuid-b"))
	if oldA.ID == oldB.ID {
		t.Fatalf("夹具无效：两个节点应有不同 ID")
	}

	group := &config.ProxyGroup{
		Name:     "显式选择组",
		Type:     "select",
		Members:  []config.ProxyGroupMember{{Type: "node", ID: oldB.ID}},
		Selected: fmt.Sprintf("node:%d", oldB.ID),
	}
	if err := db.CreateProxyGroup(group); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProxyGroupSelected(group.ID, group.Selected); err != nil {
		t.Fatal(err)
	}

	// 顺序交换：uuid-a 在前。旧实现里「单值索引留下 B」+「首个新节点吃掉 B 的 ID」，
	// 会让 node:B 落到 uuid-a 上。
	s.URL = serveSubscription(t, strings.Join([]string{
		vlessLink("uuid-a", "A-new"),
		vlessLink("uuid-b", "B-new"),
	}, "\n"))
	if err := db.UpdateSubscription(s); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Update(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetProxyGroup(group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 1 {
		t.Fatalf("组成员应保留 1 条，得到 %+v", got.Members)
	}
	if got.Members[0].ID != oldB.ID {
		t.Errorf("组成员应仍指向 node:%d，实得 node:%d", oldB.ID, got.Members[0].ID)
	}
	member := nodeByID(t, db, s.ID, got.Members[0].ID)
	if uuid, _ := member.Metadata["uuid"].(string); uuid != "uuid-b" {
		t.Errorf("组成员语义漂移：node:%d 现在指向 uuid=%q，应为 uuid-b", member.ID, uuid)
	}
	if got.Selected != fmt.Sprintf("node:%d", oldB.ID) {
		t.Errorf("selected 应仍为 node:%d，实得 %q", oldB.ID, got.Selected)
	}
}

func nodeByID(t *testing.T, db *storage.DB, subID, id int64) *config.Node {
	t.Helper()
	for _, n := range identityNodes(t, db, subID) {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("未找到 ID=%d 的节点（订阅 %d）", id, subID)
	return nil
}

// 连接语义完全一致的两个节点：两个旧 ID 都必须被消费且各用一次。
//
// 旧实现下单值索引只留一个旧节点，第二个必定拿到新自增 ID。
func TestUpdateDuplicateIdenticalNodesUsesEachOldIDOnce(t *testing.T) {
	m, db := newIdentityTestManager(t)
	s := mustCreateIdentitySubscription(t, db)

	dup1 := mustCreateIdentityNode(t, db, vlessNode(s.ID, "dup-1", "uuid-same"))
	dup2 := mustCreateIdentityNode(t, db, vlessNode(s.ID, "dup-2", "uuid-same"))

	s.URL = serveSubscription(t, strings.Join([]string{
		vlessLink("uuid-same", "dup-1"),
		vlessLink("uuid-same", "dup-2"),
	}, "\n"))
	if err := db.UpdateSubscription(s); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Update(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}

	nodes := identityNodes(t, db, s.ID)
	if len(nodes) != 2 {
		t.Fatalf("完全相同的两个节点刷新后应仍是 2 行，得到 %d", len(nodes))
	}
	ids := map[int64]int{}
	for _, n := range nodes {
		ids[n.ID]++
	}
	for _, want := range []int64{dup1.ID, dup2.ID} {
		if ids[want] == 0 {
			t.Errorf("旧 ID %d 未被继承，实得 ID 集合 %v", want, ids)
		}
	}
	for id, count := range ids {
		if count != 1 {
			t.Errorf("ID %d 出现 %d 次，每个 ID 只应出现一次", id, count)
		}
	}
}
