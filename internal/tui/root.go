// Package tui 实现主框架：页面路由、全局键位、状态栏与帮助弹窗。
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/core"
	"github.com/bbbstyyy/karing-tui/internal/tui/pages"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
)

// RootModel 是 TUI 根模型。
type RootModel struct {
	app      *application.App
	pages    []pages.Page
	current  int
	width    int
	height   int
	showHelp bool
	quitting bool
}

// NewRoot 创建根模型，装配七个页面。
func NewRoot(app *application.App) RootModel {
	return RootModel{
		app: app,
		pages: []pages.Page{
			pages.NewDashboard(app),
			pages.NewProfiles(app),
			pages.NewGroups(app),
			pages.NewRules(app),
			pages.NewDNS(app),
			pages.NewLogs(app),
			pages.NewSettings(app),
		},
	}
}

// Init 初始化当前页。
func (m RootModel) Init() tea.Cmd {
	return m.pages[m.current].Init()
}

// Update 处理全局键位并转发消息给当前页。
func (m RootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "q":
			// 编辑控件/帮助弹窗保留各自的 q 语义；全局 q 仅在普通页面退出。
			if !m.pages[m.current].Editing() && !m.showHelp {
				m.quitting = true
				return m, tea.Quit
			}
		}
		// 页面处于文本输入状态时，按键直达页面（数字切页/? 帮助不拦截）。
		if !m.pages[m.current].Editing() {
			switch key.String() {
			case "?":
				m.showHelp = !m.showHelp
				return m, nil
			}
			// 帮助弹窗打开时拦截其余按键，Esc 关闭。
			if m.showHelp {
				if key.String() == "esc" {
					m.showHelp = false
				}
				return m, nil
			}
			// 全局页码切换 1-7。
			if len(key.String()) == 1 && key.String()[0] >= '1' && key.String()[0] <= '7' {
				idx := int(key.String()[0] - '1')
				if idx != m.current {
					m.current = idx
					return m, m.sendActivate()
				}
				return m, nil
			}
		}
	}

	// 窗口尺寸消息要同步给根布局与全部页面。
	// 只发给当前页是不够的：非当前页收不到尺寸，其 height 会一直是 0，
	// 切过去后依赖高度的渲染（Logs 回看行数、Rules 分类库视窗）只能走兜底值。
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = size.Width
		m.height = size.Height
		var cmds []tea.Cmd
		for i := range m.pages {
			var cmd tea.Cmd
			m.pages[i], cmd = m.pages[i].Update(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)
	}

	var cmd tea.Cmd
	m.pages[m.current], cmd = m.pages[m.current].Update(msg)
	return m, cmd
}

// sendActivate 通知当前页已激活。
func (m RootModel) sendActivate() tea.Cmd {
	return func() tea.Msg { return pages.ActivateMsg{} }
}

// View 渲染标题栏、页面内容、状态栏与帮助弹窗。
func (m RootModel) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// 标题栏 + 页签
	var tabs strings.Builder
	tabs.WriteString(styles.Title.Render("Karing TUI") + " ")
	for i, p := range m.pages {
		name := fmt.Sprintf("%d %s", i+1, p.Title())
		if i == m.current {
			tabs.WriteString(styles.TabActive.Render(name) + " ")
		} else {
			tabs.WriteString(styles.TabInactive.Render(name) + " ")
		}
	}
	b.WriteString(tabs.String() + "\n")

	// 页面内容（留出页签 1 行 + 状态栏 1 行 + 余量）
	contentHeight := m.height - 3
	content := m.pages[m.current].View()
	if contentHeight > 0 {
		content = trimToHeight(content, contentHeight)
	}
	b.WriteString(content + "\n")

	// 状态栏
	b.WriteString(m.statusBar())

	// 帮助弹窗
	if m.showHelp {
		b.WriteString("\n" + helpView())
	}
	return b.String()
}

func (m RootModel) statusBar() string {
	st := m.app.Core.Status()
	state := styles.Err.Render("停止")
	if st.State == core.StateRunning {
		state = styles.Ok.Render("运行")
	}
	left := fmt.Sprintf(" %s ", m.pages[m.current].Title())
	mid := fmt.Sprintf("sing-box: %s", state)
	if st.Version != "" {
		mid += fmt.Sprintf(" · v%s", st.Version)
	}
	if st.State == core.StateRunning {
		mid += fmt.Sprintf(" · %s", st.Uptime)
	}
	right := " 1-7 切页 · ? 帮助 · q 退出 "

	pad := m.width - len([]rune(stripANSI(left+mid))) - len([]rune(right))
	if pad < 1 {
		pad = 1
	}
	return styles.StatusBar.Render(left + mid + strings.Repeat(" ", pad) + right)
}

func helpView() string {
	lines := []string{
		"全局键位",
		"  1-7      切换页面 (Dashboard / Profiles / Proxy Groups / Rules / DNS / Logs / Settings)",
		"  ?        打开/关闭本帮助",
		"  q / C-c  退出（编辑时使用 C-c）",
		"",
		"Dashboard:  s 启动核心 · x 停止 · r 重启 · g 生成并校验配置 · t 组测速（运行中）",
		"Profiles:   a 添加订阅 · e 编辑 · u 更新 · U 全部更新 · space 启停 · d 删除 · enter 查看节点",
		"列表页:     ↑/↓ 或 j/k 移动 · r 刷新",
		"Logs:       Tab 切换日志来源 · ↑/↓ 回看 · G 跟随",
	}
	return styles.HelpOverlay.Render(strings.Join(lines, "\n"))
}

// stripANSI 去掉 ANSI 转义序列，用于计算显示宽度。
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if r == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// trimToHeight 将多行文本裁剪到最多 h 行。
func trimToHeight(s string, h int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= h {
		return s
	}
	return strings.Join(lines[len(lines)-h:], "\n")
}
