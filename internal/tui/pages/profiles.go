package pages

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
)

// profilesMode Profiles 页的视图模式。
type profilesMode int

const (
	profilesSubs    profilesMode = iota // 订阅列表
	profilesNodes                       // 节点列表（单订阅或全部）
	profilesForm                        // 表单（订阅/节点/导入链接）
	profilesConfirm                     // 删除确认
)

// Profiles 管理订阅与节点：订阅增删改与更新、节点搜索过滤排序、
// 手动节点管理、延迟测试。
type Profiles struct {
	base
	mode  profilesMode
	subs  []*config.Subscription
	cur   *config.Subscription // nodes 视图对应的订阅；nil 表示全部节点
	nodes []*config.Node       // nodes 视图的原始列表
	list  components.SimpleList

	// nodes 视图的过滤/排序状态
	search      string
	searchMode  bool
	searchInput textinput.Model
	protoFilter string         // "" = 全部
	subFilter   string         // "" = 全部；"手动"；或订阅名
	sortBy      string         // name / latency
	filtered    []*config.Node // 过滤排序后的展示列表

	form      components.Form
	formKind  string // add-sub / edit-sub / add-node / edit-node / import-links
	editID    int64  // edit-sub / edit-node 的目标 ID
	editNode  *config.Node
	confirm   components.Confirm
	confirmID int64

	err    error
	status string
	busy   bool
}

// NewProfiles 创建 Profiles 页。
func NewProfiles(app *application.App) *Profiles {
	si := textinput.New()
	si.Placeholder = "名称或服务器关键字"
	return &Profiles{base: base{app: app}, searchInput: si}
}

func (p *Profiles) Title() string { return "Profiles" }

// Editing 表单或搜索输入状态时拦截全局键位。
func (p *Profiles) Editing() bool { return p.mode == profilesForm || p.searchMode }

func (p *Profiles) Init() tea.Cmd { return nil }

func (p *Profiles) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.SetSize(msg.Width, msg.Height)
		return p, nil
	case ActivateMsg:
		p.reload()
		return p, nil
	case actionDoneMsg:
		return p.onActionDone(msg)
	case components.ConfirmMsg:
		return p.onConfirm(msg)
	case tea.KeyMsg:
		return p.handleKey(msg)
	}
	return p, nil
}

func (p *Profiles) handleKey(msg tea.KeyMsg) (Page, tea.Cmd) {
	key := msg.String()
	switch p.mode {
	case profilesForm:
		switch key {
		case "esc":
			p.mode = p.formBackMode()
			return p, nil
		case "enter":
			p.submitForm()
			return p, nil
		}
		p.form.Update(msg)
		return p, nil

	case profilesConfirm:
		if consumed, cmd := p.confirm.Update(msg); consumed {
			return p, cmd
		}
		return p, nil

	case profilesNodes:
		return p.handleNodesKey(msg, key)

	default: // profilesSubs
		if consumed, cmd := p.list.Update(msg); consumed {
			return p, cmd
		}
		switch key {
		case "a":
			p.openSubForm("add-sub")
		case "e":
			if s, ok := p.selectedSub(); ok {
				p.openSubFormEdit(s)
			}
		case "u":
			if s, ok := p.selectedSub(); ok && !p.busy {
				return p, p.updateSub(s.ID)
			}
		case "U":
			if !p.busy && len(p.subs) > 0 {
				return p, p.updateAll()
			}
		case " ":
			if s, ok := p.selectedSub(); ok {
				if err := p.app.Subs.SetEnabled(s.ID, !s.Enabled); err != nil {
					p.err = err
				}
				p.reload()
			}
		case "d":
			if s, ok := p.selectedSub(); ok {
				p.confirm = components.NewConfirm("delete-sub",
					fmt.Sprintf("删除订阅 %q 及其全部节点？", s.Name))
				p.confirmID = s.ID
				p.mode = profilesConfirm
			}
		case "enter":
			if s, ok := p.selectedSub(); ok {
				p.cur = s
				p.resetNodeView()
				p.mode = profilesNodes
				p.reloadNodes()
			}
		case "n":
			p.cur = nil
			p.resetNodeView()
			p.mode = profilesNodes
			p.reloadNodes()
		case "r":
			p.reload()
		}
		return p, nil
	}
}

