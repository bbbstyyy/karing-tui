// Package keys defines keyboard actions used by dispatch, menus and help.
package keys

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	Add        = "a"
	Edit       = "e"
	Delete     = "d"
	Refresh    = "r"
	Restart    = "R"
	Test       = "t"
	TestAll    = "T"
	Update     = "u"
	UpdateAll  = "U"
	Results    = "v"
	Retry      = "f"
	Search     = "/"
	Clear      = "c"
	Next       = "]"
	Previous   = "["
	Focus      = "tab"
	FocusBack  = "shift+tab"
	Save       = "ctrl+s"
	SaveUpdate = "ctrl+u"
	Advanced   = "ctrl+g"
	Help       = "?"
	Menu       = "ctrl+o"
	Apply      = "ctrl+a"
	Cancel     = "esc"
	Enter      = "enter"
	Space      = " "
	Quit       = "q"
	Exit       = "ctrl+c"
	Reveal     = "ctrl+r"
	Error      = "ctrl+e"
	Up         = "up"
	Down       = "down"
	PageUp     = "pgup"
	PageDown   = "pgdown"
	First      = "home"
	Last       = "end"
)

// Navigation normalizes list and text-view aliases before dispatch.
func Navigation(key string) string {
	switch key {
	case "k":
		return Up
	case "j":
		return Down
	case "g":
		return First
	case "G":
		return Last
	}
	return key
}

type Binding struct {
	Key, Label string
	Aliases    []string
}

func (b Binding) Matches(key string) bool {
	if key == b.Key {
		return true
	}
	for _, alias := range b.Aliases {
		if key == alias {
			return true
		}
	}
	return false
}

func (b Binding) Hint() string { return Display(b.Key) + " " + b.Label }

func Display(key string) string {
	switch key {
	case " ":
		return "Space"
	case "enter":
		return "Enter"
	case "esc":
		return "Esc"
	case "tab":
		return "Tab"
	case "shift+tab":
		return "Shift+Tab"
	case "alt+enter":
		return "Alt+Enter"
	}
	if strings.HasPrefix(key, "ctrl+") {
		return "Ctrl+" + strings.ToUpper(strings.TrimPrefix(key, "ctrl+"))
	}
	return key
}

func Message(key string) tea.KeyMsg {
	switch strings.ToLower(key) {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "space", " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "ctrl+a":
		return tea.KeyMsg{Type: tea.KeyCtrlA}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "ctrl+u":
		return tea.KeyMsg{Type: tea.KeyCtrlU}
	case "ctrl+g":
		return tea.KeyMsg{Type: tea.KeyCtrlG}
	case "ctrl+r":
		return tea.KeyMsg{Type: tea.KeyCtrlR}
	case "ctrl+e":
		return tea.KeyMsg{Type: tea.KeyCtrlE}
	case "ctrl+o":
		return tea.KeyMsg{Type: tea.KeyCtrlO}
	case "alt+enter":
		return tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
}

var Global = []Binding{
	{Key: "1–7", Label: "直达页面"},
	{Key: "←/→ h/l", Label: "顶层切页"},
	{Key: Focus, Label: "切换列表与详情焦点", Aliases: []string{FocusBack}},
	{Key: "[/]", Label: "切换子页签"},
	{Key: Menu, Label: "当前对象操作菜单"},
	{Key: Apply, Label: "生成、校验并应用配置"},
	{Key: Help, Label: "打开或关闭帮助"},
	{Key: "q/Ctrl+C", Label: "退出；运行中确认停机影响"},
}

var List = []Binding{
	{Key: "↑/↓ j/k", Label: "移动当前行"},
	{Key: "PgUp/PgDn", Label: "翻页"},
	{Key: "Home/End", Label: "首尾"},
	{Key: Enter, Label: "查看详情或执行已聚焦动作"},
	{Key: "alt+enter", Label: "查看完整详情"},
	{Key: Cancel, Label: "返回上一层"},
}

var Form = []Binding{
	{Key: "Tab/Shift+Tab", Label: "切换字段和按钮"},
	{Key: Enter, Label: "选择选项或执行当前按钮"},
	{Key: Space, Label: "切换开关或复选框"},
	{Key: Save, Label: "保存"},
	{Key: SaveUpdate, Label: "订阅保存并更新"},
	{Key: Advanced, Label: "展开或收起高级选项"},
	{Key: Reveal, Label: "显示或隐藏当前凭据"},
	{Key: Error, Label: "查看错误全文"},
	{Key: Cancel, Label: "取消当前层；有修改时确认放弃"},
}
