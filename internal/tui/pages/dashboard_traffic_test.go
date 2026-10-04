package pages

import (
	"strconv"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/clashapi"
	"github.com/bbbstyyy/karing-tui/internal/config"
)

func trafficProxyGroups(names ...string) map[string]clashapi.ProxyInfo {
	out := make(map[string]clashapi.ProxyInfo, len(names))
	for _, name := range names {
		out[name] = clashapi.ProxyInfo{All: []string{"leaf"}}
	}
	return out
}

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
	}, trafficProxyGroups("Auto", "AI", "Unused"))
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
	}, trafficProxyGroups("Auto", "AI", "Unused"))
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
	}, trafficProxyGroups("Auto", "AI", "Unused"))
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
	}, trafficProxyGroups("Auto"))
	d.updateGroupTraffic(instance, []clashapi.TrafficConnection{
		{ID: "a", Upload: 5, Download: 8, Chains: []string{"Auto"}},
	}, trafficProxyGroups("Auto"))
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

func TestDashboardTrafficUpdateSteadyStateAllocations(t *testing.T) {
	d := &Dashboard{groups: []*config.ProxyGroup{{Name: "Auto"}}}
	instance := time.Now()
	conns := make([]clashapi.TrafficConnection, 512)
	for i := range conns {
		conns[i] = clashapi.TrafficConnection{
			ID:       "conn-" + strconv.Itoa(i),
			Upload:   int64(i + 1),
			Download: int64(i + 2),
			Chains:   []string{"node", "Auto"},
		}
	}
	// 两次预热让交替复用的两张连接基线 map 都拥有足够容量。
	proxies := trafficProxyGroups("Auto")
	d.updateGroupTraffic(instance, conns, proxies)
	d.updateGroupTraffic(instance, conns, proxies)

	if allocs := testing.AllocsPerRun(20, func() {
		d.updateGroupTraffic(instance, conns, proxies)
	}); allocs != 0 {
		t.Fatalf("steady-state traffic update allocates: %.1f allocs/run", allocs)
	}
}

func TestDashboardTrafficRejectsNodeTagMatchingUnappliedGroupName(t *testing.T) {
	d := &Dashboard{groups: []*config.ProxyGroup{{Name: "AI"}}}
	instance := time.Now()
	conns := []clashapi.TrafficConnection{{
		ID: "a", Upload: 100, Download: 200, Chains: []string{"AI"},
	}}

	// 运行核心仍是旧配置：AI 在 /proxies 里只是节点，不带组专有的 all 字段。
	d.updateGroupTraffic(instance, conns, map[string]clashapi.ProxyInfo{
		"AI": {Type: "Trojan"},
	})
	if got := d.groupTraffic["AI"]; got != (groupTrafficStat{}) {
		t.Fatalf("node tag was misclassified as a proxy group: %+v", got)
	}

	// 新配置应用后，同名 tag 成为真实运行时组，才允许开始归属。
	d.updateGroupTraffic(instance, []clashapi.TrafficConnection{{
		ID: "b", Upload: 7, Download: 9, Chains: []string{"AI"},
	}}, trafficProxyGroups("AI"))
	if got := d.groupTraffic["AI"]; got != (groupTrafficStat{Upload: 7, Download: 9, Connections: 1}) {
		t.Fatalf("runtime group tag was not attributed: %+v", got)
	}
}