// handleNodesKey 节点视图按键。
func (p *Profiles) handleNodesKey(msg tea.KeyMsg, key string) (Page, tea.Cmd) {
	// 搜索输入态优先
	if p.searchMode {
		switch key {
		case "esc":
			p.searchMode = false
			p.searchInput.Blur()
		case "enter":
			p.search = strings.TrimSpace(p.searchInput.Value())
			p.searchMode = false
			p.searchInput.Blur()
			p.applyNodeFilter()
		default:
			p.searchInput, _ = p.searchInput.Update(msg)
		}
		return p, nil
	}
	if consumed, cmd := p.list.Update(msg); consumed {
		return p, cmd
	}
	switch key {
	case "esc":
		p.mode = profilesSubs
		p.reload()
	case "r":
		p.reloadNodes()
	case "/":
		p.searchMode = true
		p.searchInput.SetValue(p.search)
		p.searchInput.Focus()
	case "p":
		p.cycleProtoFilter()
	case "S":
		p.cycleSubFilter()
	case "o":
		if p.sortBy == "name" {
			p.sortBy = "latency"
		} else {
			p.sortBy = "name"
		}
		p.applyNodeFilter()
	case "s":
		if n, ok := p.selectedNode(); ok && !p.busy {
			return p, p.testLatency([]int64{n.ID}, "正在测速…")
		}
	case "A":
		if !p.busy && len(p.filtered) > 0 {
			ids := make([]int64, 0, len(p.filtered))
			for _, n := range p.filtered {
				ids = append(ids, n.ID)
			}
			return p, p.testLatency(ids, fmt.Sprintf("正在测速 %d 个节点…", len(ids)))
		}
	case " ":
		if n, ok := p.selectedNode(); ok {
			if err := p.app.Proxy.SetEnabled(n.ID, !n.Enabled); err != nil {
				p.err = err
			}
			p.reloadNodes()
		}
	case "i":
		p.openImportForm()
	case "f":
		p.openNodeForm(nil)
	case "e":
		if n, ok := p.selectedNode(); ok {
			if n.SubscriptionID != config.ManualSubscriptionID {
				p.err = fmt.Errorf("订阅节点由订阅管理，仅手动节点可编辑")
				return p, nil
			}
			p.openNodeForm(n)
		}
	case "D":
		if n, ok := p.selectedNode(); ok {
			if n.SubscriptionID != config.ManualSubscriptionID {
				p.err = fmt.Errorf("订阅节点由订阅管理，仅手动节点可删除")
				return p, nil
			}
			p.confirm = components.NewConfirm("delete-node",
				fmt.Sprintf("删除节点 %q？", n.Name))
			p.confirmID = n.ID
			p.mode = profilesConfirm
		}
	}
	return p, nil
}

// --- 数据加载 ---

func (p *Profiles) reload() {
	subs, err := p.app.DB.ListSubscriptions()
	p.subs = subs
	if err != nil {
		p.err = err // 仅查询失败时覆盖，避免清掉尚未确认的操作错误
	}
	p.list.Items = nil
	for _, s := range subs {
		updated := "从未更新"
		if !s.LastUpdated.IsZero() {
			updated = s.LastUpdated.Format("2006-01-02 15:04")
		}
		state := "启用"
		if !s.Enabled {
			state = "停用"
		}
		url := s.URL
		if len(url) > 40 {
			url = url[:37] + "…"
		}
		p.list.Items = append(p.list.Items,
			fmt.Sprintf("%-18s %3d节点 %-16s %-4s %-14s %s", s.Name, s.NodeCount, updated, state, s.DownloadStrategy, url))
	}
	p.clampCursor(len(subs))
}

