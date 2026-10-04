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
		{ID: "a", Upload: 100, Download: 200, Chains: []string{"node-1", "AI", "Auto", "AI"}},
		{ID: "b", Upload: 30, Download: 40, Chains: []string{"node-2", "Auto"}},
	})
	if got := d.groupTraffic["AI"]; got != (groupTrafficStat{Upload: 100, Download: 200, Connections: 1}) {
		t.Fatalf("AI first sample = %+v", got)
	}
	if got := d.groupTraffic["Auto"]; got != (groupTrafficStat{Upload: 130, Download: 240, Connections: 2}) {
		t.Fatalf("Auto first sample = %+v", got)
	}
	if got := d.groupTraffic["Unused"]; got != (groupTrafficStat{}) {
		t.Fatalf("Unused first sample = %+v", got)
	}

	// Existing connections contribute only the delta; a new connection contributes
	// its current counters. The vanished connection keeps its accumulated bytes but
	// is no longer counted as active.
	d.updateGroupTraffic(instance, []clashapi.TrafficConnection{
		{ID: "a", Upload: 150, Download: 260, Chains: []string{"node-1", "AI", "Auto"}},
		{ID: "c", Upload: 10, Download: 20, Chains: []string{"node-3", "AI"}},
	})
	if got := d.groupTraffic["AI"]; got != (groupTrafficStat{Upload: 160, Download: 280, Connections: 2}) {
		t.Fatalf("AI second sample = %+v", got)
	}
	if got := d.groupTraffic["Auto"]; got != (groupTrafficStat{Upload: 180, Download: 300, Connections: 1}) {
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
