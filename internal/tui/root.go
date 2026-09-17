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

// 下面两条消息替换了原先每 200ms 自我续期的全局 frame tick。旧实现无论
// 状态是否变化都会走一遍 Update → View，空闲态约 8 次/秒（叠加 Dashboard
// 与 Logs 各自的 timer），并且每次都重渲当前可见页。新实现只由状态驱动：
//
//	spinnerMsg   — 仅当有任务在跑时按 200ms 续期，驱动进度计数与 spinner；
//	transientMsg — 仅当仍有过期倒计时中的瞬时文案时唤醒一次，用于收尾重绘。
//
// 两者都不再无条件续期：没有任务、也没有待过期文案时，Root 不持有任何定时器。

// spinnerMsg 是任务进行中的进度重绘节拍。
type spinnerMsg struct{}

// transientMsg 是展示窗口到期后的收尾重绘。
type transientMsg struct{}

// spinnerInterval 与旧的 frame tick 保持一致的节拍，使任务进行中的
// 进度显示（spinner 帧、N/M 计数）与改动前完全同步。
const spinnerInterval = 200 * time.Millisecond

// transientWindow 是页面瞬时文案的展示上限（notice / 成功结果 / 操作反馈）。
//
// Root 在 Update 阶段看不到新开的窗口：反馈窗口要等页面的 View 渲染时
// 才由 feedback() 记录起点。因此这里按上限保守地排一次收尾重绘；到期时
// 若窗口仍未结束（通常是几毫秒的渲染延迟），会按实际剩余时间再续一次。
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
	// spinner 标记任务进度节拍是否在途，refreshing 标记收尾重绘是否在途。
	// 二者的唯一作用是防止同一种唤醒被重复排定（例如一次按键与一次在途
	// 定时器叠加，会把节拍翻倍）。
	spinner    bool
	refreshing bool
	// pageTouched 记录本次消息是否真的进了页面，用于决定要不要排收尾重绘。
	pageTouched bool
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

// Update 是对 update 的薄包装：合并本次需要排定的唤醒命令。
//
// 只有「消息确实进了某个页面」（见 touch）时才排一次收尾重绘：覆盖层
// （帮助/确认/操作菜单）自己消费按键，不会开启页面展示窗口，因此不排期。
// 这同时让 Update 保持「返回 nil 命令 = 什么都没做」的既有语义。
func (m RootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	switch msg.(type) {
	case spinnerMsg:
		// 本次节拍已到达，交给下面的 wakeup 按任务状态决定是否续期。
		next.spinner = false
	case transientMsg:
		next.refreshing = false
	default:
		// 页面可能刚刚开启一个展示窗口，但 Root 在 Update 阶段还看不到它
		// （反馈窗口要等页面 View 时才开始计时），因此按窗口上限保守排一次。
		if next.pageTouched && !next.refreshing {
			next.refreshing = true
			cmd = tea.Batch(cmd, transientCmd(transientWindow))
		}
	}
	next.pageTouched = false
	wake, spinner, refreshing := next.wakeup()
	next.spinner, next.refreshing = spinner, refreshing
	return next, tea.Batch(cmd, wake)
}

// touch 记录本次消息确实进入了页面。收尾重绘只在这种情况下才排定，
// 见 Update 的说明。
func (m *RootModel) touch() { m.pageTouched = true }

// switchTo 把可见页切到 idx：先向旧页发 DeactivateMsg（带 timer 的页面据此
// 停止续期），再向新页发 ActivateMsg（页面据此刷新数据并启动新一代 tick）。
//
// 所有切页入口都必须走这里——快捷键、NavigateMsg 与后续新增的入口；
// 只发 ActivateMsg 会让旧页的 timer 继续在后台空转。
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

// wakeup 按当前状态决定是否排定一个一次性唤醒。三个布尔返回值的含义：
// 唤醒命令、spinner 定时器是否在途、收尾重绘是否在途。
func (m RootModel) wakeup() (tea.Cmd, bool, bool) {
	if m.taskRunning() {
		// 任务进行中：进度计数与 spinner 需要持续重绘（与改动前同节拍）。
		if m.spinner {
			return nil, true, m.refreshing
		}
		return spinnerCmd(), true, m.refreshing
	}
	if !m.refreshing {
		return nil, false, false
	}
	d, ok := m.deadline()
	if !ok {
		return nil, false, false
	}
	return transientCmd(time.Until(d)), false, true
}

// taskRunning 报告是否有任意页面正在执行任务。Root 的底部任务行与各页
// 的进度文案都要靠它决定是否需要 spinner 节拍，因此检查全部页面而不只是当前页。
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

// deadline 返回最早仍会到期的展示窗口；没有任何窗口待过期时 ok 为 false。
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
	if _, ok := msg.(transientMsg); ok {
		// 收尾重绘到达时顺手清掉已过期的通知，避免它一直占用底部行。
		if m.notice != "" && !time.Now().Before(m.noticeUntil) {
			m.notice = ""
		}
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
	right := "1–7 切页 · Ctrl+O 操作 · ? 帮助"
	return styles.StatusBar.Render(components.Pad(left, max(0, width-1-components.DisplayWidth(right))) + " " + right)
}
