package routing

import (
	"errors"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// C14 失败回滚验证：一次 Move/Relocate 由多个独立 DB 写组成，中途失败
// 必须整体回滚 —— group kind、源层/目标层 position 全部保持操作前状态。
// 注入点：Manager.afterPlacement（第 n 次 placement 写成功后调用）。

var errInjected = errors.New("injected placement failure")

type groupPlacement struct {
	Kind     string
	Position int
}

func placementSnapshot(t *testing.T, m *Manager) map[int64]groupPlacement {
	t.Helper()
	groups, err := m.DB.ListRoutingGroups()
	if err != nil {
		t.Fatalf("ListRoutingGroups: %v", err)
	}
	out := map[int64]groupPlacement{}
	for _, g := range groups {
		out[g.ID] = groupPlacement{Kind: g.Kind, Position: g.Position}
	}
	return out
}

func assertPlacementUnchanged(t *testing.T, want, got map[int64]groupPlacement) {
	t.Helper()
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Fatalf("组 %d 消失了（回滚不完整）", id)
		}
		if g != w {
			t.Errorf("组 %d 在回滚后状态改变：want %+v, got %+v（事务未回滚干净）", id, w, g)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("组数量变化：want %d, got %d", len(want), len(got))
	}
}

// seedCustomAndGeositeLayers 造 custom 层 3 组 + geosite 层 2 组，返回按名索引的 id。
func seedCustomAndGeositeLayers(t *testing.T, m *Manager) map[string]int64 {
	t.Helper()
	ids := map[string]int64{}
	for _, n := range []string{"c1", "c2", "c3"} {
		g, err := m.CreateGroup(n, "Auto", []config.Rule{{Type: "domain", Value: n + ".com", Enabled: true}})
		if err != nil {
			t.Fatalf("CreateGroup(%s): %v", n, err)
		}
		ids[n] = g.ID
	}
	for _, n := range []string{"g1", "g2"} {
		value := map[string]string{"g1": "geosite:cn", "g2": "geosite:telegram"}[n]
		g, err := m.CreateGroupIn(n, "Auto", config.KindGeosite, []config.Rule{
			{Type: "rule_set", Value: value, Enabled: true}})
		if err != nil {
			t.Fatalf("CreateGroupIn(%s): %v", n, err)
		}
		ids[n] = g.ID
	}
	return ids
}

// TestMoveGroupToKindRollsBackOnMidwayFailure 跨层移动：目标层第 2 次 placement
// 写失败时，此前已落库的组行更新与第 1 次 placement 写必须一起回滚。
func TestMoveGroupToKindRollsBackOnMidwayFailure(t *testing.T) {
	m := newTestManager(t)
	ids := seedCustomAndGeositeLayers(t, m)
	before := placementSnapshot(t, m)

	// relocate 顺序：整行更新 c1（kind=geosite,pos=0，不经 placement 计数）→
	// 目标层重排：跳过 c1，写 g1(n=1)、g2(n=2) → 在 n=2 注入失败。
	m.afterPlacement = func(n int) error {
		if n == 2 {
			return errInjected
		}
		return nil
	}
	if err := m.MoveGroupToKind(ids["c1"], config.KindGeosite, 0); !errors.Is(err, errInjected) {
		t.Fatalf("期望注入错误透出，得到 %v", err)
	}

	after := placementSnapshot(t, m)
	assertPlacementUnchanged(t, before, after)
}

// TestMoveGroupRollsBackOnMidwayFailure 层内移动：第 1 次 placement 写失败时，
// 对换中的另一侧不得已经落库（否则出现 position 重复的半状态）。
func TestMoveGroupRollsBackOnMidwayFailure(t *testing.T) {
	m := newTestManager(t)
	ids := seedCustomAndGeositeLayers(t, m)
	before := placementSnapshot(t, m)

	// MoveGroup(c2,-1)：[c1,c2,c3] → [c2,c1,c3]，写 c2(pos0, n=1)、c1(pos1, n=2)。
	m.afterPlacement = func(n int) error {
		if n == 1 {
			return errInjected
		}
		return nil
	}
	if err := m.MoveGroup(ids["c2"], -1); !errors.Is(err, errInjected) {
		t.Fatalf("期望注入错误透出，得到 %v", err)
	}

	after := placementSnapshot(t, m)
	assertPlacementUnchanged(t, before, after)
}