func (p *Profiles) reloadNodes() {
	var (
		nodes []*config.Node
		err   error
	)
	if p.cur != nil {
		nodes, err = p.app.DB.ListNodes(p.cur.ID)
	} else {
		nodes, err = p.app.DB.ListNodes(0)
	}
	p.nodes, p.err = nodes, err
	p.applyNodeFilter()
}

// applyNodeFilter 按搜索/协议/订阅过滤并排序，重建列表项。
func (p *Profiles) applyNodeFilter() {
	subIDByName := map[string]int64{}
	for _, s := range p.subs {
		subIDByName[s.Name] = s.ID
	}
	search := strings.ToLower(p.search)

	filtered := make([]*config.Node, 0, len(p.nodes))
	for _, n := range p.nodes {
		if search != "" &&
			!strings.Contains(strings.ToLower(n.Name), search) &&
			!strings.Contains(strings.ToLower(n.Server), search) {
			continue
		}
		if p.protoFilter != "" && n.Protocol != p.protoFilter {
			continue
		}
		if p.subFilter != "" {
			if p.subFilter == "手动" {
				if n.SubscriptionID != config.ManualSubscriptionID {
					continue
				}
			} else if subIDByName[p.subFilter] != n.SubscriptionID {
				continue
			}
		}
		filtered = append(filtered, n)
	}
	if p.sortBy == "latency" {
		sortNodesByLatency(filtered)
	} else {
		sortNodesByName(filtered)
	}
	p.filtered = filtered

	showSub := p.cur == nil
	p.list.Items = nil
	for _, n := range filtered {
		lat := "  -  "
		if !n.LastTested.IsZero() || n.LatencyMS >= 0 {
			if n.LatencyMS >= 0 {
				lat = fmt.Sprintf("%4dms", n.LatencyMS)
			} else {
				lat = "失败"
			}
		}
		state := "启用"
		if !n.Enabled {
			state = "停用"
		}
		tls := ""
		if n.TLS {
			tls = "tls"
		}
		sub := ""
		if showSub {
			if n.SubscriptionID == config.ManualSubscriptionID {
				sub = "[手动] "
			} else {
				for _, s := range p.subs {
					if s.ID == n.SubscriptionID {
						sub = "[" + s.Name + "] "
						break
					}
				}
			}
		}
		p.list.Items = append(p.list.Items,
			fmt.Sprintf("%s%-22s %-12s %6s %-24s:%-5d %-3s %-11s %s",
				sub, n.Name, n.Protocol, lat, n.Server, n.Port, tls, n.Transport, state))
	}
	p.clampCursor(len(filtered))
}

func (p *Profiles) resetNodeView() {
	p.search, p.searchMode = "", false
	p.searchInput.Blur()
	p.protoFilter, p.subFilter, p.sortBy = "", "", "name"
}

func (p *Profiles) cycleProtoFilter() {
	protos := []string{""}
	seen := map[string]bool{}
	for _, n := range p.nodes {
		if !seen[n.Protocol] {
			seen[n.Protocol] = true
			protos = append(protos, n.Protocol)
		}
	}
	p.protoFilter = nextCycle(protos, p.protoFilter)
	p.applyNodeFilter()
}

func (p *Profiles) cycleSubFilter() {
	opts := []string{"", "手动"}
	for _, s := range p.subs {
		opts = append(opts, s.Name)
	}
	p.subFilter = nextCycle(opts, p.subFilter)
	p.applyNodeFilter()
}

func nextCycle(opts []string, cur string) string {
	idx := 0
	for i, o := range opts {
		if o == cur {
			idx = i
			break
		}
	}
	return opts[(idx+1)%len(opts)]
}

func sortNodesByName(nodes []*config.Node) {
	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0 && nodes[j].Name < nodes[j-1].Name; j-- {
			nodes[j], nodes[j-1] = nodes[j-1], nodes[j]
		}
	}
}

// sortNodesByLatency 已测通的按延迟升序，未测/失败的按名称排在其后。
func sortNodesByLatency(nodes []*config.Node) {
	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0; j-- {
			a, b := nodes[j], nodes[j-1]
			if latencyLess(a, b) {
				nodes[j], nodes[j-1] = nodes[j-1], nodes[j]
				continue
			}
			break
		}
	}
}

