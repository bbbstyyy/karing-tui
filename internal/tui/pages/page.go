// Package pages 实现 TUI 的七个页面。
package pages

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bbbstyyy/karing-tui/internal/application"
)

// Page 是所有页面的公共接口。
type Page interface {
	// Title 是页面显示名。
	Title() string
	Init() tea.Cmd
	Update(tea.Msg) (Page, tea.Cmd)
	View() string
	// Editing 报告页面是否处于文本输入（表单/搜索）状态；
	// 为 true 时主框架不拦截全局键位（数字切页、? 帮助），按键直达页面。
	Editing() bool
}

// ActivateMsg 在页面被切换为当前页时发送，页面可借此刷新数据。
type ActivateMsg struct{}

// tickMsg 周期刷新消息。
type tickMsg time.Time

// tickAt 返回一个在 d 之后触发 tickMsg 的命令。
func tickAt(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// actionDoneMsg 异步操作完成消息。
type actionDoneMsg struct {
	Action string
	Data   string // 附加数据（如备份文件路径）
	Err    error
}

// base 是页面公共字段。
type base struct {
	app    *application.App
	width  int
	height int
}

func (b *base) SetSize(width, height int) {
	b.width = width
	b.height = height
}

// Editing 默认不处于编辑状态；有文本输入的页面自行覆盖。
func (b *base) Editing() bool { return false }
