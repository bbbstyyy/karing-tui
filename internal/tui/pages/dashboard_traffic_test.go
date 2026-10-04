package pages

import (
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/clashapi"
	"github.com/bbbstyyy/karing-tui/internal/config"
)

func TestDashboardTracksTrafficPerProxyGroup(t *testing.T) {
	d := &Dashboard{
		groups: []*config.ProxyGroup{
			{Name: "Auto"},
			{Name: "AI"},
			{Name: "Unused"},
		},
	}
	instance := time.Now()

	d.updateGroupTraffic(instance, []clashapi.TrafficConnection{
		// karing-tui 会把嵌套组展开成 leaf 节点，因此正常运行时一条连接只
		// 归属实际出现在 sing-box chain 里的组。重复 Auto 用来钉住防御性去重。
		{ID: "a", Upload: 100, Download: 200, Chains: []string{"node-1", "Auto", "Auto"}},
		{ID: "b", Upload: 30, Download: 40, Chains: []string{"node-2", "AI"}},
	})
	if got := d.groupTraffic["AI"]; got != (groupTrafficStat{Upload: 30, Download: 40, Connections: 1}) {
		t.Fatalf("AI first sample = %+v", got)
	}
	if got := d.groupTraffic["Auto"]; got != (groupTrafficStat{Upload: 100, Download: 200, Connections: 1}) {
		t.Fatalf("Auto first sample = %+v", got)
	}
	if got := d.groupTraffic["Unused"]; got != (groupTrafficStat{}) {
		t.Fatalf("Unused first sample = %+v", got)
	}

	// Existing connections contribute only the delta; a new connection contributes
	// its current counters. The vanished connection keeps its accumulated bytes but
	// is no longer counted as active.
	d.updateGroupTraffic(instance, []clashapi.TrafficConnection{
		{ID: "a", Upload: 150, Download: 260, Chains: []string{"node-1", "Auto"}},
		{ID: "c", Upload: 10, Download: 20, Chains: []string{"node-3", "AI"}},
	})
	if got := d.groupTraffic["AI"]; got != (groupTrafficStat{Upload: 40, Download: 60, Connections: 1}) {
		t.Fatalf("AI second sample = %+v", got)
	}
	if got := d.groupTraffic["Auto"]; got != (groupTrafficStat{Upload: 150, Download: 260, Connections: 1}) {
		t.Fatalf("Auto second sample = %+v", got)
	}

	// A new core instance owns a new set of counters; old traffic must not leak.
	newInstance := instance.Add(time.Second)
	d.updateGroupTraffic(newInstance, []clashapi.TrafficConnection{
		{ID: "z", Upload: 7, Download: 9, Chains: []string{"AI"}},
	})
	if got := d.groupTraffic["AI"]; got != (groupTrafficStat{Upload: 7, Download: 9, Connections: 1}) {
		t.Fatalf("AI after restart = %+v", got)
	}
	if got := d.groupTraffic["Auto"]; got != (groupTrafficStat{}) {
		t.Fatalf("Auto after restart = %+v", got)
	}
}

func TestDashboardTrafficCounterRollbackUsesCurrentValue(t *testing.T) {
	d := &Dashboard{groups: []*config.ProxyGroup{{Name: "Auto"}}}
	instance := time.Now()
	d.updateGroupTraffic(instance, []clashapi.TrafficConnection{
		{ID: "a", Upload: 100, Download: 100, Chains: []string{"Auto"}},
	})
	d.updateGroupTraffic(instance, []clashapi.TrafficConnection{
		{ID: "a", Upload: 5, Download: 8, Chains: []string{"Auto"}},
	})
	if got := d.groupTraffic["Auto"]; got != (groupTrafficStat{Upload: 105, Download: 108, Connections: 1}) {
		t.Fatalf("rollback should start a fresh counter epoch, got %+v", got)
	}
}

func TestDashboardTrafficGroupIndexPrunesRemovedGroups(t *testing.T) {
	d := &Dashboard{
		groups: []*config.ProxyGroup{{Name: "Auto"}, {Name: "Old"}},
		groupTraffic: map[string]groupTrafficStat{
			"Auto": {Upload: 10},
			"Old":  {Upload: 20},
		},
	}
	d.refreshGroupTrafficNames()
	d.groups = []*config.ProxyGroup{{Name: "Auto"}, {Name: "New"}}
	d.refreshGroupTrafficNames()

	if _, ok := d.groupTraffic["Old"]; ok {
		t.Fatal("removed group left stale traffic state behind")
	}
	if _, ok := d.groupTrafficNames["Old"]; ok {
		t.Fatal("removed group left stale name index behind")
	}
	if _, ok := d.groupTrafficNames["New"]; !ok {
		t.Fatal("new group missing from traffic name index")
	}
}
