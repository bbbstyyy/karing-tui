package pages

import (
	"cmp"
	"fmt"
	"slices"
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
	// rotated 表示锚点行已被轮转挤出缓冲、锚点被钳到最旧行。C11 起由
	// refresh 在 Update 侧维护，View 只读它显示「更早日志已轮转」；导航、
	// G/end、过滤条件变化时清除。旧行为在 View 里逐帧重判并当场改写锚点，
	// 提示只闪现一帧；现在按实际状态持续显示，直到用户移动或改过滤。
	rotated bool
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

	// C11 过滤缓存：refresh 只在 (version, query, level, source) 任一变化时
	// 重算，其余调用 O(1) 命中。旧行为每帧做两次全量拷贝 + 逐行 Strip ANSI +
	// ToLower（Update 导航一次、View 渲染一次），查询词的小写还在循环内重复。
	// recalcs 是重算计数器，测试用它断言「按 20 次方向键只重算 1 次」
	// 「View 不触发重算」——这正是条目的验收标准，因此留在生产结构里。
	cachedVersion uint64
	cachedQuery   string
	cachedLevel   string
	cachedSource  int
	cachedHits    []logLine
	recalcs       int
	// plainByID[source][绝对行号] = ToLower(Strip(原文))。ANSI strip 前移到
	// 每行一次，与 LogBuf.AppendLine 在写入侧做 redact.Text 的先例同理；
	// 回绕或过滤变化触发的重算只对新行做 Strip，旧行按绝对行号命中缓存。
	// 两个来源的行号空间独立（各自从 0 起算），必须分 map，否则跨来源撞号。
	// refresh 会修剪 first 之前的行号，容量始终 ≤ 缓冲上限 + 本轮新行。
	plainByID [2]map[int]string
}

func NewLogs(app *application.App) *LogsPage {
	search := textinput.New()
	search.CharLimit = 0
	return &LogsPage{base: base{app: app}, positions: [2]logPosition{{following: true}, {following: true}}, search: search}
}

func (l *LogsPage) Title() string { return "日志" }
func (l *LogsPage) Editing() bool { return l.typing }

// logTickInterval 是日志页的刷新节拍。日志由后台持续写入，页面只需按这个
// 节拍重绘以跟随尾部。
const logTickInterval = 500 * time.Millisecond

// Init 不再启动 tick：周期刷新由 ActivateMsg 启动，隐藏页面不再空转。
func (l *LogsPage) Init() tea.Cmd { return nil }

// Update 先处理消息、再统一 refresh：所有可能改变过滤输入（缓冲 version、
// query、level、source）的路径在同一点收敛，View 因此可以只读缓存。
func (l *LogsPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	cmd := l.handleMsg(msg)
	l.refresh()
	return l, cmd
}

func (l *LogsPage) handleMsg(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		l.SetSize(msg.Width, msg.Height)
		l.search.Width = max(5, l.width-12) // 原在 View 内改写，随 View 纯化移到 Update
	case ActivateMsg:
		return l.startTick(logTickInterval)
	case DeactivateMsg:
		l.stopTick()
		return nil
	case tickMsg:
		if renew, ok := l.acceptTick(msg, logTickInterval); ok {
			return renew
		}
		return nil
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
				l.positions[l.source].rotated = false
			default:
				l.search, _ = l.search.Update(msg)
				l.query = l.search.Value()
				l.positions[l.source].rotated = false
			}
			return nil
		}
		switch key {
		case "[", "]":
			l.source = 1 - l.source
		case "/":
			l.previousQuery = l.query
			l.search.SetValue(l.query)
			l.search.Width = max(5, l.width-12)
			l.typing = true
			return l.search.Focus()
		case "c":
			l.query, l.level = "", ""
			l.positions[l.source].rotated = false
		case "f":
			l.level = nextCycle([]string{"", "debug", "info", "warn", "error"}, l.level)
			l.positions[l.source].rotated = false
		case "alt+left":
			l.horizontal = max(0, l.horizontal-10)
		case "alt+right":
			l.horizontal += 10
		case "G", "end":
			l.positions[l.source].following = true
			l.positions[l.source].rotated = false
		case "up", "k", "down", "j", "pgup", "pgdown", "home", "g":
			hits := l.hits()
			if len(hits) == 0 {
				return nil
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
			pos.rotated = false
		}
	default:
		if l.typing {
			l.search, _ = l.search.Update(msg)
			l.query = l.search.Value()
			l.positions[l.source].rotated = false
		}
	}
	return nil
}

func (l *LogsPage) buf() *core.LogBuf {
	if l.source == 0 {
		return l.app.AppLog
	}
	return l.app.Core.Output
}

func (l *LogsPage) visible() int { return max(1, l.height-3) }

