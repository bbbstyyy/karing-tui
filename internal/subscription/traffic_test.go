package subscription

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// V7-10：订阅刷新写 traffic 元数据失败时必须让整次刷新失败并回滚。
//
// 旧实现是 `_ = m.DB.UpdateSubscriptionTraffic(...)`：错误被吞掉，界面上报「更新成功」，
// 而流量/到期时间仍是旧值——用户无从察觉上游已经续费或改了额度。

// serveSubscriptionWithUserInfo 返回带 subscription-userinfo 头的订阅正文。
func serveSubscriptionWithUserInfo(t *testing.T, body, userInfo string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("subscription-userinfo", userInfo)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestUpdatePropagatesTrafficWriteFailure(t *testing.T) {
	m, db := newIdentityTestManager(t)
	s := mustCreateIdentitySubscription(t, db)
	mustCreateIdentityNode(t, db, vlessNode(s.ID, "old-name", "uuid-a"))

	// storage.DB 不暴露执行任意 SQL 的入口；trigger 是 schema 对象，
	// 用另一条连接建好后对 storage 的写入同样生效。
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER inject_traffic BEFORE UPDATE ON subscriptions
		WHEN NEW.traffic_total <> OLD.traffic_total BEGIN SELECT RAISE(ABORT, 'injected'); END;`); err != nil {
		t.Fatal(err)
	}

	// 节点备注也改了：回滚是否彻底可以同时通过节点名观察。
	s.URL = serveSubscriptionWithUserInfo(t, vlessLink("uuid-a", "new-name"), "upload=1; download=2; total=999; expire=1700000000")
	if err := db.UpdateSubscription(s); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Update(context.Background(), s.ID); err == nil {
		t.Fatal("traffic 写库失败时刷新必须报错，而不是吞掉错误假装成功")
	}

	// 整体回滚：节点池与订阅状态都不能留下「更新了一半」的痕迹。
	nodes := identityNodes(t, db, s.ID)
	if len(nodes) != 1 || nodes[0].Name != "old-name" {
		t.Errorf("刷新失败后节点池应保持原样，实得 %+v", nodes)
	}
	after, err := db.GetSubscription(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.TrafficTotal != 0 {
		t.Errorf("刷新失败后流量信息不应被写入，实得 total=%d", after.TrafficTotal)
	}
	if !after.LastUpdated.Equal(s.LastUpdated) {
		t.Errorf("刷新失败后 last_updated 不应变化：%v -> %v", s.LastUpdated, after.LastUpdated)
	}
}
