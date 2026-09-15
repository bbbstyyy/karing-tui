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
	"github.com/bbbstyyy/karing-tui/internal/subscription"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	"github.com/bbbstyyy/karing-tui/internal/validation"
)

// profilesMode Profiles 页的视图模式。
type profilesMode int

const (
	profilesSubs    profilesMode = iota // 订阅列表
	profilesNodes                       // 节点列表（单订阅或全部）
	profilesForm                        // 表单（订阅/节点/导入链接）
	profilesConfirm                     // 删除确认
	profilesResults
)

// Profiles 管理订阅与节点：订阅增删改与更新、节点搜索过滤排序、
// 手动节点管理、延迟测试。
type Profiles struct {
	base
	mode             profilesMode
	subs             []*config.Subscription
	cur              *config.Subscription // nodes 视图对应的订阅；nil 表示全部节点
	nodes            []*config.Node       // nodes 视图的原始列表
	list             components.SimpleList
	subList          components.SimpleList
	nodeList         components.SimpleList
	searchBefore     string
	searchListBefore components.SimpleList
	results          []itemResult
	resultsKind      string
	resultList       components.SimpleList
	resultBack       profilesMode

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

func (p *Profiles) Title() string { return "订阅与节点" }

// Editing 表单或搜索输入状态时拦截全局键位。
func (p *Profiles) Editing() bool {
	return p.detailActive || p.mode == profilesForm || p.mode == profilesConfirm || p.searchMode
}

func (p *Profiles) Init() tea.Cmd { return nil }

func (p *Profiles) Update(msg tea.Msg) (Page, tea.Cmd) {
	if !p.Editing() {
		if handled, cmd := p.handleRetry(msg); handled {
			return p, cmd
		}
	}
	if p.detailActive || !p.Editing() {
		if p.handleDetails(msg, p.err) {
			return p, nil
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.SetSize(msg.Width, msg.Height)
		switch p.mode {
		case profilesNodes:
			p.applyNodeFilter()
		case profilesSubs:
			p.reload()
		}
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
	if p.mode == profilesForm {
		return p, p.form.Update(msg)
	}
	if p.searchMode {
		p.searchInput, _ = p.searchInput.Update(msg)
		p.search = strings.TrimSpace(p.searchInput.Value())
		p.applyNodeFilter()
	}
	return p, nil
}

func (p *Profiles) handleKey(msg tea.KeyMsg) (Page, tea.Cmd) {
	key := commandKey(p, msg)
	switch p.mode {
	case profilesResults:
		if key == "enter" || key == "alt+enter" {
			if line, ok := p.resultList.Selected(); ok {
				p.openDetails("逐项结果", line)
			}
			return p, nil
		}
		if key == "esc" || key == "q" {
			p.mode = p.resultBack
			p.reload()
			return p, nil
		}
		_, cmd := p.resultList.Update(msg)
		return p, cmd
	case profilesForm:
		action, cmd := p.form.Handle(msg)
		switch action {
		case "cancel":
			p.mode = p.formBackMode()
			p.err = nil
		case "save", "save-update":
			p.submitForm()
			if action == "save-update" && p.mode == profilesSubs && p.err == nil {
				return p, p.updateSub(p.editID)
			}
		}
		return p, cmd

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
		case "f":
			if !p.busy {
				return p, p.retryFailed()
			}
		case "v":
			p.openResults()
		case " ":
			if s, ok := p.selectedSub(); ok {
				if err := p.app.Subs.SetEnabled(s.ID, !s.Enabled); err != nil {
					p.err = err
				} else {
					p.app.MarkConfigDirty()
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
				p.subList = p.list
				p.list = p.nodeList
				p.cur = s
				p.resetNodeView()
				p.mode = profilesNodes
				p.reloadNodes()
			}
		case "alt+enter":
			if sub, ok := p.selectedSub(); ok {
				p.openDetails("订阅详情", subscriptionDetails(sub))
			}
		case "n", "]", "[":
			p.subList = p.list
			p.list = p.nodeList
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
			p.search = p.searchBefore
			p.list = p.searchListBefore
			p.applyNodeFilter()
		case "enter":
			p.search = strings.TrimSpace(p.searchInput.Value())
			p.searchMode = false
			p.searchInput.Blur()
			p.applyNodeFilter()
		default:
			p.searchInput, _ = p.searchInput.Update(msg)
			p.search = strings.TrimSpace(p.searchInput.Value())
			p.applyNodeFilter()
		}
		return p, nil
	}
	if consumed, cmd := p.list.Update(msg); consumed {
		return p, cmd
	}
	switch key {
	case "enter", "alt+enter":
		if node, ok := p.selectedNode(); ok {
			p.openDetails("节点详情", nodeDetails(node))
		}
	case "esc", "[", "]":
		p.nodeList = p.list
		p.list = p.subList
		p.mode = profilesSubs
		p.reload()
	case "r":
		p.reloadNodes()
	case "/":
		p.searchBefore = p.search
		p.searchListBefore = p.list
		p.searchMode = true
		p.searchInput.SetValue(p.search)
		p.searchInput.Focus()
	case "c":
		p.search, p.protoFilter, p.subFilter = "", "", ""
		p.applyNodeFilter()
	case "v":
		p.openResults()
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
	case "t":
		if n, ok := p.selectedNode(); ok && !p.busy {
			return p, p.testLatency([]int64{n.ID}, "正在测速…")
		}
	case "T":
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
			} else {
				p.app.MarkConfigDirty()
			}
			p.reloadNodes()
		}
	case "i":
		p.openImportForm()
	case "a":
		p.openNodeForm(nil)
	case "e":
		if n, ok := p.selectedNode(); ok {
			if n.SubscriptionID != config.ManualSubscriptionID {
				p.err = fmt.Errorf("订阅节点由订阅管理，仅手动节点可编辑")
				return p, nil
			}
			p.openNodeForm(n)
		}
	case "D", "d":
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
	if p.mode == profilesNodes || (p.mode == profilesForm && p.formBackMode() == profilesNodes) || (p.mode == profilesConfirm && p.confirm.ID == "delete-node") {
		p.reloadNodes()
		return
	}
	if p.mode == profilesResults {
		return
	}
	selected := p.list.SelectedKey()
	p.list.Items = nil
	p.list.Keys = nil
	for _, s := range subs {
		updated := "从未更新"
		if !s.LastUpdated.IsZero() {
			updated = s.LastUpdated.Format("2006-01-02 15:04")
		}
		state := "启用"
		if !s.Enabled {
			state = "停用"
		}
		p.list.Items = append(p.list.Items,
			fmt.Sprintf("%s %4d节点 %s %s", components.Pad(s.Name, max(12, p.mainWidth()-38)), s.NodeCount, state, updated))
		p.list.Keys = append(p.list.Keys, strconv.FormatInt(s.ID, 10))
	}
	p.list.SelectKey(selected)
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
	p.nodes = nodes
	if err != nil {
		p.err = err
	}
	p.applyNodeFilter()
}

// applyNodeFilter 按搜索/协议/订阅过滤并排序，重建列表项。
func (p *Profiles) applyNodeFilter() {
	selected := p.list.SelectedKey()
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

	p.list.Items = nil
	p.list.Keys = nil
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
		p.list.Items = append(p.list.Items,
			fmt.Sprintf("%s %s %6s %s", components.Pad(n.Name, max(10, p.mainWidth()-33)), components.Pad(n.Protocol, 12), lat, state))
		p.list.Keys = append(p.list.Keys, strconv.FormatInt(n.ID, 10))
	}
	p.list.SelectKey(selected)
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
	p.configureSubForm()
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
	p.configureSubForm()
	p.err = nil
	p.mode = profilesForm
}

// nodeFormKeys 手动节点表单的字段定义。
var nodeFormKeys = []string{"name", "protocol", "server", "port", "cred", "method", "uuid", "user", "sni", "transport", "host", "path", "tls", "key_path"}

func (p *Profiles) openNodeForm(n *config.Node) {
	p.form = components.NewForm("手动节点",
		[]string{"名称", "协议", "服务器", "端口", "凭据(UUID/密码)", "加密方法", "TUIC UUID", "用户名", "SNI", "传输", "Host", "Path", "TLS", "SSH 私钥路径"},
		nodeFormKeys,
		[]string{"留空用 server:port", "shadowsocks/vmess/vless/trojan/hysteria2", "地址", "整数",
			"VMess/VLESS 填 UUID，其余协议填密码", "ss 加密 / vmess security", "TUIC 的 UUID", "HTTP/SOCKS/SSH/Naive 用户名", "TLS 域名", "空/ws/grpc/http/httpupgrade",
			"ws/http 的 Host 头", "ws/http 的路径", "TLS 协议自动启用", "可选；文件由核心读取"})
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
		p.form.SetValueByKey("uuid", strMeta(n.Metadata, "uuid"))
		user := strMeta(n.Metadata, "username")
		if n.Protocol == "ssh" {
			user = orDash(strMeta(n.Metadata, "user"), user)
		}
		p.form.SetValueByKey("user", user)
		p.form.SetValueByKey("key_path", strMeta(n.Metadata, "private_key_path"))
		if n.Protocol == "tuic" {
			p.form.SetValueByKey("cred", strMeta(n.Metadata, "password"))
		}
		if n.TLS {
			p.form.SetValueByKey("tls", "true")
		}
	} else {
		p.formKind = "add-node"
		p.editNode = nil
	}
	p.configureNodeForm()
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
	name := strings.TrimSpace(p.form.ValueByKey("name"))
	rawURL := p.form.ValueByKey("url")
	ua := p.form.ValueByKey("ua")
	strategy := strings.TrimSpace(p.form.ValueByKey("download_strategy"))
	if strategy == "" {
		strategy = config.DefaultDownloadStrategy
	}
	filter, sortBy := p.form.ValueByKey("filter"), p.form.ValueByKey("sort")
	if err := subscription.ValidateNodeFilter(filter); err != nil {
		p.err = err
		return
	}
	autoTest, _ := strconv.ParseBool(p.form.ValueByKey("autotest"))
	autoClean, _ := strconv.ParseBool(p.form.ValueByKey("autoclean"))
	if name == "" {
		p.err = validation.New("name", "订阅名称不能为空，请填写名称")
		return
	}
	if rawURL == "" {
		p.err = validation.New("url", "订阅 URL 不能为空，请填写下载地址")
		return
	}
	if _, err := config.NormalizeDownloadStrategy(strategy); err != nil {
		p.err = validation.At("download_strategy", err)
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
		p.editID = added.ID
	} else {
		original := p.subByID(p.editID)
		if original == nil {
			p.err = fmt.Errorf("订阅不存在")
			p.mode = profilesSubs
			return
		}
		s := *original
		s.Name, s.URL, s.UserAgent, s.DownloadStrategy, s.NodeFilter, s.SortBy, s.AutoTest, s.AutoClean = name, rawURL, ua, strategy, filter, sortBy, autoTest, autoClean
		if err := p.app.Subs.Edit(&s); err != nil {
			p.err = err
			return
		}
		p.status = "订阅已保存"
	}
	p.err = nil
	p.mode = profilesSubs
	p.app.MarkConfigDirty()
	p.reload()
	p.list.SelectKey(strconv.FormatInt(p.editID, 10))
}