// hits 返回过滤结果；必要时先 refresh。导航路径用它保证拿到的是当前缓冲
// 状态下的结果（缓冲可能在两条消息之间被后台写入）。
func (l *LogsPage) hits() []logLine {
	l.refresh()
	return l.cachedHits
}

// refresh 维护日志过滤缓存（C11）。键 = (version, query, level, source)，
// 任一变化才重算；version 先用 LogBuf.Version() 廉价预检，避免每条消息都
// 全量拷贝快照——这是「按住 ↓ 只重算一次」的实现基础，看起来多余的
// Version() 预检不可省：Snapshot 本身就要拷贝全部行。
//
// 重算时 ToLower(query) 只算一次（原实现每行重算）；每行的 Strip+ToLower
// 按绝对行号缓存在 plainByID，回绕后的重算只处理新行。
func (l *LogsPage) refresh() {
	if v := l.buf().Version(); v == l.cachedVersion &&
		l.query == l.cachedQuery && l.level == l.cachedLevel &&
		l.source == l.cachedSource {
		return
	}
	l.recalcs++
	first, version, lines := l.buf().Snapshot()
	l.cachedVersion, l.cachedQuery, l.cachedLevel, l.cachedSource =
		version, l.query, l.level, l.source

	plain := l.plainByID[l.source]
	if plain == nil {
		plain = make(map[int]string)
		l.plainByID[l.source] = plain
	}
	// 修剪已被轮转挤出的行号。
	for id := range plain {
		if id < first {
			delete(plain, id)
		}
	}

	queryLower := strings.ToLower(l.query)
	var hits []logLine
	for i, line := range lines {
		id := first + i
		p, ok := plain[id]
		if !ok {
			p = strings.ToLower(ansi.Strip(line))
			plain[id] = p
		}
		if l.query != "" && !strings.Contains(p, queryLower) {
			continue
		}
		if l.level != "" && !strings.Contains(p, l.level) {
			continue
		}
		hits = append(hits, logLine{id: id, text: line})
	}
	l.cachedHits = hits

	// 锚点被轮转挤出缓冲时钳到最旧行并记录状态。原来这段改写在 View 里
	// （C11 清单点名的 View 副作用），现移到过滤缓存更新处。
	pos := &l.positions[l.source]
	if len(hits) > 0 && !pos.following && pos.anchor < hits[0].id {
		pos.anchor = hits[0].id
		pos.rotated = true
	}
}

func (l *LogsPage) topIndex(hits []logLine) int {
	pos := l.positions[l.source]
	if pos.following {
		return max(0, len(hits)-l.visible())
	}
	// hits 按 id 升序（构造保证）：二分定位第一个 id >= 锚点的行。
	// 原实现线性扫描整个结果集，每次按键和每帧渲染都扫一遍。
	// 锚点被查询/级别过滤出结果集时（idx 落到末尾），与旧行为一致贴底。
	idx, _ := slices.BinarySearchFunc(hits, pos.anchor, func(h logLine, target int) int {
		return cmp.Compare(h.id, target)
	})
	if idx < len(hits) {
		return idx
	}
	return max(0, len(hits)-l.visible())
}

func (l *LogsPage) View() string {
	source := "[程序]  sing-box"
	if l.source == 1 {
		source = "程序  [sing-box]"
	}
	pos := l.positions[l.source]
	state := "跟随中"
	if !pos.following {
		state = "回看已固定 · G 回到底部"
	}
	head := fmt.Sprintf("日志 %s · %s · 级别:%s", source, state, orDash(l.level, "全部"))
	if l.typing {
		head = "搜索: " + l.search.View() + " · Esc 撤销"
	} else if l.query != "" {
		head += " · 搜索:" + l.query
	}
	// 只读缓存：Update 在每条消息后 refresh，View 不再 Snapshot / Strip /
	// 改写锚点（C11 前三者都发生在 View）。同一状态下重复渲染必须字节一致。
	hits := l.cachedHits
	body := "暂无匹配日志；c 清除过滤。"
	if len(hits) > 0 {
		start := l.topIndex(hits)
		var lines []string
		for _, hit := range hits[start:min(len(hits), start+l.visible())] {
			lines = append(lines, ansi.Cut(hit.text, l.horizontal, l.horizontal+max(1, l.width)))
		}
		body = strings.Join(lines, "\n")
		if !pos.following && pos.rotated {
			head += " · 更早日志已轮转"
		}
	}
	footer := "[/] 来源 · ↑/↓ PgUp/PgDn 回看 · Home 最早 · End/G 跟随\n/ 搜索 · f 级别 · c 清除 · Alt+←/→ 查看长行"
	return components.Clip(styles.Title.Render(head), l.width) + "\n" + components.Fit(body, l.width, l.visible()) + "\n" + styles.Dim.Render(footer)
}
