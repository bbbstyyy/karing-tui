package pages

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/clashapi"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/core"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
)

// snapMsg clash API 快照（流量 + 代理组状态）。
type snapMsg struct {
	conns   clashapi.Connections
	proxies map[string]clashapi.ProxyInfo
	err     error
}

// testDoneMsg Dashboard 组测速完成。
type testDoneMsg struct{ err error }

// Dashboard 展示运行状态、流量统计、代理组当前节点、订阅状态与配置状态，
// 并提供核心启停、配置生成与组测速操作。
type Dashboard struct {
	base
	status          core.Status
	lastErr         error // 最近一次操作错误
	configGenerated bool
	checkedAt       time.Time
	checkErr        error

	// 内部模型（未运行时展示持久化的组选择）
	subs   []*config.Subscription
	groups []*config.ProxyGroup
	nodes  map[int64]*config.Node

	// clash API 运行时状态
	apiOK     bool
	upSpeed   int64 // B/s
	downSpeed int64
	upTotal   int64 // 进程启动以来累计
	downTotal int64
	conns     int
	prevUp    int64
	prevDown  int64
	prevAt    time.Time
	havePrev  bool
	proxies   map[string]clashapi.ProxyInfo

	fetching       bool
	fetchStartedAt time.Time
	testBusy       bool
	testStartedAt  time.Time
	tickCount      int
}

// NewDashboard 创建 Dashboard 页。
func NewDashboard(app *application.App) *Dashboard {
	return &Dashboard{base: base{app: app}}
}

func (d *Dashboard) Title() string { return "Dashboard" }

func (d *Dashboard) Init() tea.Cmd { return tickAt(time.Second) }

func (d *Dashboard) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		d.SetSize(msg.Width, msg.Height)
		return d, nil

	case ActivateMsg:
		// 切回本页时 tick 链已断，重启并立即刷新数据
		d.reloadModel()
		d.fetching = false
		d.testBusy = false
		return d, tickAt(time.Second)

	case tickMsg:
		d.tickCount++
		d.status = d.app.Core.Status()
		d.configGenerated, d.checkedAt, d.checkErr = d.app.ConfigStatus()
		if d.tickCount%5 == 1 { // 每 5 秒刷新订阅与组（本地 DB 查询）
			d.reloadModel()
		}
		// 在途请求超时自愈（切页期间结果被丢弃时 fetching 会卡住）
		if d.fetching && time.Since(d.fetchStartedAt) > 3*time.Second {
			d.fetching = false
		}
		if d.testBusy && time.Since(d.testStartedAt) > 4*time.Minute {
			d.testBusy = false
		}
		if d.status.State != core.StateRunning {
			d.apiOK = false
			d.havePrev = false
			d.upSpeed, d.downSpeed = 0, 0
			d.upTotal, d.downTotal, d.conns = 0, 0, 0
			d.proxies = nil
		} else if client := d.app.ClashClient(); client != nil && !d.fetching {
			d.fetching = true
			d.fetchStartedAt = time.Now()
			return d, tea.Batch(d.fetchSnapshot(client), tickAt(time.Second))
		}
		return d, tickAt(time.Second)

	case snapMsg:
		d.fetching = false
		if msg.err != nil {
			// 启动初期 API 未就绪属正常，静默等下一轮
			d.apiOK = false
			return d, nil
		}
		now := time.Now()
		if d.havePrev {
			if dt := now.Sub(d.prevAt).Seconds(); dt > 0.2 {
				d.upSpeed = int64(float64(msg.conns.UploadTotal-d.prevUp) / dt)
				d.downSpeed = int64(float64(msg.conns.DownloadTotal-d.prevDown) / dt)
			}
		}
		d.prevUp, d.prevDown, d.prevAt, d.havePrev =
			msg.conns.UploadTotal, msg.conns.DownloadTotal, now, true
		d.upTotal, d.downTotal, d.conns =
			msg.conns.UploadTotal, msg.conns.DownloadTotal, msg.conns.Count
		d.proxies = msg.proxies
		d.apiOK = true
		return d, nil

	case testDoneMsg:
		d.testBusy = false
		if msg.err != nil {
			d.lastErr = msg.err
			return d, nil
		}
		d.lastErr = nil
		// 测速完成立即拉一次快照，刷新延迟显示
		if client := d.app.ClashClient(); client != nil && !d.fetching {
			d.fetching = true
			d.fetchStartedAt = time.Now()
			return d, d.fetchSnapshot(client)
		}
		return d, nil

	case actionDoneMsg:
		if msg.Err != nil {
			d.lastErr = msg.Err
		} else {
			d.lastErr = nil
			switch msg.Action {
			case "start", "restart":
				d.status = d.app.Core.Status()
			}
		}
		return d, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "s":
			return d, d.runAction("start", func(ctx context.Context) error {
				return d.app.StartCore(ctx)
			})
		case "x":
			return d, d.runAction("stop", func(ctx context.Context) error {
				return d.app.StopCore()
			})
		case "r":
			return d, d.runAction("restart", func(ctx context.Context) error {
				return d.app.RestartCore(ctx)
			})
		case "g":
			return d, d.runAction("generate", func(ctx context.Context) error {
				if err := d.app.GenerateConfig(ctx); err != nil {
					return err
				}
				return d.app.CheckConfig(ctx)
			})
		case "t":
			return d, d.startGroupTest()
		}
	}
	return d, nil
}

