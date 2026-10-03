package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestCommandPaletteSearchesAcrossPagesAndExecutes(t *testing.T) {
	m, _ := rootFixture(t)
	send(&m, tea.KeyMsg{Type: tea.KeyCtrlP})
	if !m.showPalette {
		t.Fatal("Ctrl+P did not open command palette")
	}
	for _, r := range "添加订阅" {
		send(&m, runeKey(string(r)))
	}
	if len(m.paletteCommands) == 0 {
		t.Fatal("command palette search returned no results")
	}
	if !strings.Contains(m.paletteCommands[0].Label, "订阅与节点") {
		t.Fatalf("unexpected first palette result: %q", m.paletteCommands[0].Label)
	}
	send(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.showPalette {
		t.Fatal("command palette did not close after execution")
	}
	if m.current != 1 {
		t.Fatalf("palette command did not navigate to profiles page: %d", m.current)
	}
	if !m.pages[m.current].Editing() {
		t.Fatal("palette command did not execute add subscription action")
	}
}

func TestCommandPaletteOwnsGlobalShortcuts(t *testing.T) {
	m, _ := rootFixture(t)
	send(&m, tea.KeyMsg{Type: tea.KeyCtrlP})
	send(&m, runeKey("7"))
	if m.current != 0 {
		t.Fatal("page shortcut leaked through command palette")
	}
	if got := m.paletteInput.Value(); got != "7" {
		t.Fatalf("palette did not receive typed shortcut, got %q", got)
	}
	send(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.showPalette {
		t.Fatal("Esc did not close command palette")
	}
}

func TestTaskCenterListsPagesAndNavigates(t *testing.T) {
	m, _ := rootFixture(t)
	send(&m, tea.KeyMsg{Type: tea.KeyCtrlT})
	if !m.showTasks {
		t.Fatal("Ctrl+T did not open task center")
	}
	if len(m.taskPages) != len(m.pages) {
		t.Fatalf("task center has %d pages, want %d", len(m.taskPages), len(m.pages))
	}
	send(&m, tea.KeyMsg{Type: tea.KeyDown})
	send(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.showTasks {
		t.Fatal("task center did not close after navigation")
	}
	if m.current != 1 {
		t.Fatalf("task center navigated to page %d, want 1", m.current)
	}
}

func TestDashboardShowsSetupChecklist(t *testing.T) {
	m, _ := rootFixture(t)
	view := m.View()
	for _, want := range []string{"快速设置:", "准备可用节点", "配置代理组", "生成并校验配置", "启动代理核心"} {
		if !strings.Contains(view, want) {
			t.Fatalf("dashboard is missing setup checklist text %q", want)
		}
	}
}

func TestRootFooterAdvertisesNewGlobalTools(t *testing.T) {
	m, _ := rootFixture(t)
	view := m.View()
	if !strings.Contains(view, "Ctrl+P 命令") || !strings.Contains(view, "Ctrl+T 任务") {
		t.Fatal("footer does not advertise command palette and task center")
	}
}