func (p *Profiles) submitNodeForm() {
	node := nodeFromForm(p.form)
	if node == nil {
		key := "port"
		if p.form.ValueByKey("server") == "" {
			key = "server"
		}
		if p.form.ValueByKey("protocol") == "" {
			key = "protocol"
		}
		p.err = validation.New(key, "协议、服务器、端口为必填项；端口须在 1–65535 之间")
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
		// Preserve protocol options not exposed in this form, including Reality,
		// ALPN and plugin settings on imported manual nodes.
		if node.Protocol == p.editNode.Protocol {
			meta := make(map[string]any, len(p.editNode.Metadata)+len(node.Metadata))
			for key, value := range p.editNode.Metadata {
				meta[key] = value
			}
			for _, key := range []string{"uuid", "password", "method", "sni", "host", "path", "username", "user", "private_key_path"} {
				delete(meta, key)
			}
			for key, value := range node.Metadata {
				meta[key] = value
			}
			node.Metadata = meta
		}
	}
	if err := p.app.Proxy.SaveManual(node); err != nil {
		p.err = err
		return
	}
	p.err = nil
	p.status = "节点已保存"
	p.app.MarkConfigDirty()
	p.mode = profilesNodes
	p.reloadNodes()
}

func (p *Profiles) submitImportForm() {
	content := p.form.ValueByKey("links")
	if strings.TrimSpace(content) == "" {
		p.err = fmt.Errorf("请粘贴分享链接")
		return
	}
	results := p.app.Proxy.AddFromLinksDetailed(content)
	p.results = nil
	p.resultsKind = "分享链接导入"
	added, failed := 0, 0
	for _, res := range results {
		row := itemResult{Name: fmt.Sprintf("第 %d 条 %s", res.Index, res.Name), State: "成功"}
		if res.Err != nil {
			row.State, row.Detail = "失败", res.Err.Error()
			failed++
		} else {
			added++
		}
		p.results = append(p.results, row)
	}
	p.err = nil
	if failed > 0 {
		p.err = fmt.Errorf("%d 条导入失败；v 查看逐条结果", failed)
	}
	p.status = fmt.Sprintf("分享链接导入：成功 %d · 失败 %d · v 详情", added, failed)
	if added > 0 {
		p.app.MarkConfigDirty()
	}
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
	case "tuic":
		meta["uuid"], meta["password"] = f.ValueByKey("uuid"), cred
	case "ssh":
		meta["user"], meta["password"] = f.ValueByKey("user"), cred
		if path := f.ValueByKey("key_path"); path != "" {
			meta["private_key_path"] = path
		}
	case "socks", "socks5", "http", "naive":
		meta["username"], meta["password"] = f.ValueByKey("user"), cred
	case "trojan", "hysteria2", "hysteria", "anytls", "shadowtls":
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
	tls := mandatoryTLS(protocol) ||
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
	return p.updateSubscriptions([]int64{id}, false)
}