func (d *Dashboard) runAction(action string, fn func(context.Context) error) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		return actionDoneMsg{Action: action, Err: fn(ctx)}
	}
}

// --- 数据获取 ---

// reloadModel 从 DB 刷新订阅、代理组与节点索引。
func (d *Dashboard) reloadModel() {
	if subs, err := d.app.DB.ListSubscriptions(); err == nil {
		d.subs = subs
	}
	if groups, err := d.app.DB.ListProxyGroups(); err == nil {
		d.groups = groups
	}
	if nodes, err := d.app.DB.ListNodes(0); err == nil {
		d.nodes = make(map[int64]*config.Node, len(nodes))
		for _, n := range nodes {
			d.nodes[n.ID] = n
		}
	}
}

// fetchSnapshot 拉取 clash API 流量与代理组快照。
func (d *Dashboard) fetchSnapshot(client *clashapi.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var m snapMsg
		if m.conns, m.err = client.Connections(ctx); m.err == nil {
			m.proxies, m.err = client.Proxies(ctx)
		}
		return m
	}
}

// startGroupTest 对全部代理组触发 clash API 测速（仅运行中可用）。
func (d *Dashboard) startGroupTest() tea.Cmd {
	if d.testBusy {
		return nil
	}
	client := d.app.ClashClient()
	if client == nil {
		d.lastErr = fmt.Errorf("clash API 未开启（设置 clash_api_port 后重新生成配置）")
		return nil
	}
	if d.status.State != core.StateRunning {
		d.lastErr = fmt.Errorf("sing-box 未运行，无法测速")
		return nil
	}
	names := make([]string, 0, len(d.groups))
	for _, g := range d.groups {
		names = append(names, g.Name)
	}
	if len(names) == 0 {
		d.lastErr = fmt.Errorf("没有代理组")
		return nil
	}
	d.testBusy = true
	d.testStartedAt = time.Now()
	d.lastErr = nil
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		var firstErr error
		for _, name := range names {
			if _, err := client.GroupDelay(ctx, name, "", 5000); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return testDoneMsg{err: firstErr}
	}
}

// --- 渲染 ---

func (d *Dashboard) View() string {
	stateLine := styles.Err.Render("已停止")
	if d.status.State == core.StateRunning {
		stateLine = styles.Ok.Render(fmt.Sprintf("运行中 (%s)", d.status.Uptime))
	}

	version := d.status.Version
	if version == "" {
		version = styles.Dim.Render("未知（未安装可按 s 启动时自动下载）")
	}

	configLine := styles.Dim.Render("未生成")
	if d.configGenerated {
		configLine = styles.Ok.Render("已生成")
		if !d.checkedAt.IsZero() {
			if d.checkErr != nil {
				configLine += " · " + styles.Err.Render("校验失败")
			} else {
				configLine += " · " + styles.Ok.Render("校验通过") +
					styles.Dim.Render(fmt.Sprintf(" (%s)", d.checkedAt.Format("15:04:05")))
			}
		}
	}

	apiLine := styles.Dim.Render("关闭")
	set := d.app.GetSettings()
	if set.ClashAPIPort > 0 {
		apiLine = fmt.Sprintf("127.0.0.1:%d", set.ClashAPIPort)
	}

	trafficLine := d.trafficLine()

	body := fmt.Sprintf(
		"运行状态:  %s · sing-box %s\n配置状态:  %s\n入站端口:  %d (mixed) · Clash API %s\n流量:      %s\n数据目录:  %s\n",
		stateLine, version, configLine, set.MixedPort, apiLine, trafficLine, d.app.Paths.Root,
	)

	body += "\n" + d.groupLines() + "\n" + d.subLines()

	if d.lastErr != nil {
		body += "\n" + styles.Err.Render("最近操作失败: "+d.lastErr.Error())
	}

	hints := styles.Dim.Render("\ns 启动 · x 停止 · r 重启 · g 生成并校验配置 · t 组测速（运行中）")
	return body + hints
}

