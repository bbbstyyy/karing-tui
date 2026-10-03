// Package tui implements page routing, input ownership and terminal layout.
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	"github.com/bbbstyyy/karing-tui/internal/tui/keys"
	"github.com/bbbstyyy/karing-tui/internal/tui/pages"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
)

type pageMsg struct {
	Page int
	Msg  tea.Msg
}

// Root owns at most one pending spinner and one pending expiry timer.
// A pending flag is cleared only when that timer's message arrives: tea.Tick
// cannot be cancelled just because a task or its visible feedback has ended.
type spinnerMsg struct{}
type transientMsg struct{}

const spinnerInterval = 200 * time.Millisecond
const transientWindow = 8 * time.Second

func spinnerCmd() tea.Cmd {
	return tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return spinnerMsg{} })
}

func transientCmd(d time.Duration) tea.Cmd {
	return tea.Tick(max(d, 0), func(time.Time) tea.Msg { return transientMsg{} })
}

type RootModel struct {
	app                    *application.App
	pages                  []pages.Page
	current, width, height int
	showHelp, quitting     bool
	help                   components.TextView
	confirm                components.Confirm
	notice                 string
	noticeUntil            time.Time
	showActions            bool
	actions                []pages.Action
	actionList             components.SimpleList
	showPalette            bool
	paletteInput           textinput.Model
	paletteCommands        []paletteCommand
	paletteList            components.SimpleList
	showTasks              bool
	taskList               components.SimpleList
	taskPages              []int
	spinner                bool
	refreshing             bool
	pageTouched            bool
}

func NewRoot(app *application.App) RootModel {
	return RootModel{app: app, pages: []pages.Page{
		pages.NewDashboard(app), pages.NewProfiles(app), pages.NewGroups(app),
		pages.NewRules(app), pages.NewDNS(app), pages.NewLogs(app), pages.NewSettings(app),
	}}
}

// route preserves command ownership, including commands nested inside batches.
func route(page int, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		switch msg := msg.(type) {
		case tea.BatchMsg:
			cmds := make([]tea.Cmd, len(msg))
			for i, c := range msg {
				cmds[i] = route(page, c)
			}
			return tea.Batch(cmds...)()
		case tea.QuitMsg:
			return msg
		default:
			return pageMsg{Page: page, Msg: msg}
		}
	}
}

func (m RootModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	for i, p := range m.pages {
		cmds = append(cmds, route(i, p.Init()))
	}
	cmds = append(cmds, route(0, func() tea.Msg { return pages.ActivateMsg{} }))
	return tea.Batch(cmds...)
}

func (m RootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	switch msg.(type) {
	case spinnerMsg:
		next.spinner = false
	case transientMsg:
		next.refreshing = false
	}
	if next.quitting {
		return next, cmd
	}

	wake, spinner, refreshing := next.wakeup()
	next.spinner, next.refreshing = spinner, refreshing
	// Some feedback windows are first established by the following View.
	// Reserve one conservative cleanup timer only when no expiry is pending.
	// Internal wakeups and resizing do not open a feedback window.
	switch msg.(type) {
	case spinnerMsg, transientMsg, tea.WindowSizeMsg:
	default:
		if next.pageTouched && !next.refreshing {
			next.refreshing = true
			wake = tea.Batch(wake, transientCmd(transientWindow))
		}
	}
	if next.showTasks {
		next.refreshTaskCenter()
	}
	next.pageTouched = false
	return next, tea.Batch(cmd, wake)
}

func (m *RootModel) touch() { m.pageTouched = true }

// switchTo deactivates the old page before activating the new page, so hidden
// pages cannot keep renewing their polling timers.
func (m *RootModel) switchTo(idx int) tea.Cmd {
	if idx < 0 || idx >= len(m.pages) {
		return nil
	}
	var stop tea.Cmd
	if idx != m.current {
		p, cmd := m.pages[m.current].Update(pages.DeactivateMsg{})
		m.pages[m.current] = p
		stop = cmd
	}
	m.current = idx
	m.touch()
	p, start := m.pages[idx].Update(pages.ActivateMsg{})
	m.pages[idx] = p
	return tea.Batch(stop, start)
}

// wakeup schedules missing timers without forgetting timers already in flight.
// Spinner and expiry ownership are independent: task completion must not clear
// an outstanding spinner, and repeated page messages must not duplicate expiry.
func (m RootModel) wakeup() (tea.Cmd, bool, bool) {
	spinner, refreshing := m.spinner, m.refreshing
	var cmds []tea.Cmd
	if m.taskRunning() && !spinner {
		cmds = append(cmds, spinnerCmd())
		spinner = true
	}
	if !refreshing {
		if d, ok := m.deadline(); ok {
			cmds = append(cmds, transientCmd(time.Until(d)))
			refreshing = true
		}
	}
	return tea.Batch(cmds...), spinner, refreshing
}

