package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/bbbstyyy/karing-tui/internal/tui/pages"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
)

type paletteCommand struct {
	Page      int
	Action    pages.Action
	HasAction bool
	Label     string
	Search    string
}

func (m *RootModel) resizeGlobalOverlays() {
	width, height := m.width, m.height
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	contentWidth := max(1, width-1)
	m.paletteList.Width = max(1, contentWidth-4)
	m.paletteList.Height = max(3, height-9)
	m.taskList.Width = max(1, contentWidth-4)
	m.taskList.Height = max(3, height-8)
	if m.paletteInput.Width >= 0 {
		m.paletteInput.Width = max(1, contentWidth-6)
	}
}

func (m *RootModel) openPalette() tea.Cmd {
	m.showActions, m.showHelp, m.showTasks = false, false, false
	m.showPalette = true
	m.paletteInput = textinput.New()
	m.paletteInput.Placeholder = "搜索页面或操作"
	m.paletteInput.Prompt = "> "
	m.paletteInput.CharLimit = 64
	m.resizeGlobalOverlays()
	cmd := m.paletteInput.Focus()
	m.rebuildPalette()
	return cmd
}

func (m *RootModel) rebuildPalette() {
	query := strings.ToLower(strings.TrimSpace(m.paletteInput.Value()))
	m.paletteCommands = nil
	items, itemKeys := []string{}, []string{}
	for pageIndex, page := range m.pages {
		pageLabel := fmt.Sprintf("%d · 前往 %s", pageIndex+1, page.Title())
		pageSearch := strings.ToLower(page.Title() + " 页面 前往 " + fmt.Sprint(pageIndex+1))
		if query == "" || strings.Contains(pageSearch, query) {
			m.paletteCommands = append(m.paletteCommands, paletteCommand{
				Page: pageIndex, Label: pageLabel, Search: pageSearch,
			})
			items = append(items, pageLabel)
			itemKeys = append(itemKeys, fmt.Sprintf("page:%d", pageIndex))
		}
		for actionIndex, action := range pages.Actions(page) {
			aliases := strings.Join(action.Aliases, " ")
			search := strings.ToLower(page.Title() + " " + action.Key + " " + action.Label + " " + aliases)
			if query != "" && !strings.Contains(search, query) {
				continue
			}
			label := fmt.Sprintf("%s · %s · %s", page.Title(), action.Key, action.Label)
			if action.Disabled != "" {
				label += "（" + action.Disabled + "）"
			}
			m.paletteCommands = append(m.paletteCommands, paletteCommand{
				Page: pageIndex, Action: action, HasAction: true, Label: label, Search: search,
			})
			items = append(items, label)
			itemKeys = append(itemKeys, fmt.Sprintf("action:%d:%d", pageIndex, actionIndex))
		}
	}
	m.paletteList.SetItems(items, itemKeys)
	if len(items) > 0 && (m.paletteList.Cursor < 0 || m.paletteList.Cursor >= len(items)) {
		m.paletteList.Cursor = 0
	}
}

func (m *RootModel) handlePaletteKey(key tea.KeyMsg) (tea.Cmd, bool) {
	if !m.showPalette {
		return nil, false
	}
	switch key.String() {
	case "esc", "ctrl+p":
		m.showPalette = false
		m.paletteInput.Blur()
		return nil, true
	case "ctrl+c":
		m.showPalette = false
		m.paletteInput.Blur()
		return m.requestQuit(), true
	case "up", "down", "pgup", "pgdown", "home", "end":
		m.paletteList.Update(key)
		return nil, true
	case "enter":
		if m.paletteList.Cursor < 0 || m.paletteList.Cursor >= len(m.paletteCommands) {
			return nil, true
		}
		entry := m.paletteCommands[m.paletteList.Cursor]
		if entry.HasAction && entry.Action.Disabled != "" {
			return nil, true
		}
		m.showPalette = false
		m.paletteInput.Blur()
		var cmds []tea.Cmd
		if entry.Page != m.current {
			cmds = append(cmds, route(entry.Page, m.switchTo(entry.Page)))
		}
		if entry.HasAction {
			next, cmd := m.update(entry.Action.Message())
			*m = next
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...), true
	default:
		before := m.paletteInput.Value()
		var cmd tea.Cmd
		m.paletteInput, cmd = m.paletteInput.Update(key)
		if m.paletteInput.Value() != before {
			m.rebuildPalette()
		}
		return cmd, true
	}
}

func (m *RootModel) openTaskCenter() {
	m.showActions, m.showHelp, m.showPalette = false, false, false
	m.showTasks = true
	m.resizeGlobalOverlays()
	m.refreshTaskCenter()
}

func (m *RootModel) refreshTaskCenter() {
	items, itemKeys, pageIndexes := []string{}, []string{}, []int{}
	for i, page := range m.pages {
		task, ok := page.(interface{ TaskStatus() (bool, string) })
		if !ok {
			continue
		}
		running, label := task.TaskStatus()
		state := "空闲"
		if running {
			state = "进行中"
		} else if label != "" {
			state = "最近"
		}
		if label == "" {
			label = "暂无任务记录"
		}
		items = append(items, fmt.Sprintf("%s · %s · %s", page.Title(), state, label))
		itemKeys = append(itemKeys, fmt.Sprintf("task:%d", i))
		pageIndexes = append(pageIndexes, i)
	}
	m.taskPages = pageIndexes
	m.taskList.SetItems(items, itemKeys)
}

func (m *RootModel) handleTaskCenterKey(key tea.KeyMsg) (tea.Cmd, bool) {
	if !m.showTasks {
		return nil, false
	}
	switch key.String() {
	case "esc", "q", "ctrl+t":
		m.showTasks = false
		return nil, true
	case "ctrl+c":
		m.showTasks = false
		return m.requestQuit(), true
	case "up", "down", "j", "k", "pgup", "pgdown", "home", "end":
		m.taskList.Update(key)
		return nil, true
	case "enter":
		if m.taskList.Cursor < 0 || m.taskList.Cursor >= len(m.taskPages) {
			return nil, true
		}
		idx := m.taskPages[m.taskList.Cursor]
		m.showTasks = false
		if idx == m.current {
			return nil, true
		}
		return route(idx, m.switchTo(idx)), true
	}
	return nil, true
}

func (m RootModel) paletteView(width, height int) string {
	input := m.paletteInput.View()
	body := "命令面板 · 输入关键字筛选页面与操作\n" + input + "\n" +
		m.paletteList.View("没有匹配的命令") +
		"\nEnter 执行 · ↑/↓ 选择 · Esc 关闭"
	return styles.HelpOverlay.Width(width - 2).Render(body)
}

func (m RootModel) taskCenterView(width, height int) string {
	body := "任务中心 · 所有页面的后台任务与最近结果\n" +
		m.taskList.View("暂无任务") +
		"\nEnter 前往来源页；到来源页后 v 看结果 / f 重试失败项 · Esc 关闭"
	return styles.HelpOverlay.Width(width - 2).Render(body)
}