// trafficLine 渲染流量统计行。
func (d *Dashboard) trafficLine() string {
	if d.status.State != core.StateRunning || d.app.GetSettings().ClashAPIPort <= 0 {
		return styles.Dim.Render("—")
	}
	if !d.apiOK {
		return styles.Dim.Render("连接中…")
	}
	return fmt.Sprintf("↑ %s/s  ↓ %s/s   累计 ↑ %s  ↓ %s   连接 %d",
		humanBytes(d.upSpeed), humanBytes(d.downSpeed),
		humanBytes(d.upTotal), humanBytes(d.downTotal), d.conns)
}

// groupLines 渲染代理组与当前节点（含延迟）。
func (d *Dashboard) groupLines() string {
	if len(d.groups) == 0 {
		return styles.Dim.Render("代理组:    （无，Proxy Groups 页创建）")
	}
	var b strings.Builder
	b.WriteString("代理组:    ")
	for i, g := range d.groups {
		if i > 0 {
			b.WriteString("\n           ")
		}
		fmt.Fprintf(&b, "%-14s [%-7s] → %s", g.Name, g.Type, d.groupTarget(g))
	}
	return b.String()
}

// groupTarget 单个组的当前节点与延迟：运行中取 clash API 实时状态，
// 未运行时 select 组显示持久化选择。
func (d *Dashboard) groupTarget(g *config.ProxyGroup) string {
	if d.status.State == core.StateRunning && d.proxies != nil {
		if p, ok := d.proxies[g.Name]; ok {
			now := p.Now
			if now == "" {
				return styles.Dim.Render("（无成员）")
			}
			line := now
			// 延迟：优先组自身记录（urltest），否则查当前成员的记录
			delay := p.DelayMS
			if delay == 0 {
				if np, ok := d.proxies[now]; ok {
					delay = np.DelayMS
				}
			}
			if delay > 0 {
				line += fmt.Sprintf(" · %dms", delay)
			}
			return line
		}
		return styles.Dim.Render("…")
	}
	if g.Type == "select" {
		if label := d.memberLabel(g.Selected); label != "" {
			return label + styles.Dim.Render(" （未运行）")
		}
		return styles.Dim.Render("（未选择）")
	}
	return styles.Dim.Render("自动测速")
}

// memberLabel 把成员键解析为展示文本；空键或失效引用返回空串。
func (d *Dashboard) memberLabel(key string) string {
	switch {
	case key == "all":
		return "全部节点（动态）"
	case key == "":
		return ""
	case strings.HasPrefix(key, "group:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(key, "group:"), 10, 64)
		for _, g := range d.groups {
			if g.ID == id {
				return "[组] " + g.Name
			}
		}
	case strings.HasPrefix(key, "node:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(key, "node:"), 10, 64)
		if n, ok := d.nodes[id]; ok {
			return fmt.Sprintf("%s (%s:%d)", n.Name, n.Protocol, n.Port)
		}
	}
	return ""
}

// subLines 渲染订阅状态。
func (d *Dashboard) subLines() string {
	if len(d.subs) == 0 {
		return styles.Dim.Render("订阅:      （无，Profiles 页添加）")
	}
	var b strings.Builder
	b.WriteString("订阅:      ")
	for i, s := range d.subs {
		if i > 0 {
			b.WriteString("\n           ")
		}
		mark := styles.Ok.Render("●")
		if !s.Enabled {
			mark = styles.Dim.Render("○")
		}
		info := fmt.Sprintf("%d 节点", s.NodeCount)
		if s.LastUpdated.IsZero() {
			info = styles.Dim.Render("未更新")
		} else {
			info += " · " + styles.Dim.Render(relTime(s.LastUpdated)+"更新")
		}
		fmt.Fprintf(&b, "%s %-16s %s", mark, s.Name, info)
	}
	return b.String()
}

// humanBytes 字节数人性化显示。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// relTime 相对时间描述。
func relTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d小时前", int(d.Hours()))
	default:
		return fmt.Sprintf("%d天前", int(d.Hours()/24))
	}
}