func latencyLess(a, b *config.Node) bool {
	aOK, bOK := a.LatencyMS >= 0, b.LatencyMS >= 0
	switch {
	case aOK && bOK:
		return a.LatencyMS < b.LatencyMS
	case aOK != bOK:
		return aOK // 测通的排前面
	default:
		return a.Name < b.Name
	}
}

func (p *Profiles) clampCursor(n int) {
	if p.list.Cursor >= n {
		p.list.Cursor = n - 1
	}
	if p.list.Cursor < 0 {
		p.list.Cursor = 0
	}
}

func (p *Profiles) selectedSub() (*config.Subscription, bool) {
	if p.list.Cursor < 0 || p.list.Cursor >= len(p.subs) {
		return nil, false
	}
	return p.subs[p.list.Cursor], true
}

func (p *Profiles) selectedNode() (*config.Node, bool) {
	if p.list.Cursor < 0 || p.list.Cursor >= len(p.filtered) {
		return nil, false
	}
	return p.filtered[p.list.Cursor], true
}

// --- 表单 ---

func (p *Profiles) openSubForm(kind string) {
	p.formKind = kind
	p.form = components.NewForm("添加订阅",
		[]string{"名称", "URL", "User-Agent", "下载通道", "节点过滤器", "排序", "更新后测速", "清理失败节点"},
		[]string{"name", "url", "ua", "download_strategy", "filter", "sort", "autotest", "autoclean"},
		[]string{"机场 A", "https://example.com/sub?token=...", "可选，默认使用 Karing 格式", "prefer_proxy（代理失败时直连）", "正则，逗号分隔，!排除", "name 或 latency", "true/false", "true/false"})
	p.form.Reset()
	p.err = nil
	p.mode = profilesForm
}

func (p *Profiles) openSubFormEdit(s *config.Subscription) {
	p.formKind = "edit-sub"
	p.editID = s.ID
	p.form = components.NewForm("编辑订阅",
		[]string{"名称", "URL", "User-Agent", "下载通道", "节点过滤器", "排序", "更新后测速", "清理失败节点"},
		[]string{"name", "url", "ua", "download_strategy", "filter", "sort", "autotest", "autoclean"},
		[]string{"", "", "", "prefer_proxy/prefer_direct/only_proxy/only_direct", "", "name/latency", "true/false", "true/false"})
	p.form.SetValueByKey("name", s.Name)
	p.form.SetValueByKey("url", s.URL)
	p.form.SetValueByKey("ua", s.UserAgent)
	p.form.SetValueByKey("download_strategy", s.DownloadStrategy)
	p.form.SetValueByKey("filter", s.NodeFilter)
	p.form.SetValueByKey("sort", s.SortBy)
	p.form.SetValueByKey("autotest", strconv.FormatBool(s.AutoTest))
	p.form.SetValueByKey("autoclean", strconv.FormatBool(s.AutoClean))
	p.err = nil
	p.mode = profilesForm
}

// nodeFormKeys 手动节点表单的字段定义。
var nodeFormKeys = []string{"name", "protocol", "server", "port", "cred", "method", "sni", "transport", "host", "path", "tls"}

