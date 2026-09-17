// Package pages 实现 TUI 的七个页面。
package pages

import (
	"fmt"
	"github.com/bbbstyyy/karing-tui/internal/tui/keys"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/redact"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
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

// ActivateMsg 在页面被切换为当前页时发送，页面可借此刷新数据并启动周期刷新。
type ActivateMsg struct{}

// DeactivateMsg 在页面被切换离开时发送。带周期 timer 的页面必须据此停止续期，
// 否则隐藏页面仍会持续产生 Update/View 乃至后台请求。
type DeactivateMsg struct{}

// tickMsg 周期刷新消息。
//
// Generation 是启动这次 tick 时页面的「激活代次」。tea.Tick 是一次性且
// 不可取消的命令：页面快速切走又切回时，切走后那一代 tick 仍会到达，
// 若不加代次判定就会在重新激活后再续出一条链，造成 timer 叠加。
type tickMsg struct {
	Generation uint64
	At         time.Time
}

// tickAt 返回一个在 d 之后触发 tickMsg 的命令，携带启动时的激活代次。
func tickAt(d time.Duration, generation uint64) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg{Generation: generation, At: t} })
}

// actionDoneMsg 异步操作完成消息。
type actionDoneMsg struct {
	ID       uint64
	Action   string
	Data     string // 附加数据（如备份文件路径）
	Err      error
	Results  []itemResult
	Restore  *application.PreparedRestore
	Instance time.Time
}

type itemResult struct {
	ID                  int64
	Name, State, Detail string
	Key                 string
}

type ApplyConfigMsg struct{}
type NavigateMsg struct{ Page int }

// base 是页面公共字段。
type base struct {
	app                     *application.App
	width                   int
	height                  int
	taskID                  uint64
	taskActive              bool
	taskLabel               string
	taskResult              string
	detailActive            bool
	detailTitle, detailText string
	detailScroll            components.TextView
	preview                 string
	previewFocus            bool
	previewScroll           components.TextView
	previewKey              string
	progress                *taskProgress
	taskResults             []itemResult
	taskFailed              bool
	taskUntil               time.Time
	retryTask               func() tea.Cmd
	feedbackText            string
	feedbackUntil           time.Time
	// active / tickGeneration 是页面生命周期状态（C2）：
	//   active          — 本页是否是当前可见页（带 timer 的页面据它决定是否续期）
	//   tickGeneration  — 激活代次，用于丢弃切页前启动、切页后才到达的在途 tick
	// 递增点只有 startTick / stopTick 两处，见各自说明。
	active         bool
	tickGeneration uint64
}

func (b *base) SetSize(width, height int) {
	b.width = width
	b.height = height
	if width < 120 {
		b.previewFocus = false
	}
}

// startTick 在页面被激活时启动一代新的周期刷新，并返回第一发 tick。
//
// 代次在这里递增：停用时也会再递增一次，于是「停用前启动、停用后才到达」
// 的 tick 因代次不匹配而被丢弃，无法在重新激活后续出新链。
func (b *base) startTick(interval time.Duration) tea.Cmd {
	b.active = true
	b.tickGeneration++
	return tickAt(interval, b.tickGeneration)
}

// stopTick 在页面被切换离开时让在途 tick 全部失效。
func (b *base) stopTick() {
	b.active = false
	b.tickGeneration++
}

// acceptTick 处理周期刷新消息：返回该 tick 是否属于当前激活代次，以及
// 属于当前代次时应续发的下一发 tick。
//
// 只有能被接受的 tick 才会续期——旧代次在切页之后到达时直接丢弃，
// 重新激活时由 startTick 另起一代，因此 timer 链不会叠加。
func (b *base) acceptTick(msg tickMsg, interval time.Duration) (tea.Cmd, bool) {
	if !b.active || msg.Generation != b.tickGeneration {
		return nil, false
	}
	return tickAt(interval, b.tickGeneration), true
}

// Editing 默认不处于编辑状态；有文本输入的页面自行覆盖。
func (b *base) Editing() bool { return b.detailActive }

func (b *base) openDetails(title, text string) {
	b.detailActive = true
	b.detailTitle = title
	b.detailText = redact.Text(text)
	b.detailScroll.Offset = 0
}

func (b *base) handleDetails(msg tea.Msg, err error) bool {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false
	}
	if b.detailActive {
		if key.String() == keys.Cancel || key.String() == "q" {
			b.detailActive = false
		} else {
			b.detailScroll.Move(key.String(), max(1, b.height-2))
		}
		return true
	}
	if key.String() == "v" && len(b.taskResults) > 0 {
		b.openDetails(b.taskLabel+"结果", resultsText(b.taskResults))
		return true
	}
	if (key.String() == keys.Focus || key.String() == keys.FocusBack) && b.width >= 120 && b.preview != "" {
		b.previewFocus = !b.previewFocus
		return true
	}
	if b.previewFocus {
		switch key.String() {
		case keys.Cancel, "q":
			b.previewFocus = false
			return true
		case keys.Enter, "alt+enter":
			b.openDetails("详情", b.preview)
			return true
		}
		if b.previewScroll.Move(key.String(), max(1, b.height-7)) {
			return true
		}
	}
	if key.String() == "!" && err != nil {
		b.openDetails("错误详情", err.Error())
		return true
	}
	return false
}

func (b *base) detailsView() string {
	return components.Clip(styles.Title.Render(b.detailTitle), b.width) + "\n" +
		b.detailScroll.View(b.detailText, b.width, max(1, b.height-2)) + "\n" +
		styles.Dim.Render("↑/↓ PgUp/PgDn 滚动 · Home/End 首尾 · Esc 返回")
}