func (p *Profiles) updateAll() tea.Cmd {
	ids := make([]int64, 0, len(p.subs))
	for _, sub := range p.subs {
		ids = append(ids, sub.ID)
	}
	return p.updateSubscriptions(ids, true)
}

func (p *Profiles) retryFailed() tea.Cmd {
	if p.resultsKind == "节点测速" {
		ids := failedIDs(p.results)
		if len(ids) > 0 {
			return p.testLatency(ids, "重试失败节点")
		}
		return nil
	}
	if p.resultsKind != "订阅更新" {
		p.status = "没有失败的订阅需要重试"
		return nil
	}
	var ids []int64
	for _, res := range p.results {
		if res.State == "失败" {
			ids = append(ids, res.ID)
		}
	}
	if len(ids) == 0 {
		p.status = "没有失败的订阅需要重试"
		return nil
	}
	return p.updateSubscriptions(ids, true)
}

func (p *Profiles) updateSubscriptions(ids []int64, enabledOnly bool) tea.Cmd {
	if p.busy || len(ids) == 0 {
		return nil
	}
	p.busy = true
	p.status = fmt.Sprintf("正在更新 %d 个订阅…", len(ids))
	p.err = nil
	p.retryTask = p.retryFailed
	return p.taskN("更新订阅", len(ids), func(report func(itemResult)) tea.Msg {
		ctx, cancel := context.WithTimeout(p.app.BackgroundContext(), 10*time.Minute)
		defer cancel()
		var results []itemResult
		for _, id := range ids {
			res := itemResult{ID: id, Name: fmt.Sprintf("订阅 %d", id), State: "成功"}
			sub, err := p.app.DB.GetSubscription(id)
			if err == nil {
				res.Name = sub.Name
				if enabledOnly && !sub.Enabled {
					res.State, res.Detail = "跳过", "订阅已停用，未发起下载"
					results = append(results, res)
					report(res)
					continue
				}
				updated, updateErr := p.app.Subs.Update(ctx, id)
				err = updateErr
				if err == nil {
					res.Detail = fmt.Sprintf("%d 节点", updated.NodeCount)
					p.app.MarkConfigDirty()
				}
			}
			if err != nil {
				res.State, res.Detail = "失败", err.Error()
			}
			results = append(results, res)
			report(res)
		}
		return actionDoneMsg{Action: "update-subscriptions", Results: results, Err: resultError(results)}
	})
}

