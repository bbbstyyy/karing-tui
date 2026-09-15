package components

import (
	"github.com/bbbstyyy/karing-tui/internal/tui/keys"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
	tea "github.com/charmbracelet/bubbletea"
)

type ConfirmMsg struct {
	ID        string
	Confirmed bool
}

// Confirm always starts on Cancel and owns every key until it closes.
type Confirm struct {
	ID            string
	Prompt        string
	Active        bool
	Accept        bool
	Width, Height int
	viewport      TextView
}

func NewConfirm(id, prompt string) Confirm {
	return Confirm{ID: id, Prompt: prompt, Active: true}
}

func (c *Confirm) Choose(key string) (done, accepted bool) {
	switch key {
	case keys.Focus, keys.FocusBack, "left", "right", "h", "l":
		c.Accept = !c.Accept
	case keys.Enter:
		c.Active = false
		return true, c.Accept
	case "y", "Y":
		c.Active = false
		return true, true
	case "n", "N", keys.Cancel, "q":
		c.Active = false
		return true, false
	default:
		c.viewport.Move(key, max(1, c.Height-6))
	}
	return false, false
}

func (c *Confirm) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || !c.Active {
		return false, nil
	}
	if done, accepted := c.Choose(key.String()); done {
		id := c.ID
		return true, func() tea.Msg { return ConfirmMsg{ID: id, Confirmed: accepted} }
	}
	return true, nil
}

func (c *Confirm) View() string {
	w, h := c.Width, c.Height
	if w <= 0 {
		w = 76
	}
	if h <= 0 {
		h = 16
	}
	buttons := "> [取消]    [确认]"
	if c.Accept {
		buttons = "  [取消]  > [确认]"
	}
	innerW, innerH := max(1, w-4), max(1, h-2)
	bodyH := max(1, innerH-4)
	body := "确认\n" + c.viewport.View(c.Prompt, innerW, bodyH) + "\n" + buttons +
		"\n" + Clip("Tab/←/→ 选择 · Enter 当前按钮 · q/Esc 取消", innerW)
	return styles.HelpOverlay.Width(w - 2).Render(strings.TrimSuffix(body, "\n"))
}