func (p *Profiles) openNodeForm(n *config.Node) {
	p.form = components.NewForm("手动节点",
		[]string{"名称", "协议", "服务器", "端口", "凭据(UUID/密码)", "加密方法", "SNI", "传输", "Host", "Path", "TLS"},
		nodeFormKeys,
		[]string{"留空用 server:port", "shadowsocks/vmess/vless/trojan/hysteria2", "地址", "整数",
			"按协议填 UUID 或密码", "ss 加密 / vmess security", "TLS 域名", "空/ws/grpc/http/httpupgrade",
			"ws/http 的 Host 头", "ws/http 的路径", "true/false，trojan/hy2 自动 true"})
	if n != nil {
		p.formKind = "edit-node"
		p.editID = n.ID
		p.editNode = n
		p.form.SetValueByKey("name", n.Name)
		p.form.SetValueByKey("protocol", n.Protocol)
		p.form.SetValueByKey("server", n.Server)
		p.form.SetValueByKey("port", strconv.Itoa(n.Port))
		p.form.SetValueByKey("cred", nodeCred(n))
		p.form.SetValueByKey("method", strMeta(n.Metadata, "method"))
		p.form.SetValueByKey("sni", strMeta(n.Metadata, "sni"))
		p.form.SetValueByKey("transport", n.Transport)
		p.form.SetValueByKey("host", strMeta(n.Metadata, "host"))
		p.form.SetValueByKey("path", strMeta(n.Metadata, "path"))
		if n.TLS {
			p.form.SetValueByKey("tls", "true")
		}
	} else {
		p.formKind = "add-node"
		p.editNode = nil
	}
	p.err = nil
	p.mode = profilesForm
}

func (p *Profiles) openImportForm() {
	p.formKind = "import-links"
	p.form = components.NewForm("导入分享链接",
		[]string{"链接（可多条，空白分隔）"},
		[]string{"links"},
		[]string{"ss://… vmess://… trojan://…"})
	p.form.Reset()
	p.err = nil
	p.mode = profilesForm
}

// formBackMode 表单取消后应返回的视图。
func (p *Profiles) formBackMode() profilesMode {
	if p.formKind == "add-sub" || p.formKind == "edit-sub" {
		return profilesSubs
	}
	return profilesNodes
}

func (p *Profiles) submitForm() {
	switch p.formKind {
	case "add-sub", "edit-sub":
		p.submitSubForm()
	case "add-node", "edit-node":
		p.submitNodeForm()
	case "import-links":
		p.submitImportForm()
	}
}

func (p *Profiles) submitSubForm() {
	name := p.form.ValueByKey("name")
	rawURL := p.form.ValueByKey("url")
	ua := p.form.ValueByKey("ua")
	strategy := strings.TrimSpace(p.form.ValueByKey("download_strategy"))
	if strategy == "" {
		strategy = config.DefaultDownloadStrategy
	}
	filter, sortBy := p.form.ValueByKey("filter"), p.form.ValueByKey("sort")
	autoTest, _ := strconv.ParseBool(p.form.ValueByKey("autotest"))
	autoClean, _ := strconv.ParseBool(p.form.ValueByKey("autoclean"))
	if name == "" || rawURL == "" {
		p.err = fmt.Errorf("名称与 URL 不能为空")
		return
	}
	if _, err := config.NormalizeDownloadStrategy(strategy); err != nil {
		p.err = err
		return
	}
	if p.formKind == "add-sub" {
		added, err := p.app.Subs.AddWithStrategy(context.Background(), name, rawURL, ua, strategy)
		if err != nil {
			p.err = err
			return
		}
		added.NodeFilter, added.SortBy, added.AutoTest, added.AutoClean = filter, sortBy, autoTest, autoClean
		if err := p.app.Subs.Edit(added); err != nil {
			p.err = err
			return
		}
		p.status = "订阅已添加"
	} else {
		s := p.subByID(p.editID)
		if s == nil {
			p.err = fmt.Errorf("订阅不存在")
			p.mode = profilesSubs
			return
		}
		s.Name, s.URL, s.UserAgent, s.DownloadStrategy, s.NodeFilter, s.SortBy, s.AutoTest, s.AutoClean = name, rawURL, ua, strategy, filter, sortBy, autoTest, autoClean
		if err := p.app.Subs.Edit(s); err != nil {
			p.err = err
			return
		}
		p.status = "订阅已保存"
	}
	p.err = nil
	p.mode = profilesSubs
	p.reload()
}

