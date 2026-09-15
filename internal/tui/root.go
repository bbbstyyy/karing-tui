// Package tui implements page routing, input ownership and terminal layout.
package tui

import (
	"fmt"
	"github.com/bbbstyyy/karing-tui/internal/tui/keys"
	"strings"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	"github.com/bbbstyyy/karing-tui/internal/tui/pages"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
	tea "github.com/charmbracelet/bubbletea"
)

type pageMsg struct {
	Page int
	Msg  tea.Msg
}
type frameMsg time.Time

func frame() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg { return frameMsg(t) })
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
	cmds := []tea.Cmd{frame()}
	for i, p := range m.pages {
		cmds = append(cmds, route(i, p.Init()))
	}
	cmds = append(cmds, route(0, func() tea.Msg { return pages.ActivateMsg{} }))
	return tea.Batch(cmds...)
}

func (m RootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(frameMsg); ok {
		return m, frame()
	}
	if nav, ok := msg.(pages.NavigateMsg); ok {
		if nav.Page >= 0 && nav.Page < len(m.pages) {
			m.current = nav.Page
			p, cmd := m.pages[m.current].Update(pages.ActivateMsg{})
			m.pages[m.current] = p
			return m, route(m.current, cmd)
		}
		return m, nil
	}
	if routed, ok := msg.(pageMsg); ok {
		if nav, ok := routed.Msg.(pages.NavigateMsg); ok {
			return m.Update(nav)
		}
		if routed.Page < 0 || routed.Page >= len(m.pages) {
			return m, nil
		}
		wasBusy := false
		if task, ok := m.pages[routed.Page].(interface{ TaskStatus() (bool, string) }); ok {
			wasBusy, _ = task.TaskStatus()
		}
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
		var cmds []tea.Cmd
		for i := range m.pages {
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
						return m.Update(action.Message())
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
			case keys.Menu:
				m.actions = pages.Actions(m.pages[m.current])
				m.actionList = components.SimpleList{}
				for _, a := range m.actions {
					label := a.Key + " · " + a.Label
					if a.Disabled != "" {
						label += "（" + a.Disabled + "）"
					}
					m.actionList.Items = append(m.actionList.Items, label)
				}
				m.showActions = true
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
				m.current = idx
				p, cmd := m.pages[idx].Update(pages.ActivateMsg{})
				m.pages[idx] = p
				return m, route(idx, cmd)
			}
		}
	}
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
		m.actionList.Width, m.actionList.Height = w-4, h-7
		content = styles.HelpOverlay.Width(w - 2).Render("操作 · " + m.pages[m.current].Title() + "\n" + m.actionList.View("暂无操作") + "\nEnter 执行 · Esc 返回")
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
	if time.Now().Before(m.noticeUntil) {
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
	right := "1–7 切页 · Ctrl+O 操作 · ? 帮助"
	return styles.StatusBar.Render(components.Pad(left, max(0, width-1-components.DisplayWidth(right))) + " " + right)
}
