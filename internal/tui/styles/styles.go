// Package styles 统一管理 TUI 的 lipgloss 样式。
package styles

import "github.com/charmbracelet/lipgloss"

var (
	// Title 是页面标题栏。
	Title = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39")).Padding(0, 1)

	// StatusBar 是底部状态栏。
	StatusBar = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("236")).Padding(0, 1)

	// TabActive / TabInactive 是页面切换标签。
	TabActive   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("39")).Padding(0, 1)
	TabInactive = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Padding(0, 1)

	// Ok / Error / Dim / Accent 是语义色。
	Ok     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	Err    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	Dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	Accent = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))

	// Selected 是列表选中项。
	Selected = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("237"))

	// Box 是内容面板边框。
	Box = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("238")).Padding(0, 1)

	// HelpOverlay 是帮助弹窗。
	HelpOverlay = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("39")).
			Padding(1, 2).
			Background(lipgloss.Color("235"))
)
