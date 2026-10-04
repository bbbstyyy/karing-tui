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
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
)

// snapMsg clash API 快照（流量 + 代理组状态）。
type snapMsg struct {
	id        uint64
	startedAt time.Time
	sampledAt time.Time
	conns     clashapi.Connections
	traffic   []clashapi.TrafficConnection
	proxies   map[string]clashapi.ProxyInfo
	err       error
}

// testDoneMsg Dashboard 组测速完成。
type testDoneMsg struct {
	id        uint64
	startedAt time.Time
	err       error
}

type groupTrafficStat struct {
	Upload      int64
	Download    int64
	Connections int
}

type connTrafficSample struct {
	Upload   int64
	Download int64
}

// Dashboard 展示运行状态、流量统计、代理组当前节点、订阅状态与配置状态，
// 并提供核心启停、配置生成与组测速操作。
type Dashboard struct {
	base
	status  core.Status
	lastErr error // 最近一次操作错误
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

	// 按代理组做采样累计。Clash API 只暴露当前活跃连接，因此完全发生在两次
	// 采样之间的短连接无法归属；UI 明确标为“采样”，避免把它误解为精确账单。
	// 热路径复用 map，避免按秒轮询时产生与连接数成正比的临时 map 分配。
	groupTraffic         map[string]groupTrafficStat
	groupTrafficNames    map[string]struct{}
	connTraffic          map[string]connTrafficSample
	nextConnTraffic      map[string]connTrafficSample
	groupTrafficInstance time.Time

	fetching       bool
	fetchStartedAt time.Time
	testBusy       bool
	testStartedAt  time.Time
	testID         uint64
	tickCount      int
	fetchID        uint64
	prevInstance   time.Time
	details        components.TextView
	confirm        components.Confirm
}

// NewDashboard 创建 Dashboard 页。
func NewDashboard(app *application.App) *Dashboard {
	return &Dashboard{base: base{app: app}}
}

func (d *Dashboard) Title() string { return "概览" }

func (d *Dashboard) Editing() bool { return d.detailActive || d.confirm.Active }

// dashboardTickInterval 是概览页的刷新节拍：刷新核心状态、流量与配置状态。
const dashboardTickInterval = time.Second

// Init 不再启动 tick：周期刷新由 ActivateMsg 启动，隐藏页面不再空转。
func (d *Dashboard) Init() tea.Cmd { return nil }