func (p *Profiles) testLatency(ids []int64, doing string) tea.Cmd {
	if p.busy || len(ids) == 0 {
		return nil
	}
	p.busy, p.status, p.err = true, doing, nil
	p.retryTask = p.retryFailed
	return p.taskN("节点测速", len(ids), func(report func(itemResult)) tea.Msg {
		return latencyResults(p.app, ids, report)
	})
}

func (p *Profiles) onActionDone(msg actionDoneMsg) (Page, tea.Cmd) {
	if !p.accept(msg) {
		return p, nil
	}
	p.busy = false
	p.err = msg.Err
	if msg.Action == "update-subscriptions" {
		p.resultsKind = "订阅更新"
		p.results = msg.Results
		p.status = "订阅更新：" + resultSummary(msg.Results) + " · v 详情"
	} else {
		if msg.Action == "test-latency" {
			p.resultsKind = "节点测速"
			p.results = msg.Results
		}
		p.status = msg.Data
	}
	p.reload()
	return p, nil
}

func (p *Profiles) openResults() {
	if len(p.results) == 0 {
		p.status = "暂无订阅更新结果，u/U 开始更新"
		return
	}
	p.resultBack = p.mode
	p.resultList = components.SimpleList{}
	for _, res := range p.results {
		p.resultList.Items = append(p.resultList.Items, res.State+" · "+res.Name+" · "+res.Detail)
	}
	p.mode = profilesResults
}

