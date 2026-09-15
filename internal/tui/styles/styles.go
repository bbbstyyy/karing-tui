// Package styles 统一管理 TUI 的 lipgloss 样式。
package styles

import "github.com/charmbracelet/lipgloss"

var (
	// Title 是页面标题栏。
	Title = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "25", Dark: "81"})

	// StatusBar 是底部状态栏。
	StatusBar = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "235", Dark: "252"}).Background(lipgloss.AdaptiveColor{Light: "254", Dark: "236"})

	// TabActive / TabInactive 是页面切换标签。
	TabActive   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("25"))
	TabInactive = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "250"})

	// Ok / Error / Dim / Accent 是语义色。
	Ok     = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "78"})
	Err    = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "124", Dark: "203"})
	Dim    = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "250"})
	Accent = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "25", Dark: "81"})

	// Selected 是列表选中项。
	Selected = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "235", Dark: "255"}).Background(lipgloss.AdaptiveColor{Light: "253", Dark: "238"})

	// Box 是内容面板边框。
	Box = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("238")).Padding(0, 1)

	// HelpOverlay 是帮助弹窗。
	HelpOverlay = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.AdaptiveColor{Light: "25", Dark: "81"}).
			Padding(0, 1).
			Foreground(lipgloss.AdaptiveColor{Light: "235", Dark: "255"}).
			Background(lipgloss.AdaptiveColor{Light: "255", Dark: "235"})
)
