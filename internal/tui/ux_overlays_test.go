package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/tui/pages"
)

func TestCommandPaletteCrossPageActionRevalidatesBeforeExecution(t *testing.T) {
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
		t.Fatal("command palette did not close after cross-page selection")
	}
	if m.current != 1 {
		t.Fatalf("palette command did not navigate to profiles page: %d", m.current)
	}
	if !m.showActions {
		t.Fatal("cross-page action was executed without target-page revalidation")
	}
	if m.actionList.Cursor >= len(m.actions) || m.actions[m.actionList.Cursor].Label != "添加订阅" {
		t.Fatal("target-page action menu did not focus the requested command")
	}
	if m.pages[m.current].Editing() {
		t.Fatal("cross-page action ran before explicit confirmation in fresh context")
	}
	send(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.pages[m.current].Editing() {
		t.Fatal("revalidated add subscription action did not execute")
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

func TestGlobalOverlaysRenderWhenOpened(t *testing.T) {
	m, _ := rootFixture(t)

	send(&m, tea.KeyMsg{Type: tea.KeyCtrlP})
	if view := m.View(); !strings.Contains(view, "命令面板") || !strings.Contains(view, "搜索页面或操作") {
		t.Fatal("command palette state is open but overlay is not rendered")
	}

	send(&m, tea.KeyMsg{Type: tea.KeyEsc})
	send(&m, tea.KeyMsg{Type: tea.KeyCtrlT})
	if view := m.View(); !strings.Contains(view, "任务中心") || !strings.Contains(view, "所有页面的后台任务与最近结果") {
		t.Fatal("task center state is open but overlay is not rendered")
	}
}

func TestCommandPalettePageDownUsesOverlayViewport(t *testing.T) {
	m, _ := rootFixture(t)
	send(&m, tea.KeyMsg{Type: tea.KeyCtrlP})
	if m.paletteList.Height <= 3 {
		t.Fatalf("palette list height was not initialized from terminal: %d", m.paletteList.Height)
	}
	before := m.paletteList.Cursor
	send(&m, tea.KeyMsg{Type: tea.KeyPgDown})
	if m.paletteList.Cursor <= before+1 {
		t.Fatalf("PgDown moved only %d row(s); overlay viewport height was not used", m.paletteList.Cursor-before)
	}
}

func TestGlobalOverlaySizesFollowTerminalResize(t *testing.T) {
	m, _ := rootFixture(t)
	send(&m, tea.KeyMsg{Type: tea.KeyCtrlP})
	oldListHeight := m.paletteList.Height
	oldInputWidth := m.paletteInput.Width

	send(&m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.paletteList.Height <= oldListHeight {
		t.Fatalf("palette height did not grow after resize: %d -> %d", oldListHeight, m.paletteList.Height)
	}
	if m.paletteInput.Width <= oldInputWidth {
		t.Fatalf("palette input width did not grow after resize: %d -> %d", oldInputWidth, m.paletteInput.Width)
	}

	send(&m, tea.KeyMsg{Type: tea.KeyEsc})
	send(&m, tea.KeyMsg{Type: tea.KeyCtrlT})
	if m.taskList.Height != 32 {
		t.Fatalf("task center height = %d, want 32 for 120x40 terminal", m.taskList.Height)
	}
}

type taskLifecyclePage struct {
	activations   int
	deactivations int
	active        bool
}

func (p *taskLifecyclePage) Title() string { return "task-life" }
func (p *taskLifecyclePage) Init() tea.Cmd { return nil }
func (p *taskLifecyclePage) View() string  { return "" }
func (p *taskLifecyclePage) Editing() bool { return false }
func (p *taskLifecyclePage) TaskStatus() (bool, string) {
	return false, "最近任务"
}
func (p *taskLifecyclePage) Update(msg tea.Msg) (pages.Page, tea.Cmd) {
	switch msg.(type) {
	case pages.ActivateMsg:
		p.activations++
		p.active = true
	case pages.DeactivateMsg:
		p.deactivations++
		p.active = false
	}
	return p, nil
}

func TestTaskCenterEnterOnCurrentPageDoesNotReactivate(t *testing.T) {
	p := &taskLifecyclePage{}
	m := RootModel{pages: []pages.Page{p}}
	runCmds(t, &m, m.Init())
	send(&m, tea.WindowSizeMsg{Width: 80, Height: 24})
	if p.activations != 1 {
		t.Fatalf("initial activations = %d, want 1", p.activations)
	}

	send(&m, tea.KeyMsg{Type: tea.KeyCtrlT})
	send(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if p.activations != 1 || p.deactivations != 0 {
		t.Fatalf("same-page task navigation restarted lifecycle: activations=%d deactivations=%d", p.activations, p.deactivations)
	}
}

func TestGlobalOverlaysDoNotStealFormInput(t *testing.T) {
	m, _ := rootFixture(t)
	send(&m, runeKey("2"))
	send(&m, runeKey("a"))
	if !m.pages[m.current].Editing() {
		t.Fatal("fixture did not enter subscription form")
	}

	send(&m, tea.KeyMsg{Type: tea.KeyCtrlP})
	send(&m, tea.KeyMsg{Type: tea.KeyCtrlT})
	if m.showPalette || m.showTasks {
		t.Fatal("global overlays opened while a form owned keyboard input")
	}
}

func TestSetupChecklistRequiresEnabledUsableNode(t *testing.T) {
	m, app := rootFixture(t)
	if err := app.DB.CreateNode(&config.Node{
		Name: "disabled", Protocol: "shadowsocks", Server: "127.0.0.1", Port: 8388,
		Enabled: false, Metadata: map[string]any{"method": "aes-128-gcm", "password": "pw"},
	}); err != nil {
		t.Fatal(err)
	}
	send(&m, pages.ActivateMsg{})
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "[ ] 准备可用节点") {
		t.Fatal("disabled node was incorrectly treated as usable in setup checklist")
	}

	nodes, err := app.DB.ListNodes(0)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("ListNodes: %v / %d", err, len(nodes))
	}
	nodes[0].Enabled = true
	if err := app.DB.UpdateNode(nodes[0]); err != nil {
		t.Fatal(err)
	}
	send(&m, pages.ActivateMsg{})
	view = ansi.Strip(m.View())
	if !strings.Contains(view, "[x] 准备可用节点") {
		t.Fatal("enabled manual node was not recognized by setup checklist")
	}
}

func TestGlobalOverlayViewsStayInsideTerminal(t *testing.T) {
	m, _ := rootFixture(t)
	for _, size := range [][2]int{{60, 18}, {80, 24}, {120, 30}} {
		send(&m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, key := range []tea.KeyType{tea.KeyCtrlP, tea.KeyCtrlT} {
			send(&m, tea.KeyMsg{Type: key})
			view := m.View()
			lines := strings.Split(view, "\n")
			if len(lines) != size[1] {
				t.Fatalf("%dx%d overlay height = %d", size[0], size[1], len(lines))
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > size[0] {
					t.Fatalf("%dx%d overlay exceeds width: %q", size[0], size[1], line)
				}
			}
			send(&m, tea.KeyMsg{Type: tea.KeyEsc})
		}
	}
}

func TestActionMenuPageDownUsesOverlayViewport(t *testing.T) {
	m, _ := rootFixture(t)
	send(&m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if !m.showActions {
		t.Fatal("Ctrl+O did not open action menu")
	}
	if m.actionList.Height <= 3 {
		t.Fatalf("action list height was not initialized from terminal: %d", m.actionList.Height)
	}
	before := m.actionList.Cursor
	send(&m, tea.KeyMsg{Type: tea.KeyPgDown})
	if m.actionList.Cursor <= before+1 {
		t.Fatalf("action PgDown moved only %d row(s); overlay viewport height was not used", m.actionList.Cursor-before)
	}
}

func TestSetupChecklistRespectsSubscriptionEnabledState(t *testing.T) {
	m, app := rootFixture(t)
	sub := &config.Subscription{Name: "fixture-sub", URL: "https://example.invalid/sub"}
	if err := app.DB.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	if err := app.DB.CreateNode(&config.Node{
		SubscriptionID: sub.ID, Name: "sub-node", Protocol: "shadowsocks",
		Server: "127.0.0.1", Port: 8388, Enabled: true,
		Metadata: map[string]any{"method": "aes-128-gcm", "password": "pw"},
	}); err != nil {
		t.Fatal(err)
	}
	sub.Enabled = false
	if err := app.DB.UpdateSubscription(sub); err != nil {
		t.Fatal(err)
	}

	send(&m, pages.ActivateMsg{})
	if view := ansi.Strip(m.View()); !strings.Contains(view, "[ ] 准备可用节点") {
		t.Fatal("node from disabled subscription was incorrectly treated as usable")
	}

	sub.Enabled = true
	if err := app.DB.UpdateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	send(&m, pages.ActivateMsg{})
	if view := ansi.Strip(m.View()); !strings.Contains(view, "[x] 准备可用节点") {
		t.Fatal("enabled subscription node was not recognized as usable")
	}
}

func TestActionMenuRejectsMissingPaletteTarget(t *testing.T) {
	m, _ := rootFixture(t)
	missing := pages.Action{Key: "z", Label: "已失效操作"}
	if m.openActionMenu(&missing) {
		t.Fatal("action menu accepted a palette action that no longer exists")
	}
	if m.showActions {
		t.Fatal("missing palette target fell back to an unrelated action")
	}
}

func TestCommandPaletteCurrentPageActionExecutesDirectly(t *testing.T) {
	m, _ := rootFixture(t)
	send(&m, runeKey("2"))
	if m.current != 1 {
		t.Fatalf("failed to enter profiles page: %d", m.current)
	}

	send(&m, tea.KeyMsg{Type: tea.KeyCtrlP})
	for _, r := range "添加订阅" {
		send(&m, runeKey(string(r)))
	}
	send(&m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.showPalette || m.showActions {
		t.Fatal("same-page palette action should execute directly")
	}
	if !m.pages[m.current].Editing() {
		t.Fatal("same-page palette action did not open subscription form")
	}
}