// taskRunning checks all pages, including background tasks on hidden pages.
func (m RootModel) taskRunning() bool {
	for _, p := range m.pages {
		if task, ok := p.(interface{ TaskStatus() (bool, string) }); ok {
			if running, _ := task.TaskStatus(); running {
				return true
			}
		}
	}
	return false
}

func (m RootModel) deadline() (time.Time, bool) {
	now := time.Now()
	var best time.Time
	consider := func(t time.Time) {
		if !t.After(now) {
			return
		}
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	if m.notice != "" {
		consider(m.noticeUntil)
	}
	for _, p := range m.pages {
		if d, ok := p.(interface{ TransientDeadline() time.Time }); ok {
			consider(d.TransientDeadline())
		}
	}
	return best, !best.IsZero()
}

func (m RootModel) update(msg tea.Msg) (RootModel, tea.Cmd) {
	switch msg.(type) {
	case spinnerMsg:
		return m, nil
	case transientMsg:
		if m.notice != "" && !time.Now().Before(m.noticeUntil) {
			m.notice = ""
		}
		return m, nil
	}
	if nav, ok := msg.(pages.NavigateMsg); ok {
		return m, route(max(0, min(nav.Page, len(m.pages)-1)), m.switchTo(nav.Page))
	}
	if routed, ok := msg.(pageMsg); ok {
		if nav, ok := routed.Msg.(pages.NavigateMsg); ok {
			return m.update(nav)
		}
		if routed.Page < 0 || routed.Page >= len(m.pages) {
			return m, nil
		}
		wasBusy := false
		if task, ok := m.pages[routed.Page].(interface{ TaskStatus() (bool, string) }); ok {
			wasBusy, _ = task.TaskStatus()
		}
		m.touch()
		p, cmd := m.pages[routed.Page].Update(routed.Msg)
		m.pages[routed.Page] = p
		if task, ok := p.(interface{ TaskStatus() (bool, string) }); ok {
			if active, label := task.TaskStatus(); wasBusy && !active {
				m.notice = fmt.Sprintf("%s: %s · %d 查看", p.Title(), label, routed.Page+1)
				m.noticeUntil = time.Now().Add(8 * time.Second)
			}
		}
		return m, route(routed.Page, cmd)
	}
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = size.Width, size.Height
		m.resizeGlobalOverlays()
		var cmds []tea.Cmd
		for i := range m.pages {
			m.touch()
			p, cmd := m.pages[i].Update(tea.WindowSizeMsg{Width: max(1, size.Width-1), Height: max(1, size.Height-3)})
			m.pages[i] = p
			cmds = append(cmds, route(i, cmd))
		}
		return m, tea.Batch(cmds...)
	}
	if result, ok := msg.(components.ConfirmMsg); ok {
		if result.Confirmed {
			switch result.ID {
			case "quit":
				m.quitting = true
				return m, tea.Quit
			case "apply":
				cmd := m.apply()
				return m, cmd
			}
		}
		return m, nil
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		if cmd, handled := m.handlePaletteKey(key); handled {
			return m, cmd
		}
		if cmd, handled := m.handleTaskCenterKey(key); handled {
			return m, cmd
		}
		if m.confirm.Active {
			_, cmd := m.confirm.Update(key)
			return m, cmd
		}
		if m.showActions {
			switch key.String() {
			case keys.Cancel, "q", keys.Menu:
				m.showActions = false
			case keys.Exit:
				cmd := m.requestQuit()
				return m, cmd
			case keys.Enter:
				if m.actionList.Cursor < len(m.actions) {
					action := m.actions[m.actionList.Cursor]
					if action.Disabled == "" {
						m.showActions = false
						return m.update(action.Message())
					}
				}
			default:
				m.actionList.Update(key)
			}
			return m, nil
		}
		if m.showHelp {
			switch key.String() {

			case keys.Help, keys.Cancel, keys.Quit:
				m.showHelp = false
			case keys.Exit:
				cmd := m.requestQuit()
				return m, cmd
			default:
				m.help.Move(key.String(), max(1, m.height-9))
			}
			return m, nil
		}
		if key.String() == keys.Exit {
			cmd := m.requestQuit()
			return m, cmd
		}
		if !m.pages[m.current].Editing() {
			switch key.String() {
			case keys.Palette:
				return m, m.openPalette()
			case keys.Tasks:
				m.openTaskCenter()
				return m, nil
			case keys.Menu:
				m.openActionMenu(nil)
				return m, nil
			case keys.Help:
				m.showHelp = true
				m.help.Offset = 0
				return m, nil
			case keys.Quit:
				if pages.AtTop(m.pages[m.current]) {
					cmd := m.requestQuit()
					return m, cmd
				}
				msg = tea.KeyMsg{Type: tea.KeyEsc}
			case keys.Apply:
				if m.app.Core.IsRunning() {
					m.confirm = components.NewConfirm("apply", "生成并校验最新配置；通过后重启核心，现有连接会中断。校验失败时保持当前核心运行。")
					return m, nil
				}
				cmd := m.apply()
				return m, cmd
			}
			k := key.String()
			idx := m.current
			if len(k) == 1 && k[0] >= '1' && k[0] <= '7' {
				idx = int(k[0] - '1')
			}
			if pages.AtTop(m.pages[m.current]) {
				if k == "left" || k == "h" {
					idx = (idx + len(m.pages) - 1) % len(m.pages)
				}
				if k == "right" || k == "l" {
					idx = (idx + 1) % len(m.pages)
				}
			}
			if idx != m.current {
				return m, route(idx, m.switchTo(idx))
			}
		}
	}
	m.touch()
	p, cmd := m.pages[m.current].Update(msg)
	m.pages[m.current] = p
	return m, route(m.current, cmd)
}

