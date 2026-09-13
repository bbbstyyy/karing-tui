package pages

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/core"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
)

// LogsPage 查看程序日志与 sing-box 日志，Tab 切换来源，自动跟随滚动。
type LogsPage struct {
	base
	source   int // 0=程序日志, 1=sing-box 日志
	offset   int // 距底部的行数偏移，0 表示跟随
	maxLines int
}

// NewLogs 创建 Logs 页。
func NewLogs(app *application.App) *LogsPage {
	return &LogsPage{base: base{app: app}, maxLines: 500}
}

func (l *LogsPage) Title() string { return "Logs" }

func (l *LogsPage) Init() tea.Cmd { return tickAt(500 * time.Millisecond) }

func (l *LogsPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		l.SetSize(msg.Width, msg.Height)
	case ActivateMsg:
		// 切回本页时 tick 链已断，重启自动刷新
		return l, tickAt(500 * time.Millisecond)
	case tickMsg:
		// 新日志到来时保持跟随。
		if l.offset == 0 {
			return l, tickAt(500 * time.Millisecond)
		}
		return l, tickAt(500 * time.Millisecond)
	case tea.KeyMsg:
		switch msg.String() {
		case "tab":
			l.source = (l.source + 1) % 2
			l.offset = 0
		case "up", "k":
			l.offset++
		case "down", "j":
			if l.offset > 0 {
				l.offset--
			}
		case "G":
			l.offset = 0
		}
	}
	return l, nil
}

func (l *LogsPage) buf() *core.LogBuf {
	if l.source == 0 {
		return l.app.AppLog
	}
	return l.app.Core.Output
}

func (l *LogsPage) View() string {
	sourceName := "程序日志"
	other := "sing-box 日志"
	if l.source == 1 {
		sourceName, other = other, sourceName
	}
	head := styles.Title.Render(sourceName) + styles.Dim.Render("  (Tab 切换到 "+other+")") + "\n"

	lines := l.buf().Tail(l.maxLines)
	if l.offset > 0 && len(lines) > l.offset {
		lines = lines[:len(lines)-l.offset]
	}

	// 根据可用高度裁剪显示
	maxShow := l.height - 6
	if maxShow < 3 {
		maxShow = 3
	}
	if len(lines) > maxShow {
		lines = lines[len(lines)-maxShow:]
	}

	body := strings.Join(lines, "\n")
	if body == "" {
		body = styles.Dim.Render("暂无日志")
	}

	footer := ""
	if l.offset > 0 {
		footer = styles.Dim.Render(fmt.Sprintf("\n↑ 回看 %d 行 · G 回到底部", l.offset))
	} else {
		footer = styles.Dim.Render("\n跟随中 · ↑ 回看")
	}
	return head + body + footer
}
