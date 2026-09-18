package pages

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/proxy"
	"github.com/bbbstyyy/karing-tui/internal/redact"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
	tea "github.com/charmbracelet/bubbletea"
)

// Only the progress snapshot crosses goroutines. Page models are changed by
// Update, including when a completion arrives while another page is visible.
type taskProgress struct {
	mu      sync.Mutex
	total   int
	results []itemResult
}

func (b *base) taskN(label string, total int, run func(func(itemResult)) tea.Msg) tea.Cmd {
	return b.startTask(label, total, true, run)
}

func (b *base) startTask(label string, total int, shared bool, run func(func(itemResult)) tea.Msg) tea.Cmd {
	if b.taskActive {
		return nil
	}
	b.taskID++
	id := b.taskID
	b.taskActive, b.taskLabel, b.taskFailed = true, label, false
	progress := &taskProgress{total: total}
	b.progress = progress
	return func() tea.Msg {
		if shared {
			release, err := b.app.BeginOperation()
			if err != nil {
				return actionDoneMsg{ID: id, Action: label, Err: err}
			}
			defer release()
		}
		msg := run(func(result itemResult) {
			result.Name, result.Detail = redact.Text(result.Name), redact.Text(result.Detail)
			progress.mu.Lock()
			progress.results = append(progress.results, result)
			progress.mu.Unlock()
		})
		if done, ok := msg.(actionDoneMsg); ok {
			done.ID = id
			return done
		}
		return msg
	}
}

func (b *base) progressLabel() string {
	frames := []string{"|", "/", "-", "\\"}
	label := frames[(time.Now().UnixMilli()/200)%int64(len(frames))] + " " + b.taskLabel + "进行中"
	if b.progress == nil {
		return label
	}
	b.progress.mu.Lock()
	defer b.progress.mu.Unlock()
	if b.progress.total > 0 {
		label += fmt.Sprintf(" %d/%d", len(b.progress.results), b.progress.total)
		if len(b.progress.results) > 0 {
			label += " · " + resultSummary(b.progress.results)
		}
	}
	return label
}

func (b *base) feedback(status string, err error) string {
	if status != b.feedbackText {
		b.feedbackText, b.feedbackUntil = status, time.Now().Add(8*time.Second)
	}
	var lines []string
	if b.taskActive {
		lines = append(lines, styles.Accent.Render(b.progressLabel()))
	} else if err == nil && status != "" && time.Now().Before(b.feedbackUntil) {
		lines = append(lines, styles.Ok.Render(status))
	}
	if err != nil {
		lines = append(lines, styles.Err.Render("失败: "+redact.Text(err.Error())+" · ! 详情 · 6 日志"))
	}
	return strings.Join(lines, "\n")
}

func (b *base) handleRetry(msg tea.Msg) (bool, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "f" && b.retryTask != nil && b.taskFailed && !b.taskActive {
		return true, b.retryTask()
	}
	return false, nil
}

func (b *base) TaskActions() (bool, bool) {
	return len(b.taskResults) > 0, b.taskFailed && !b.taskActive && b.retryTask != nil
}

func resultsText(results []itemResult) string {
	lines := []string{resultSummary(results), ""}
	for _, result := range results {
		lines = append(lines, result.State+" · "+result.Name+"\n"+result.Detail+"\n")
	}
	return strings.Join(lines, "\n")
}

func resultError(results []itemResult) error {
	for _, r := range results {
		if r.State == "失败" {
			return fmt.Errorf("%s；v 查看逐项结果，f 仅重试失败项", resultSummary(results))
		}
	}
	return nil
}

func failedIDs(results []itemResult) []int64 {
	var ids []int64
	for _, r := range results {
		if r.State == "失败" {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

func latencyResults(app *application.App, ids []int64, report func(itemResult)) actionDoneMsg {
	ctx, cancel := context.WithTimeout(app.BackgroundContext(), 10*time.Minute)
	defer cancel()
	var mu sync.Mutex
	byID := map[int64]itemResult{}
	// timeoutMS 传 0：由 proxy 层取 config.DefaultLatencyTimeoutMS，
	// 与运行代理组测速保持同一默认值。
	_, err := app.Proxy.TestLatencyProgress(ctx, ids, "", 0, func(result proxy.NodeTestResult) {
		row := itemResult{ID: result.ID, Name: result.Name, State: "成功", Detail: fmt.Sprintf("%d ms", result.LatencyMS)}
		if result.Err != nil {
			row.State, row.Detail = "失败", redact.Text(result.Err.Error())
		}
		mu.Lock()
		byID[row.ID] = row
		mu.Unlock()
		report(row)
	})
	var results []itemResult
	for _, id := range ids {
		row, ok := byID[id]
		if !ok {
			row = itemResult{ID: id, Name: fmt.Sprintf("节点 %d", id), State: "失败", Detail: "测速未完成"}
			if n, getErr := app.DB.GetNode(id); getErr == nil {
				row.Name = n.Name
			}
			if err != nil {
				row.Detail = redact.Text(err.Error())
			}
			report(row)
		}
		results = append(results, row)
	}
	if err == nil {
		err = resultError(results)
	}
	return actionDoneMsg{Action: "test-latency", Results: results, Data: resultSummary(results) + latencyScopeHint, Err: err}
}

// latencyScopeHint 标注独立节点测速的 DNS 口径。
//
// 运行中代理组测速（Dashboard）用的是**已应用**的配置，独立节点测速读的是
// **已保存**的 DNS —— 核心停止时也必须能测速，只能这样。用户改了 DNS 却还没
// Ctrl+A 应用时，两条路径短暂不一致属于预期行为，写在结果里免得被当成节点故障。
const latencyScopeHint = " · 使用已保存 DNS"