func (m *RootModel) requestQuit() tea.Cmd {
	if m.app.Core.IsRunning() || pages.HasUnsaved(m.pages[m.current]) {
		prompt := "退出应用？"
		if m.app.Core.IsRunning() {
			prompt += "退出会停止 sing-box，代理连接将中断。"
		}
		if pages.HasUnsaved(m.pages[m.current]) {
			prompt += "尚未保存的输入将被放弃。"
		}
		m.confirm = components.NewConfirm("quit", prompt)
		return nil
	}
	m.quitting = true
	return tea.Quit
}

func (m *RootModel) apply() tea.Cmd {
	m.touch()
	p, cmd := m.pages[0].Update(pages.ApplyConfigMsg{})
	m.pages[0] = p
	return route(0, cmd)
}

func (m RootModel) View() string {
	if m.quitting {
		return ""
	}
	w, h := m.width, m.height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	if w < 60 || h < 18 {
		return components.Fit("窗口过小，请调整到至少 60×18；建议 80×24。\nCtrl+C 退出", w, h)
	}
	// Reserve the last cell: a line written at exactly the terminal width makes
	// tmux (<= 3.2) realise the pending wrap and add a blank line, which grows
	// the frame and pushes the fixed footer off screen.
	w--
	content := m.pages[m.current].View()
	if m.showHelp {
		body := strings.Join(pages.Help(m.pages[m.current]), "\n")
		content = styles.HelpOverlay.Width(w - 2).Render("帮助 · " + m.pages[m.current].Title() + "\n" +
			m.help.View(body, w-4, h-9) + "\n↑/↓ PgUp/PgDn 滚动 · ?/q/Esc 关闭")
	}
	if m.showActions {
		content = styles.HelpOverlay.Width(w - 2).Render("操作 · " + m.pages[m.current].Title() + "\n" + m.actionList.View("暂无操作") + "\nEnter 执行 · Esc 返回")
	}
	if m.showPalette {
		content = m.paletteView(w, h)
	}
	if m.showTasks {
		content = m.taskCenterView(w, h)
	}
	if m.confirm.Active {
		m.confirm.Width, m.confirm.Height = w, h-3
		content = m.confirm.View()
	}
	return m.tabs(w) + "\n" + components.Fit(content, w, h-3) + "\n" +
		components.Clip(m.taskLine(), w) + "\n" + m.statusBar(w)
}

func (m RootModel) tabs(width int) string {
	labels := make([]string, len(m.pages))
	for i, p := range m.pages {
		label := fmt.Sprintf("%d %s", i+1, p.Title())
		if i == m.current {
			labels[i] = styles.TabActive.Render("[" + label + "]")
		} else {
			labels[i] = styles.TabInactive.Render(label)
		}
	}
	start, end := m.current, m.current+1
	used := components.DisplayWidth(labels[m.current]) + 4
	for start > 0 && used+components.DisplayWidth(labels[start-1])+1 <= width {
		start--
		used += components.DisplayWidth(labels[start]) + 1
	}
	for end < len(labels) && used+components.DisplayWidth(labels[end])+1 <= width {
		used += components.DisplayWidth(labels[end]) + 1
		end++
	}
	left, right := "", ""
	if start > 0 {
		left = "< "
	}
	if end < len(labels) {
		right = " >"
	}
	return components.Clip(left+strings.Join(labels[start:end], " ")+right, width)
}

func (m RootModel) taskLine() string {
	var active []string
	for _, p := range m.pages {
		if task, ok := p.(interface{ TaskStatus() (bool, string) }); ok {
			if running, label := task.TaskStatus(); running {
				active = append(active, p.Title()+": "+label)
			}
		}
	}
	if len(active) > 0 {
		return styles.Accent.Render(strings.Join(active, " · "))
	}
	if m.notice != "" {
		return m.notice
	}
	return m.app.ConfigStage() + " · Ctrl+A 应用配置"
}

func (m RootModel) statusBar(width int) string {
	state := "已停止"
	if m.app.Core.IsRunning() {
		state = "运行中"
	}
	left := m.pages[m.current].Title() + " · " + state
	right := "1–7 切页 · Ctrl+P 命令 · Ctrl+T 任务 · ? 帮助"
	return styles.StatusBar.Render(components.Pad(left, max(0, width-1-components.DisplayWidth(right))) + " " + right)
}