func (p *Profiles) submitNodeForm() {
	node := nodeFromForm(p.form)
	if node == nil {
		p.err = fmt.Errorf("协议、服务器、端口为必填项")
		return
	}
	if p.formKind == "edit-node" {
		if p.editNode == nil {
			p.mode = profilesNodes
			return
		}
		node.ID = p.editNode.ID
		node.Enabled = p.editNode.Enabled
		node.LatencyMS = p.editNode.LatencyMS
		node.LastTested = p.editNode.LastTested
	}
	if err := p.app.Proxy.SaveManual(node); err != nil {
		p.err = err
		return
	}
	p.err = nil
	p.status = "节点已保存"
	p.mode = profilesNodes
	p.reloadNodes()
}

func (p *Profiles) submitImportForm() {
	content := p.form.ValueByKey("links")
	if strings.TrimSpace(content) == "" {
		p.err = fmt.Errorf("请粘贴分享链接")
		return
	}
	added, err := p.app.Proxy.AddFromLinks(content)
	if err != nil {
		p.err = err
		return
	}
	p.err = nil
	p.status = fmt.Sprintf("已导入 %d 个节点", added)
	p.mode = profilesNodes
	p.reloadNodes()
}

// nodeFromForm 由表单构建手动节点；必填缺失返回 nil。
func nodeFromForm(f components.Form) *config.Node {
	protocol := strings.ToLower(strings.TrimSpace(f.ValueByKey("protocol")))
	server := strings.TrimSpace(f.ValueByKey("server"))
	port, err := strconv.Atoi(strings.TrimSpace(f.ValueByKey("port")))
	if protocol == "" || server == "" || err != nil || port <= 0 || port > 65535 {
		return nil
	}
	cred := f.ValueByKey("cred")
	method := f.ValueByKey("method")
	meta := map[string]any{}
	switch protocol {
	case "shadowsocks":
		meta["method"], meta["password"] = method, cred
	case "vmess":
		meta["uuid"] = cred
		if method != "" {
			meta["method"] = method
		}
	case "vless":
		meta["uuid"] = cred
	case "trojan", "hysteria2":
		meta["password"] = cred
	}
	if v := f.ValueByKey("sni"); v != "" {
		meta["sni"] = v
	}
	if v := f.ValueByKey("host"); v != "" {
		meta["host"] = v
	}
	if v := f.ValueByKey("path"); v != "" {
		meta["path"] = v
	}
	transport := strings.ToLower(strings.TrimSpace(f.ValueByKey("transport")))
	switch transport {
	case "ws", "grpc", "http", "httpupgrade":
	default:
		transport = ""
	}
	tls := protocol == "trojan" || protocol == "hysteria2" ||
		strings.EqualFold(f.ValueByKey("tls"), "true") || f.ValueByKey("tls") == "1"
	return &config.Node{
		Name:      f.ValueByKey("name"),
		Protocol:  protocol,
		Server:    server,
		Port:      port,
		TLS:       tls,
		Transport: transport,
		Metadata:  meta,
	}
}

// nodeCred 取节点的 UUID 或密码（编辑表单预填用）。
func nodeCred(n *config.Node) string {
	if v := strMeta(n.Metadata, "uuid"); v != "" {
		return v
	}
	return strMeta(n.Metadata, "password")
}

func strMeta(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// --- 异步操作 ---

func (p *Profiles) updateSub(id int64) tea.Cmd {
	p.busy = true
	p.status = "正在更新订阅…"
	p.err = nil
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, err := p.app.Subs.Update(ctx, id)
		return actionDoneMsg{Action: "update-sub", Err: err}
	}
}

func (p *Profiles) updateAll() tea.Cmd {
	p.busy = true
	p.status = "正在更新全部订阅…"
	p.err = nil
	ids := make([]int64, 0, len(p.subs))
	for _, s := range p.subs {
		ids = append(ids, s.ID)
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		var firstErr error
		for _, id := range ids {
			if _, err := p.app.Subs.Update(ctx, id); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return actionDoneMsg{Action: "update-all", Err: firstErr}
	}
}

func (p *Profiles) testLatency(ids []int64, doing string) tea.Cmd {
	p.busy = true
	p.status = doing
	p.err = nil
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		_, err := p.app.Proxy.TestLatency(ctx, ids, "", 3000)
		return actionDoneMsg{Action: "test-latency", Err: err}
	}
}