func (p *Profiles) onConfirm(msg components.ConfirmMsg) (Page, tea.Cmd) {
	if p.mode != profilesConfirm {
		return p, nil
	}
	p.mode = profilesNodes
	if msg.ID == "delete-sub" {
		p.mode = profilesSubs
	}
	if msg.Confirmed {
		switch msg.ID {
		case "delete-sub":
			p.mode = profilesSubs
			if err := p.app.Subs.Delete(p.confirmID); err != nil {
				p.err = err
			} else {
				p.err = nil
				p.status = "订阅已删除"
				p.app.MarkConfigDirty()
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
				p.app.MarkConfigDirty()
			}
			p.reloadNodes()
		}
	}
	return p, nil
}

// --- 渲染 ---

func (p *Profiles) View() string {
	p.table()
	p.preview = ""
	if p.mode == profilesSubs {
		if sub, ok := p.selectedSub(); ok {
			p.preview = subscriptionDetails(sub)
		}
	}
	if p.mode == profilesNodes {
		if n, ok := p.selectedNode(); ok {
			p.preview = nodeDetails(n)
		}
	}
	if p.mode == profilesResults {
		p.preview, _ = p.resultList.Selected()
	}

	if p.detailActive {
		return p.detailsView()
	}
	switch p.mode {
	case profilesForm:
		return p.formView(&p.form, p.err)
	case profilesConfirm:
		return p.confirmView(&p.confirm)
	case profilesResults:
		return p.listView(&p.resultList, p.resultsKind+"结果 · "+resultSummary(p.results), "暂无结果", "", "Enter 完整结果 · Esc 返回 · 订阅列表 f 仅重试失败项")
	case profilesNodes:
		return p.nodesView()
	default:
		return p.listView(&p.list, "[订阅]  ] 全部节点", "暂无订阅，按 a 添加。", p.statusLine(),
			Hints(p))
	}
}

func (p *Profiles) nodesView() string {
	scope := "全部节点"
	if p.cur != nil {
		scope = p.cur.Name
	}
	head := fmt.Sprintf("订阅  [节点: %s] · %d/%d · [/] 切换\n协议:%s · 订阅:%s · 排序:%s · c 清除", scope, len(p.filtered), len(p.nodes),
		orDash(p.protoFilter, "全部"), orDash(p.subFilter, "全部"), orDash(p.sortBy, "name"))
	if p.searchMode {
		p.searchInput.Width = max(5, p.width-26)
		head += "\n搜索: " + p.searchInput.View() + " · Enter 完成 / Esc 撤销"
	} else if p.search != "" {
		head += " · 搜索:" + p.search
	}
	return p.listView(&p.list, head, "无匹配节点。c 清除过滤 · i 导入 · a 添加。", p.statusLine(),
		Hints(p))
}

func orDash(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (p *Profiles) statusLine() string {
	return p.feedback(p.status, p.err)
}

func (p *Profiles) subByID(id int64) *config.Subscription {
	for _, s := range p.subs {
		if s.ID == id {
			return s
		}
	}
	return nil
}