func (d *Dashboard) Update(msg tea.Msg) (Page, tea.Cmd) {
	if !d.Editing() {
		if handled, cmd := d.handleRetry(msg); handled {
			return d, cmd
		}
	}
	if d.detailActive || !d.Editing() {
		if d.handleDetails(msg, d.lastErr) {
			return d, nil
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		d.SetSize(msg.Width, msg.Height)
		return d, nil

	case ActivateMsg:
		// Root delivers the single refresh chain even while another page is open.
		d.reloadModel()
		d.status = d.app.Core.Status()
		return d, d.startTick(dashboardTickInterval)
	case DeactivateMsg:
		// 切走即停：请求自愈与测速超时都由激活中的 tick 驱动，
		// 隐藏时不再轮询（切回时由 ActivateMsg + startTick 重新开始）。
		d.stopTick()
		return d, nil
	case components.ConfirmMsg:
		if msg.ID == "restart" && msg.Confirmed {
			return d, d.runAction("restart", d.app.RestartCore)
		}
		return d, nil
	case ApplyConfigMsg:
		if d.app.Core.IsRunning() {
			return d, d.runAction("restart", d.app.RestartCore)
		}
		return d, d.runAction("start", d.app.StartCore)

	case tickMsg:
		renew, ok := d.acceptTick(msg, dashboardTickInterval)
		if !ok {
			return d, nil
		}
		d.tickCount++
		d.status = d.app.Core.Status()
		if d.tickCount%5 == 1 { // 每 5 秒刷新订阅与组（本地 DB 查询）
			d.reloadModel()
		}
		// 在途请求超时自愈（切页期间结果被丢弃时 fetching 会卡住）
		if d.fetching && time.Since(d.fetchStartedAt) > 3*time.Second {
			d.fetching = false
		}
		if d.testBusy && time.Since(d.testStartedAt) > 4*time.Minute {
			d.testBusy = false
			d.taskResult = "代理组测速超时，请重试"
			d.lastErr = fmt.Errorf("代理组测速超时，请按 t 重试")
		}
		if d.status.State != core.StateRunning {
			d.apiOK = false
			d.havePrev = false
			d.upSpeed, d.downSpeed = 0, 0
			d.upTotal, d.downTotal, d.conns = 0, 0, 0
			d.proxies = nil
			d.resetGroupTraffic()
		} else if client := d.app.ClashClient(); client != nil && !d.fetching {
			d.fetching = true
			d.fetchStartedAt = time.Now()
			return d, tea.Batch(d.fetchSnapshot(client), renew)
		}
		return d, renew

	case snapMsg:
		if msg.id != d.fetchID {
			return d, nil
		}
		d.fetching = false
		current := d.app.Core.Status()
		if current.State != core.StateRunning || !msg.startedAt.Equal(current.StartedAt) {
			return d, nil
		}
		if msg.err != nil {
			// 启动初期 API 未就绪属正常，静默等下一轮
			d.apiOK = false
			d.havePrev = false
			d.upSpeed, d.downSpeed = 0, 0
			return d, nil
		}
		now := msg.sampledAt
		if d.havePrev && !now.After(d.prevAt) {
			return d, nil
		}
		d.upSpeed, d.downSpeed = 0, 0
		if d.havePrev && msg.startedAt.Equal(d.prevInstance) && msg.conns.UploadTotal >= d.prevUp && msg.conns.DownloadTotal >= d.prevDown {
			if dt := now.Sub(d.prevAt).Seconds(); dt > 0.2 {
				d.upSpeed = int64(float64(msg.conns.UploadTotal-d.prevUp) / dt)
				d.downSpeed = int64(float64(msg.conns.DownloadTotal-d.prevDown) / dt)
			}
		}
		d.prevInstance = msg.startedAt
		d.prevUp, d.prevDown, d.prevAt, d.havePrev =
			msg.conns.UploadTotal, msg.conns.DownloadTotal, now, true
		d.upTotal, d.downTotal, d.conns =
			msg.conns.UploadTotal, msg.conns.DownloadTotal, msg.conns.Count
		d.updateGroupTraffic(msg.startedAt, msg.traffic)
		d.proxies = msg.proxies
		d.apiOK = true
		return d, nil

	case testDoneMsg:
		if msg.id != d.testID || !d.testBusy {
			return d, nil
		}
		d.testBusy = false
		if !msg.startedAt.Equal(d.app.Core.Status().StartedAt) || !d.app.Core.IsRunning() {
			d.taskResult = "代理组测速结束；核心已变化，请重试"
			return d, nil
		}
		if msg.err != nil {
			d.lastErr = msg.err
			d.taskResult = "代理组测速失败（回到本页查看）"
			return d, nil
		}
		d.taskResult = "代理组测速完成"
		d.lastErr = nil
		// 测速完成立即拉一次快照，刷新延迟显示
		if client := d.app.ClashClient(); client != nil && !d.fetching {
			d.fetching = true
			d.fetchStartedAt = time.Now()
			return d, d.fetchSnapshot(client)
		}
		return d, nil

	case actionDoneMsg:
		if !d.accept(msg) {
			return d, nil
		}
		if msg.Action == "group-test" {
			d.testBusy = false
			if !msg.Instance.Equal(d.app.Core.Status().StartedAt) || !d.app.Core.IsRunning() {
				d.lastErr = fmt.Errorf("测速期间核心已变化，请重新测速")
				d.taskFailed = true
				return d, nil
			}
		}
		if msg.Err != nil {
			d.lastErr = msg.Err
		} else {
			d.lastErr = nil
			switch msg.Action {
			case "start", "restart":
				d.status = d.app.Core.Status()
				d.havePrev = false
				d.apiOK = false
				d.upSpeed, d.downSpeed = 0, 0
				d.upTotal, d.downTotal, d.conns = 0, 0, 0
				d.proxies = nil
				d.resetGroupTraffic()
			}
		}
		return d, nil

	case tea.KeyMsg:
		if d.confirm.Active {
			_, cmd := d.confirm.Update(msg)
			return d, cmd
		}
		switch msg.String() {
		case "up", "down", "j", "k", "pgup", "pgdown", "home", "end":
			d.details.Move(msg.String(), max(1, d.height-10))
			return d, nil
		}
		switch msg.String() {
		case "s":
			return d, d.runAction("start", func(ctx context.Context) error {
				return d.app.StartCore(ctx)
			})
		case "x":
			return d, d.runAction("stop", func(ctx context.Context) error {
				return d.app.StopCore()
			})
		case "R":
			d.confirm = components.NewConfirm("restart", "生成并校验后重启核心，现有连接会中断。校验失败时保持当前核心运行。")
			return d, nil
		case "r":
			d.reloadModel()
			d.status = d.app.Core.Status()
			return d, nil
		case "a":
			return d, func() tea.Msg { return NavigateMsg{Page: 1} }
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
	if d.taskActive {
		return nil
	}
	d.lastErr = nil
	d.retryTask = func() tea.Cmd { return d.runAction(action, fn) }
	labels := map[string]string{"start": "启动核心", "stop": "停止核心", "restart": "应用配置并重启", "generate": "生成并校验配置"}
	return d.task(labels[action], func() tea.Msg {
		ctx, cancel := context.WithTimeout(d.app.BackgroundContext(), 10*time.Minute)
		defer cancel()
		return actionDoneMsg{Action: action, Err: fn(ctx)}
	})
}

// --- 数据获取 ---

// reloadModel 从 DB 刷新订阅、代理组与节点索引。
func (d *Dashboard) reloadModel() {
	if subs, err := d.app.DB.ListSubscriptions(); err == nil {
		d.subs = subs
	}
	if groups, err := d.app.DB.ListProxyGroups(); err == nil {
		d.groups = groups
		d.refreshGroupTrafficNames()
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
	d.fetchID++
	id := d.fetchID
	startedAt := d.app.Core.Status().StartedAt
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(d.app.BackgroundContext(), 3*time.Second)
		defer cancel()
		m := snapMsg{id: id, startedAt: startedAt}
		if m.conns, m.traffic, m.err = client.TrafficConnections(ctx); m.err == nil {
			m.proxies, m.err = client.Proxies(ctx)
		}
		m.sampledAt = time.Now()
		return m
	}
}

func (d *Dashboard) resetGroupTraffic() {
	clear(d.groupTraffic)
	clear(d.connTraffic)
	clear(d.nextConnTraffic)
	d.groupTrafficInstance = time.Time{}
}

// refreshGroupTrafficNames 重建运行时代理组名索引，并清掉已删除/改名组的陈旧统计。
func (d *Dashboard) refreshGroupTrafficNames() {
	if d.groupTrafficNames == nil {
		d.groupTrafficNames = make(map[string]struct{}, len(d.groups))
	} else {
		clear(d.groupTrafficNames)
	}
	for _, g := range d.groups {
		d.groupTrafficNames[g.Name] = struct{}{}
	}
	for name := range d.groupTraffic {
		if _, ok := d.groupTrafficNames[name]; !ok {
			delete(d.groupTraffic, name)
		}
	}
}

func (d *Dashboard) updateGroupTraffic(instance time.Time, conns []clashapi.TrafficConnection) {
	if !d.groupTrafficInstance.Equal(instance) {
		d.resetGroupTraffic()
		d.groupTrafficInstance = instance
	}
	if d.groupTrafficNames == nil {
		d.refreshGroupTrafficNames()
	}
	if d.groupTraffic == nil {
		d.groupTraffic = make(map[string]groupTrafficStat, len(d.groups))
	}
	if d.connTraffic == nil {
		d.connTraffic = make(map[string]connTrafficSample, len(conns))
	}
	if d.nextConnTraffic == nil {
		d.nextConnTraffic = make(map[string]connTrafficSample, len(conns))
	} else {
		clear(d.nextConnTraffic)
	}

	for _, g := range d.groups {
		stat := d.groupTraffic[g.Name]
		stat.Connections = 0
		d.groupTraffic[g.Name] = stat
	}

	for _, conn := range conns {
		prev, seen := d.connTraffic[conn.ID]
		upDelta, downDelta := conn.Upload, conn.Download
		if seen && conn.Upload >= prev.Upload {
			upDelta = conn.Upload - prev.Upload
		}
		if seen && conn.Download >= prev.Download {
			downDelta = conn.Download - prev.Download
		}
		d.nextConnTraffic[conn.ID] = connTrafficSample{Upload: conn.Upload, Download: conn.Download}

		for i, tag := range conn.Chains {
			if _, ok := d.groupTrafficNames[tag]; !ok {
				continue
			}
			// 正常 sing-box 出站链不会重复；仍对异常输入去重，但用小切片扫描，
			// 避免为每条连接分配一个临时 map。
			duplicate := false
			for _, previousTag := range conn.Chains[:i] {
				if previousTag == tag {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
			stat := d.groupTraffic[tag]
			stat.Upload += upDelta
			stat.Download += downDelta
			stat.Connections++
			d.groupTraffic[tag] = stat
		}
	}
	d.connTraffic, d.nextConnTraffic = d.nextConnTraffic, d.connTraffic
}

// startGroupTest 对全部代理组触发 clash API 测速（仅运行中可用）。
func (d *Dashboard) startGroupTest() tea.Cmd {
	if d.testBusy || d.taskActive {
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
	return d.testGroups(names)
}

func (d *Dashboard) testGroups(names []string) tea.Cmd {
	if d.taskActive || len(names) == 0 {
		return nil
	}
	client := d.app.ClashClient()
	if client == nil || !d.app.Core.IsRunning() {
		d.lastErr = fmt.Errorf("请先启用 Clash API 并启动核心")
		return nil
	}
	d.testBusy = true
	d.testID++
	startedAt := d.app.Core.Status().StartedAt
	d.testStartedAt = time.Now()
	d.lastErr = nil
	d.retryTask = func() tea.Cmd {
		var failed []string
		for _, result := range d.taskResults {
			if result.State == "失败" {
				failed = append(failed, result.Name)
			}
		}
		return d.testGroups(failed)
	}
	return d.taskN("运行代理组测速", len(names), func(report func(itemResult)) tea.Msg {
		ctx, cancel := context.WithTimeout(d.app.BackgroundContext(), 4*time.Minute)
		defer cancel()
		var results []itemResult
		for _, name := range names {
			result := itemResult{Name: name, State: "成功", Detail: "运行节点延迟已刷新"}
			if _, err := client.GroupDelay(ctx, name, "", config.DefaultLatencyTimeoutMS); err != nil {
				result.State, result.Detail = "失败", err.Error()
			}
			results = append(results, result)
			report(result)
		}
		return actionDoneMsg{Action: "group-test", Instance: startedAt, Results: results, Err: resultError(results)}
	})
}

func (d *Dashboard) TaskStatus() (bool, string) {
	active, label := d.base.TaskStatus()
	if d.testBusy {
		if active {
			return true, label
		}
		return true, "代理组测速进行中…"
	}
	return active, label
}

// --- 渲染 ---

func (d *Dashboard) View() string {
	if d.detailActive {
		return d.detailsView()
	}
	if d.confirm.Active {
		return d.confirmView(&d.confirm)
	}
	stateLine := styles.Err.Render("已停止")
	if d.status.State == core.StateRunning {
		stateLine = styles.Ok.Render(fmt.Sprintf("运行中 (%s)", d.status.Uptime))
	}

	version := d.status.Version
	if version == "" {
		version = styles.Dim.Render("未知（未安装可按 s 启动时自动下载）")
	}

	configLine := d.app.ConfigStage()

	apiLine := styles.Dim.Render("关闭")
	set := d.app.CoreSettings()
	if set.ClashAPIPort > 0 {
		apiLine = fmt.Sprintf("127.0.0.1:%d", set.ClashAPIPort)
	}

	trafficLine := d.trafficLine()

	body := fmt.Sprintf(
		"运行状态:  %s · sing-box %s\n配置状态:  %s\n入站端口:  %d (mixed) · Clash API %s\n流量:      %s\n数据目录:  %s\n",
		stateLine, version, configLine, set.MixedPort, apiLine, trafficLine, d.app.Paths.Root,
	)

	body = strings.TrimSuffix(body, "\n")
	footer := "Ctrl+A 应用并启动 · 2 订阅 · 3 代理组 · 6 日志 · ? 更多"
	if d.status.State == core.StateRunning {
		footer = "Ctrl+A 应用配置 · x 停止 · R 重启 · t 测速 · 6 日志 · ? 更多"
	}
	if d.lastErr != nil {
		footer = styles.Err.Render(components.Clip("操作失败: "+d.lastErr.Error(), max(0, d.width-10))+" · ! 详情") + "\n" + footer
	}
	detail := d.setupChecklist() + "\n\n" + d.groupLines() + "\n\n" + d.subLines()
	viewH := max(1, d.height-len(strings.Split(body, "\n"))-len(strings.Split(footer, "\n")))
	return body + "\n" + d.details.View(detail, d.width, viewH) + "\n" + styles.Dim.Render(footer)
}

func (d *Dashboard) hasUsableNode() bool {
	enabledSubs := make(map[int64]bool, len(d.subs))
	for _, sub := range d.subs {
		enabledSubs[sub.ID] = sub.Enabled
	}
	for _, node := range d.nodes {
		if node == nil || !node.Enabled {
			continue
		}
		if node.SubscriptionID == config.ManualSubscriptionID || enabledSubs[node.SubscriptionID] {
			return true
		}
	}
	return false
}

func (d *Dashboard) setupChecklist() string {
	type step struct {
		done bool
		text string
		next string
	}
	hasNodes := d.hasUsableNode()
	hasGroups := len(d.groups) > 0
	configReady := d.app.ConfigReadyForApply()
	running := d.status.State == core.StateRunning
	steps := []step{
		{hasNodes, "准备可用节点", "2 订阅与节点 → a 添加订阅，或 ] → i 导入节点"},
		{hasGroups, "配置代理组", "3 代理组 → a 新建代理组"},
		{configReady, "生成并校验配置", "Ctrl+A 生成、校验并应用"},
		{running, "启动代理核心", "Ctrl+A 应用并启动"},
	}
	lines := []string{"快速设置:"}
	for _, s := range steps {
		mark := "[ ]"
		if s.done {
			mark = "[x]"
		}
		line := "  " + mark + " " + s.text
		if !s.done {
			line += " · " + s.next
		}
		lines = append(lines, line)
	}
	if hasNodes && hasGroups && configReady && running {
		lines = append(lines, "  已完成基础设置 · Ctrl+P 可搜索全部操作 · Ctrl+T 查看后台任务")
	}
	return strings.Join(lines, "\n")
}

// trafficLine 渲染流量统计行。
func (d *Dashboard) trafficLine() string {
	if d.status.State != core.StateRunning || d.app.CoreSettings().ClashAPIPort <= 0 {
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
		return styles.Dim.Render("代理组:    （无，3 代理组 → a 创建）")
	}
	var b strings.Builder
	b.WriteString("代理组:    ")
	for i, g := range d.groups {
		if i > 0 {
			b.WriteString("\n           ")
		}
		traffic := ""
		if d.status.State == core.StateRunning && d.apiOK {
			stat := d.groupTraffic[g.Name]
			traffic = fmt.Sprintf(" · 采样 ↑%s ↓%s · %d连接", humanBytes(stat.Upload), humanBytes(stat.Download), stat.Connections)
		}
		fmt.Fprintf(&b, "%-14s [%-7s] → %s%s", g.Name, g.Type, d.groupTarget(g), traffic)
	}
	return b.String()
}

// groupTarget 单个组的当前节点与延迟：运行中取 clash API 实时状态，
// 未运行时 select 组显示持久化选择。
func (d *Dashboard) groupTarget(g *config.ProxyGroup) string {
	if d.status.State == core.StateRunning {
		if !d.apiOK {
			return styles.Dim.Render("读取中…")
		}
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
		return styles.Dim.Render("订阅:      （无，2 订阅与节点 → a 添加）")
	}
	var b strings.Builder
	b.WriteString("订阅:      ")
	for i, s := range d.subs {
		if i > 0 {
			b.WriteString("\n           ")
		}
		mark := styles.Ok.Render("启用")
		if !s.Enabled {
			mark = styles.Dim.Render("停用")
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