func (b *base) mainWidth() int {
	if b.width >= 120 {
		return b.width * 3 / 5
	}
	return b.width
}

// task assigns an identity before starting work. Root separately binds commands
// to their source page, so neither page switches nor old results lose feedback.
func (b *base) task(label string, cmd tea.Cmd) tea.Cmd {
	return b.taskN(label, 0, func(func(itemResult)) tea.Msg { return cmd() })
}

func (b *base) accept(msg actionDoneMsg) bool {
	if msg.ID != b.taskID || !b.taskActive {
		return false
	}
	b.taskActive = false
	b.taskFailed = msg.Err != nil
	b.taskResults = msg.Results
	b.taskUntil = time.Now().Add(8 * time.Second)
	b.taskResult = b.taskLabel + "成功"
	if len(msg.Results) > 0 {
		b.taskResult += " · " + resultSummary(msg.Results)
	}
	if msg.Err != nil {
		state := "失败"
		for _, result := range msg.Results {
			if result.State == "成功" {
				state = "部分失败"
				break
			}
		}
		b.taskResult = b.taskLabel + state + " · v 结果 · f 重试 · 6 日志"
		if len(msg.Results) > 0 {
			b.taskResult = b.taskLabel + state + " · " + resultSummary(msg.Results) + " · v 结果 · f 重试"
		}
	}
	return true
}

func (b *base) TaskStatus() (bool, string) {
	if b.taskActive {
		return true, b.progressLabel()
	}
	if !b.taskFailed && time.Now().After(b.taskUntil) {
		return false, ""
	}
	return false, b.taskResult
}

// TransientDeadline 返回本页仍在展示、且尚未到期的瞬时文案的结束时刻。
//
// 瞬时文案有三类，都只在 8 秒内可见：操作反馈（feedback）、成功任务结果、
// 以及 Root 自己的通知（由 Root 单独处理）。旧实现靠 Root 每 200ms 一次的
// 全局 tick 顺带重绘，窗口到期时那句文案自然消失；去掉全局 tick 后，Root
// 需要知道「还有多久才有东西要过期」，才能只在必要时唤醒一次。
//
// 返回零值表示本页当前没有需要收尾的窗口。已过期的窗口不再上报，
// 否则 Root 的续期逻辑会退化成忙循环。
func (b *base) TransientDeadline() time.Time {
	var deadline time.Time
	if b.feedbackText != "" {
		deadline = b.feedbackUntil
	}
	// 失败结果是 sticky：它不随时间消失，因此不参与收尾排期。
	if !b.taskActive && !b.taskFailed && b.taskUntil.After(deadline) {
		deadline = b.taskUntil
	}
	return deadline
}

func (b *base) formView(f *components.Form, err error) string {
	f.Width, f.Height = b.width, b.height
	if err != nil {
		f.SetError(redact.Error(err))
	}
	return f.View()
}

func (b *base) confirmView(c *components.Confirm) string {
	c.Width, c.Height = b.width, b.height
	return c.View()
}

// listView reserves the header, feedback and hints before sizing the list.
func (b *base) listView(l *components.SimpleList, head, empty, feedback, hint string) string {
	w, h := b.width, b.height
	if w <= 0 {
		w = 78
	}
	if h <= 0 {
		h = 20
	}
	head = components.Fit(components.Wrap(head, w), w, min(3, len(strings.Split(components.Wrap(head, w), "\n"))))
	feedback = redact.Text(strings.TrimSpace(feedback))
	if feedback != "" {
		feedback = components.Fit(components.Wrap(feedback, w), w, min(3, len(strings.Split(components.Wrap(feedback, w), "\n")))) + "\n"
	}
	hint = components.Wrap(hint, w)
	hint = components.Fit(hint, w, min(2, len(strings.Split(hint, "\n"))))
	footer := feedback + styles.Dim.Render(hint)
	l.Width, l.Height = w, max(1, h-len(strings.Split(head, "\n"))-len(strings.Split(footer, "\n")))
	if w >= 120 && b.preview != "" {
		if b.previewKey != l.SelectedKey() {
			b.previewKey = l.SelectedKey()
			b.previewScroll.Offset = 0
		}
		l.Width = b.mainWidth()
		rightW := w - l.Width - 3
		title := "详情 · Tab 聚焦 · Alt+Enter 全文"
		if b.previewFocus {
			title = "[焦点: 详情] · Tab 返回列表"
		}
		right := components.Clip(styles.Accent.Render(title), rightW) + "\n" + b.previewScroll.View(redact.Text(b.preview), rightW, max(1, l.Height-1))
		separator := strings.TrimSuffix(strings.Repeat(" | \n", l.Height), "\n")
		left := strings.Split(l.View(empty), "\n")
		for i := range left {
			left[i] = components.Pad(left[i], l.Width)
		}
		return head + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, strings.Join(left, "\n"), separator, right) + "\n" + footer
	}
	b.previewFocus = false
	return head + "\n" + l.View(empty) + "\n" + footer
}

func (b *base) PreviewFocused() bool { return b.previewFocus }

func resultSummary(results []itemResult) string {
	ok, failed, skipped := 0, 0, 0
	for _, r := range results {
		switch r.State {
		case "成功":
			ok++
		case "失败":
			failed++
		case "跳过":
			skipped++
		}
	}
	return fmt.Sprintf("成功 %d · 失败 %d · 跳过 %d", ok, failed, skipped)
}
