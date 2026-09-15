package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	"github.com/bbbstyyy/karing-tui/internal/tui/pages"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func rootFixture(t *testing.T) (RootModel, *application.App) {
	t.Helper()
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	app, err := application.New(paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	m := NewRoot(app)
	model, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return model.(RootModel), app
}

func send(m *RootModel, msg tea.Msg) tea.Cmd {
	next, cmd := m.Update(msg)
	*m = next.(RootModel)
	return cmd
}
func runeKey(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestRootIsolatesGroupAndBackupInput(t *testing.T) {
	m, app := rootFixture(t)
	send(&m, runeKey("3"))
	send(&m, runeKey("a"))
	for _, r := range "组1?q" {
		send(&m, runeKey(string(r)))
	}
	if m.current != 2 || m.quitting || m.showHelp {
		t.Fatal("group text activated a global shortcut")
	}
	send(&m, tea.KeyMsg{Type: tea.KeyCtrlS})
	groups, err := app.DB.ListProxyGroups()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range groups {
		if g.Name == "组1?q" {
			found = true
		}
	}
	if !found {
		t.Fatal("typed group name was not saved intact")
	}
	send(&m, runeKey("q")) // closes member picker
	if m.quitting {
		t.Fatal("q in member picker quit the app")
	}
	send(&m, runeKey("7"))
	send(&m, runeKey("i"))
	for _, r := range "/tmp/q1?backup.zip" {
		send(&m, runeKey(string(r)))
	}
	if m.current != 6 || m.quitting || m.showHelp {
		t.Fatal("backup path activated a global shortcut")
	}
	send(&m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if !m.confirm.Active || m.quitting {
		t.Fatal("unsaved input was not included in exit confirmation")
	}
	cmd := send(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		send(&m, cmd())
	}
	if m.quitting {
		t.Fatal("default exit confirmation did not cancel")
	}
}

func TestRootKeepsFooterAndOverlaysInsideTerminal(t *testing.T) {
	m, app := rootFixture(t)
	_, err := app.Subs.Add(context.Background(), "fixture", "https://example.invalid/sub", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range [][2]int{{80, 24}, {120, 30}, {160, 45}, {60, 18}, {80, 24}} {
		send(&m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for page := 1; page <= 7; page++ {
			send(&m, runeKey(string(rune('0'+page))))
			for _, overlay := range []string{"", "help", "confirm"} {
				m.showHelp = overlay == "help"
				m.confirm = components.Confirm{}
				if overlay == "confirm" {
					m.confirm = components.NewConfirm("test", strings.Repeat("确认目标与影响", 100))
				}
				view := m.View()
				lines := strings.Split(view, "\n")
				if len(lines) != size[1] {
					t.Fatalf("page %d %s: height %d want %d", page, overlay, len(lines), size[1])
				}
				for _, line := range lines {
					if ansi.StringWidth(line) > size[0] {
						t.Fatalf("page %d %s exceeds width: %q", page, overlay, line)
					}
				}
				if !strings.Contains(ansi.Strip(lines[len(lines)-1]), "帮助") {
					t.Fatal("footer is not fixed to the bottom")
				}
			}
			m.showHelp = false
			m.confirm = components.Confirm{}
		}
	}
}

type messagePage struct{ received []tea.Msg }

func (*messagePage) Title() string { return "test" }
func (*messagePage) Init() tea.Cmd { return nil }
func (p *messagePage) Update(msg tea.Msg) (pages.Page, tea.Cmd) {
	p.received = append(p.received, msg)
	return p, nil
}
func (*messagePage) View() string  { return "" }
func (*messagePage) Editing() bool { return false }

func TestRootRoutesCompletionToOriginIncludingNestedBatch(t *testing.T) {
	a, b := &messagePage{}, &messagePage{}
	m := RootModel{pages: []pages.Page{a, b}, current: 1}
	cmd := route(0, tea.Batch(func() tea.Msg { return "success" }, func() tea.Msg { return "failure" }))
	batch := cmd().(tea.BatchMsg)
	for _, child := range batch {
		send(&m, child())
	}
	if len(a.received) != 2 || len(b.received) != 0 {
		t.Fatal("background results were delivered to the visible page")
	}
}

func TestActionMenuOwnsKeysAndDisabledActionDoesNotRun(t *testing.T) {
	m, _ := rootFixture(t)
	send(&m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if !m.showActions {
		t.Fatal("operation menu is not discoverable via Ctrl+O")
	}
	send(&m, runeKey("7"))
	if m.current != 0 {
		t.Fatal("menu leaked a page shortcut")
	}
	for i, a := range m.actions {
		if a.Key == "x" {
			m.actionList.Cursor = i
		}
	}
	if cmd := send(&m, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || !m.showActions {
		t.Fatal("disabled action was executed")
	}
	view := m.View()
	if !strings.Contains(view, "核心未运行") {
		t.Fatal("disabled action has no explanation")
	}
	send(&m, runeKey("q"))
	if m.showActions || m.quitting {
		t.Fatal("q did not close only the menu")
	}
}