func (p *Profiles) onActionDone(msg actionDoneMsg) (Page, tea.Cmd) {
	p.busy = false
	if msg.Err != nil {
		p.err = msg.Err
		p.status = ""
	} else {
		p.err = nil
		switch msg.Action {
		case "update-sub":
			p.status = "订阅更新完成"
		case "update-all":
			p.status = "全部订阅更新完成"
		case "test-latency":
			p.status = "测速完成"
		}
	}
	if p.mode == profilesNodes {
		p.reloadNodes()
	} else {
		p.reload()
	}
	return p, nil
}

func (p *Profiles) onConfirm(msg components.ConfirmMsg) (Page, tea.Cmd) {
	if p.mode != profilesConfirm {
		return p, nil
	}
	p.mode = profilesNodes
	if msg.Confirmed {
		switch msg.ID {
		case "delete-sub":
			p.mode = profilesSubs
			if err := p.app.Subs.Delete(p.confirmID); err != nil {
				p.err = err
			} else {
				p.err = nil
				p.status = "订阅已删除"
				if p.cur != nil && p.cur.ID == p.confirmID {
					p.cur = nil
				}
			}
			p.reload()
		case "delete-node":
			if err := p.app.Proxy.Delete(p.confirmID); err != nil {
				p.err = err
			} else {
				p.err = nil
				p.status = "节点已删除"
			}
			p.reloadNodes()
		}
	}
	return p, nil
}

// --- 渲染 ---

func (p *Profiles) View() string {
	switch p.mode {
	case profilesForm:
		return p.form.View()
	case profilesConfirm:
		return p.confirm.View()
	case profilesNodes:
		return p.nodesView()
	default:
		head := styles.Title.Render("订阅") + "\n"
		body := p.list.View("暂无订阅，按 a 添加。")
		return head + body + p.statusLine() +
			styles.Dim.Render("\na 添加 · e 编辑 · u 更新 · U 全部更新 · space 启停 · d 删除 · enter 节点 · n 全部节点 · r 刷新")
	}
}

func (p *Profiles) nodesView() string {
	scope := "全部节点"
	if p.cur != nil {
		scope = "订阅 " + p.cur.Name
	}
	title := styles.Title.Render("节点") +
		styles.Dim.Render(fmt.Sprintf(" — %s（%d/%d 个）", scope, len(p.filtered), len(p.nodes)))

	filters := fmt.Sprintf("协议:%s · 订阅:%s · 排序:%s",
		orDash(p.protoFilter, "全部"), orDash(p.subFilter, "全部"),
		map[string]string{"name": "名称", "latency": "延迟"}[p.sortBy])
	if p.search != "" {
		filters += fmt.Sprintf(" · 搜索:%q", p.search)
	}

	var body string
	if p.searchMode {
		body = "\n搜索: " + p.searchInput.View() + styles.Dim.Render("  (Enter 应用 · Esc 取消)")
	} else {
		body = "\n" + p.list.View("无节点。i 导入链接 · f 手动添加 · 或返回更新订阅。")
	}
	return title + "\n" + styles.Dim.Render(filters) + body + p.statusLine() +
		styles.Dim.Render("\n/ 搜索 · p 协议 · S 订阅 · o 排序 · s 测速选中 · A 测速全部 · space 启停 · i 导入 · f 添加 · e 编辑 · D 删除 · esc 返回")
}

func orDash(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (p *Profiles) statusLine() string {
	var b string
	if p.busy {
		b += "\n" + styles.Accent.Render(p.status)
	} else if p.status != "" {
		b += "\n" + styles.Ok.Render(p.status)
	}
	if p.err != nil {
		b += "\n" + styles.Err.Render("错误: "+p.err.Error())
	}
	return b
}

func (p *Profiles) subByID(id int64) *config.Subscription {
	for _, s := range p.subs {
		if s.ID == id {
			return s
		}
	}
	return nil
}
