package tui

import (
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/tui/pages"
)

func TestRootStabilityLongSession(t *testing.T) {
	m := RootModel{
		pages:       []pages.Page{taskStatusPage{}},
		notice:      "completed",
		noticeUntil: time.Now().Add(time.Hour),
	}
	for i := 0; i < 20000; i++ {
		next, cmd := m.Update(pageMsg{Page: 0, Msg: i})
		m = next.(RootModel)
		if i == 0 && cmd == nil {
			t.Fatal("notice expiry was not scheduled")
		}
		if i > 0 && cmd != nil {
			t.Fatalf("message %d scheduled another timer while one is pending", i)
		}
		if !m.refreshing {
			t.Fatal("lost ownership of the pending expiry timer")
		}
	}
}

func TestRootStabilityPreservesPendingSpinner(t *testing.T) {
	m := RootModel{pages: []pages.Page{taskStatusPage{}}, spinner: true}
	cmd, spinner, refreshing := m.wakeup()
	if cmd != nil || !spinner || refreshing {
		t.Fatal("finishing a task must not forget a spinner that has not arrived")
	}
	m.spinner = spinner
	m.pages[0] = taskStatusPage{active: true}
	if cmd, _, _ := m.wakeup(); cmd != nil {
		t.Fatal("starting the next task duplicated the pending spinner")
	}
}

func TestRootStabilityReschedulesEarlyExpiry(t *testing.T) {
	m := RootModel{
		pages:       []pages.Page{taskStatusPage{}},
		notice:      "newer notice",
		noticeUntil: time.Now().Add(time.Hour),
		refreshing:  true,
	}
	next, cmd := m.Update(transientMsg{})
	m = next.(RootModel)
	if cmd == nil || !m.refreshing {
		t.Fatal("an early timer must schedule the remaining notice lifetime")
	}
	if cmd, _, refreshing := m.wakeup(); cmd != nil || !refreshing {
		t.Fatal("checking the deadline duplicated the replacement timer")
	}
	m.noticeUntil = time.Now().Add(-time.Second)
	next, cmd = m.Update(transientMsg{})
	m = next.(RootModel)
	if cmd != nil || m.refreshing || m.notice != "" {
		t.Fatal("the last expiry must terminate without another wakeup")
	}
}
