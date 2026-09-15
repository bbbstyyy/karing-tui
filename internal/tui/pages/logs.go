package pages

import (
	"fmt"
	"strings"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/core"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type logPosition struct {
	anchor    int
	following bool
}
type logLine struct {
	id   int
	text string
}

// LogsPage uses absolute line identities, not distance from a moving tail.
type LogsPage struct {
	base
	source                      int
	positions                   [2]logPosition
	horizontal                  int
	query, previousQuery, level string
	search                      textinput.Model
	typing                      bool
}

func NewLogs(app *application.App) *LogsPage {
	search := textinput.New()
	search.CharLimit = 0
	return &LogsPage{base: base{app: app}, positions: [2]logPosition{{following: true}, {following: true}}, search: search}
}

func (l *LogsPage) Title() string { return "日志" }
func (l *LogsPage) Editing() bool { return l.typing }
func (l *LogsPage) Init() tea.Cmd { return tickAt(500 * time.Millisecond) }

func (l *LogsPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		l.SetSize(msg.Width, msg.Height)
	case tickMsg:
		return l, tickAt(500 * time.Millisecond)
	case tea.KeyMsg:
		key := msg.String()
		if l.typing {
			switch key {
			case "enter":
				l.typing = false
				l.search.Blur()
			case "esc":
				l.typing = false
				l.query = l.previousQuery
				l.search.Blur()
			default:
				l.search, _ = l.search.Update(msg)
				l.query = l.search.Value()
			}
			return l, nil
		}
		switch key {
		case "[", "]":
			l.source = 1 - l.source
		case "/":
			l.previousQuery = l.query
			l.search.SetValue(l.query)
			l.typing = true
			return l, l.search.Focus()
		case "c":
			l.query, l.level = "", ""
		case "f":
			l.level = nextCycle([]string{"", "debug", "info", "warn", "error"}, l.level)
		case "alt+left":
			l.horizontal = max(0, l.horizontal-10)
		case "alt+right":
			l.horizontal += 10
		case "G", "end":
			l.positions[l.source].following = true
		case "up", "k", "down", "j", "pgup", "pgdown", "home", "g":
			hits := l.lines()
			if len(hits) == 0 {
				return l, nil
			}
			pos := &l.positions[l.source]
			index := l.topIndex(hits)
			switch key {
			case "up", "k":
				index--
			case "down", "j":
				index++
			case "pgup":
				index -= l.visible()
			case "pgdown":
				index += l.visible()
			case "home", "g":
				index = 0
			}
			index = max(0, min(index, max(0, len(hits)-l.visible())))
			pos.anchor = hits[index].id
			pos.following = false
		}
	default:
		if l.typing {
			l.search, _ = l.search.Update(msg)
			l.query = l.search.Value()
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

func (l *LogsPage) visible() int { return max(1, l.height-3) }

func (l *LogsPage) lines() []logLine {
	first, lines := l.buf().Snapshot()
	var hits []logLine
	for i, line := range lines {
		plain := strings.ToLower(ansi.Strip(line))
		if l.query != "" && !strings.Contains(plain, strings.ToLower(l.query)) {
			continue
		}
		if l.level != "" && !strings.Contains(plain, l.level) {
			continue
		}
		hits = append(hits, logLine{id: first + i, text: line})
	}
	return hits
}

func (l *LogsPage) topIndex(hits []logLine) int {
	pos := l.positions[l.source]
	if pos.following {
		return max(0, len(hits)-l.visible())
	}
	for i, hit := range hits {
		if hit.id >= pos.anchor {
			return i
		}
	}
	return max(0, len(hits)-l.visible())
}

func (l *LogsPage) View() string {
	source := "[程序]  sing-box"
	if l.source == 1 {
		source = "程序  [sing-box]"
	}
	state := "跟随中"
	if !l.positions[l.source].following {
		state = "回看已固定 · G 回到底部"
	}
	head := fmt.Sprintf("日志 %s · %s · 级别:%s", source, state, orDash(l.level, "全部"))
	if l.typing {
		l.search.Width = max(5, l.width-12)
		head = "搜索: " + l.search.View() + " · Esc 撤销"
	} else if l.query != "" {
		head += " · 搜索:" + l.query
	}
	hits := l.lines()
	body := "暂无匹配日志；c 清除过滤。"
	if len(hits) > 0 {
		start := l.topIndex(hits)
		var lines []string
		for _, hit := range hits[start:min(len(hits), start+l.visible())] {
			lines = append(lines, ansi.Cut(hit.text, l.horizontal, l.horizontal+max(1, l.width)))
		}
		body = strings.Join(lines, "\n")
		if !l.positions[l.source].following && l.positions[l.source].anchor < hits[0].id {
			head += " · 更早日志已轮转"
			l.positions[l.source].anchor = hits[0].id
		}
	}
	footer := "[/] 来源 · ↑/↓ PgUp/PgDn 回看 · Home 最早 · End/G 跟随\n/ 搜索 · f 级别 · c 清除 · Alt+←/→ 查看长行"
	return components.Clip(styles.Title.Render(head), l.width) + "\n" + components.Fit(body, l.width, l.visible()) + "\n" + styles.Dim.Render(footer)
}
