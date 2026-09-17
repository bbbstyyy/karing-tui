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
	"github.com/bbbstyyy/karing-tui/internal/clashapi"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
)

// groupsMode Groups 页的视图模式。
type groupsMode int

const (
	groupsList    groupsMode = iota // 组列表
	groupsDetail                    // 组详情（成员与当前选中）
	groupsPick                      // 成员勾选
	groupsForm                      // 新建/编辑组表单
	groupsConfirm                   // 删除确认
)

// pickCandidate 成员勾选候选项。
//
// search 是 label 的预计算小写副本（C10）：勾选列表每敲一个字符就对全部候选做一次
// 不区分大小写的包含匹配，把 ToLower 挪到候选列表建立时，而不是在按键路径上对
// 同一批 label 反复算（5000 候选 × 每键 1 次分配）。构造必须走 newPickCandidate——
// search 为空的候选将永远匹配不到任何查询词。
type pickCandidate struct {
	key    string // "all" / "group:<id>" / "node:<id>"
	label  string
	search string // strings.ToLower(label)，与 label 同源同变
}

func newPickCandidate(key, label string) pickCandidate {
	return pickCandidate{key: key, label: label, search: strings.ToLower(label)}
}

// Groups 代理组管理页：组 CRUD、成员勾选（跨订阅节点/嵌套组/全部节点）、
// select 组当前选中持久化、组成员测速。
type Groups struct {
	base
	mode           groupsMode
	groups         []*config.ProxyGroup
	nodes          []*config.Node
	subs           []*config.Subscription
	cur            *config.ProxyGroup
	list           components.SimpleList
	groupList      components.SimpleList
	members        []config.ProxyGroupMember
	runtime        map[string]clashapi.ProxyInfo
	runtimeAt      time.Time
	pickSearch     textinput.Model
	pickTyping     bool
	pickQuery      string
	pickBefore     string
	pickBeforeList components.SimpleList

	// 成员勾选状态
	pickList  components.SimpleList
	cands     []pickCandidate
	pickSet   map[string]bool
	pickOrder []string

	form     components.Form
	formKind string // add-group / edit-group
	editID   int64

	confirm   components.Confirm
	confirmID int64

	err    error
	status string
	busy   bool
}

// NewGroups 创建 Proxy Groups 页。
func NewGroups(app *application.App) *Groups {
	return &Groups{base: base{app: app}}
}

func (g *Groups) Title() string { return "代理组" }

func (g *Groups) Editing() bool {
	return g.detailActive || g.mode == groupsForm || g.mode == groupsPick || g.mode == groupsConfirm
}

func (g *Groups) Init() tea.Cmd { return nil }

func (g *Groups) Update(msg tea.Msg) (Page, tea.Cmd) {
	if !g.Editing() {
		if handled, cmd := g.handleRetry(msg); handled {
			return g, cmd
		}
	}
	if g.detailActive || !g.Editing() {
		if g.handleDetails(msg, g.err) {
			return g, nil
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		g.SetSize(msg.Width, msg.Height)
		return g, nil
	case ActivateMsg:
		g.reload()
		return g, g.fetchRuntime()
	case groupRuntimeMsg:
		if msg.startedAt.Equal(g.app.Core.Status().StartedAt) {
			g.runtime = msg.proxies
			g.runtimeAt = msg.startedAt
		}
		return g, nil
	case actionDoneMsg:
		return g.onActionDone(msg)
	case components.ConfirmMsg:
		return g.onConfirm(msg)
	case tea.KeyMsg:
		return g.handleKey(msg)
	}
	if g.mode == groupsForm {
		return g, g.form.Update(msg)
	}
	return g, nil
}

func (g *Groups) handleKey(msg tea.KeyMsg) (Page, tea.Cmd) {
	key := commandKey(g, msg)
	switch g.mode {
	case groupsForm:
		action, cmd := g.form.Handle(msg)
		switch action {
		case "cancel":
			g.mode = groupsList
		case "save":
			g.submitForm()
		}
		return g, cmd

	case groupsConfirm:
		if consumed, cmd := g.confirm.Update(msg); consumed {
			return g, cmd
		}
		return g, nil

	case groupsPick:
		if g.pickTyping {
			switch key {
			case "enter":
				g.pickTyping = false
				g.pickSearch.Blur()
			case "esc":
				g.pickQuery = g.pickBefore
				g.pickSearch.SetValue(g.pickBefore)
				g.pickList = g.pickBeforeList
				g.refreshPickItems()
				g.pickTyping = false
				g.pickSearch.Blur()
			default:
				g.pickSearch, _ = g.pickSearch.Update(msg)
				g.pickQuery = g.pickSearch.Value()
				g.refreshPickItems()
			}
			return g, nil
		}
		if consumed, cmd := g.pickList.Update(msg); consumed {
			return g, cmd
		}
		switch key {
		case " ":
			g.togglePick()
			return g, nil
		case "enter", "ctrl+s":
			g.confirmPick()
			return g, nil
		case "esc", "q":
			g.mode = groupsDetail
			g.reloadDetail()
		case "/":
			g.pickBefore = g.pickQuery
			g.pickBeforeList = g.pickList
			g.pickTyping = true
			g.pickSearch.Focus()
		case "c":
			g.pickQuery = ""
			g.pickSearch.SetValue("")
			g.refreshPickItems()
		}
		return g, nil

	case groupsDetail:
		if consumed, cmd := g.list.Update(msg); consumed {
			return g, cmd
		}
		switch key {
		case "esc":
			g.mode = groupsList
			g.list = g.groupList
			g.reload()
		case "m":
			g.openPick()
		case "t":
			if !g.busy {
				return g, g.testGroupMembers()
			}
		case " ":
			if c, ok := g.curMember(); ok {
				if g.cur.Type != "select" {
					g.err = fmt.Errorf("仅 select 组可手动选择当前节点")
					return g, nil
				}
				if err := g.app.Proxy.SetGroupSelected(g.cur.ID, c.key); err != nil {
					g.err = err
					return g, nil
				}
				g.err = nil
				g.status = "已保存选择 " + c.label + "；Ctrl+A 应用后生效"
				g.app.MarkConfigDirty()
				g.reload()
				g.reloadDetail()
			}
		case "r":
			g.reload()
			return g, g.fetchRuntime()
		case "enter", "alt+enter":
			g.openDetails("成员详情", g.memberDetails())
		}
		return g, nil

	default: // groupsList
		if consumed, cmd := g.list.Update(msg); consumed {
			return g, cmd
		}
		switch key {
		case "a":
			g.openForm("add-group")
		case "e":
			if grp, ok := g.selectedGroup(); ok {
				g.openFormEdit(grp)
			}
		case "d":
			if grp, ok := g.selectedGroup(); ok {
				g.confirm = components.NewConfirm("delete-group",
					fmt.Sprintf("删除代理组 %q？", grp.Name))
				g.confirmID = grp.ID
				g.mode = groupsConfirm
			}
		case "enter", "m":
			if grp, ok := g.selectedGroup(); ok {
				g.groupList = g.list
				g.list = components.SimpleList{}
				g.cur = grp
				g.mode = groupsDetail
				g.reloadDetail()
			}
		case "alt+enter":
			if grp, ok := g.selectedGroup(); ok {
				g.openDetails("代理组详情", g.groupDetails(grp))
			}
		case "r":
			g.reload()
			return g, g.fetchRuntime()
		}
		return g, nil
	}
}

// --- 数据加载 ---

func (g *Groups) reload() {
	groups, err := g.app.DB.ListProxyGroups()
	g.groups = groups
	if err != nil {
		g.err = err
	}
	nodes, err := g.app.DB.ListNodes(0)
	if err == nil {
		g.nodes = nodes
	}
	subs, err := g.app.DB.ListSubscriptions()
	if err == nil {
		g.subs = subs
	}
	if g.mode == groupsDetail {
		g.reloadDetail()
		return
	}
	if g.mode == groupsPick {
		return
	}
	selectedKey := g.list.SelectedKey()
	g.list.Items = nil
	g.list.Keys = nil
	for _, grp := range groups {
		selected := ""
		if grp.Type == "select" {
			selected = "当前:" + g.memberLabel(grp.Selected)
		}
		g.list.Items = append(g.list.Items,
			fmt.Sprintf("%s [%-7s] %3d 成员  %s", components.Pad(grp.Name, max(12, g.width/4)), grp.Type, len(grp.Members), selected))
		g.list.Keys = append(g.list.Keys, strconv.FormatInt(grp.ID, 10))
	}
	g.list.SelectKey(selectedKey)
}

// reloadDetail 重建组详情视图的成员列表。
func (g *Groups) reloadDetail() {
	if g.cur == nil {
		return
	}
	// 重新拉取，保证成员与选中项最新
	if grp, err := g.app.DB.GetProxyGroup(g.cur.ID); err == nil {
		g.cur = grp
	}
	selected := g.list.SelectedKey()
	members, err := g.app.Proxy.EffectiveMembers(g.cur.ID)
	if err != nil {
		g.err = err
		return
	}
	g.members = members
	g.list.Items = nil
	g.list.Keys = nil
	for _, mem := range members {
		label := g.memberLabel(mem.MemberKey())
		marker := ""
		if g.cur.Type == "select" && mem.MemberKey() == g.cur.Selected {
			marker = "[已保存] "
		}
		g.list.Items = append(g.list.Items, marker+label)
		g.list.Keys = append(g.list.Keys, mem.MemberKey())
	}
	g.list.SelectKey(selected)
}

// memberLabel 把成员键解析为展示文本。
func (g *Groups) memberLabel(key string) string {
	switch {
	case key == "all":
		return "全部节点（动态）"
	case key == "":
		return "（未选择）"
	case strings.HasPrefix(key, "group:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(key, "group:"), 10, 64)
		for _, grp := range g.groups {
			if grp.ID == id {
				return "[组] " + grp.Name
			}
		}
		return fmt.Sprintf("（组 %d 已失效）", id)
	case strings.HasPrefix(key, "node:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(key, "node:"), 10, 64)
		for _, n := range g.nodes {
			if n.ID == id {
				return fmt.Sprintf("%s (%s:%d)", n.Name, n.Protocol, n.Port)
			}
		}
		return fmt.Sprintf("（节点 %d 已失效）", id)
	}
	return key
}

// --- 成员勾选 ---

// openPick 进入成员勾选视图，构建候选项与当前勾选状态。
func (g *Groups) openPick() {
	if g.cur == nil {
		return
	}
	g.pickSet = map[string]bool{}
	g.pickList = components.SimpleList{}
	g.pickSearch = textinput.New()
	g.pickQuery = ""
	g.pickTyping = false
	g.pickOrder = nil
	for _, mem := range g.cur.Members {
		k := mem.MemberKey()
		if !g.pickSet[k] {
			g.pickSet[k] = true
			g.pickOrder = append(g.pickOrder, k)
		}
	}
	g.buildCandidates()
	g.refreshPickItems()
	g.mode = groupsPick
}

// buildCandidates 构建候选项：全部节点 + 其他代理组 + 全部节点池。
func (g *Groups) buildCandidates() {
	g.cands = []pickCandidate{newPickCandidate("all", "全部节点（动态包含所有启用节点）")}
	for _, grp := range g.groups {
		if g.cur != nil && grp.ID == g.cur.ID {
			continue
		}
		g.cands = append(g.cands, newPickCandidate(
			"group:"+strconv.FormatInt(grp.ID, 10),
			"[组] "+grp.Name+" ("+grp.Type+")",
		))
	}
	for _, n := range g.nodes {
		src := "手动"
		for _, s := range g.subs {
			if s.ID == n.SubscriptionID {
				src = s.Name
				break
			}
		}
		g.cands = append(g.cands, newPickCandidate(
			"node:"+strconv.FormatInt(n.ID, 10),
			fmt.Sprintf("%s (%s:%d) [%s]", n.Name, n.Protocol, n.Port, src),
		))
	}
}

// refreshPickItems 按勾选状态重建勾选列表项（已勾选的排前面，保持顺序）。
//
// 匹配读候选上的 search（建候选列表时算好的 label 小写副本，C10），query 的 ToLower
// 在循环外做一次；循环内不得再出现 ToLower 或字符串拼接——TestMemberPickerSearch-
// ReadsCachedSearchKey 用哨兵钉住了这一点。
func (g *Groups) refreshPickItems() {
	query := strings.ToLower(g.pickQuery)
	var items, keys []string
	for _, c := range g.cands {
		if !strings.Contains(c.search, query) {
			continue
		}
		mark := "[ ] "
		if g.pickSet[c.key] {
			mark = "[x] "
		}
		items = append(items, mark+c.label)
		keys = append(keys, c.key)
	}
	g.pickList.SetItems(items, keys)
}

// pickCursorKey 当前光标位置对应的候选键（列表前段是已勾选项）。
func (g *Groups) pickCursorKey() (string, bool) {
	key := g.pickList.SelectedKey()
	return key, key != ""
}

func (g *Groups) togglePick() {
	key, ok := g.pickCursorKey()
	if !ok {
		return
	}
	if g.pickSet[key] {
		delete(g.pickSet, key)
		for i, k := range g.pickOrder {
			if k == key {
				g.pickOrder = append(g.pickOrder[:i], g.pickOrder[i+1:]...)
				break
			}
		}
	} else {
		g.pickSet[key] = true
		g.pickOrder = append(g.pickOrder, key)
	}
	g.refreshPickItems()
}

func (g *Groups) confirmPick() {
	if g.cur == nil {
		g.mode = groupsList
		return
	}
	members := make([]config.ProxyGroupMember, 0, len(g.pickOrder))
	for _, k := range g.pickOrder {
		members = append(members, memberFromKey(k))
	}
	if err := g.app.Proxy.SetGroupMembers(g.cur.ID, members); err != nil {
		g.err = err
		g.mode = groupsDetail
		g.reloadDetail()
		return
	}
	g.err = nil
	g.status = fmt.Sprintf("代理组 %q 成员已保存（%d 项）；Ctrl+A 应用", g.cur.Name, len(members))
	g.app.MarkConfigDirty()
	g.mode = groupsDetail
	g.reload() // 先刷新组缓存（memberLabel 依赖），再重建详情条目
	g.reloadDetail()
}

func memberFromKey(key string) config.ProxyGroupMember {
	switch {
	case key == "all":
		return config.ProxyGroupMember{Type: "all"}
	case strings.HasPrefix(key, "group:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(key, "group:"), 10, 64)
		return config.ProxyGroupMember{Type: "group", ID: id}
	case strings.HasPrefix(key, "node:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(key, "node:"), 10, 64)
		return config.ProxyGroupMember{Type: "node", ID: id}
	}
	return config.ProxyGroupMember{Type: key}
}

// --- 表单 ---

func (g *Groups) openForm(kind string) {
	g.formKind = kind
	g.form = components.NewForm("新建代理组",
		[]string{"名称", "类型", "测速URL", "间隔秒"},
		[]string{"name", "type", "url", "interval"},
		[]string{"如 AI / Streaming", "select 或 urltest", "urltest 用，留空用默认", "urltest 用，如 300"})
	g.form.Reset()
	g.form.SetOptions("type", "select", "urltest")
	configureGroupForm(&g.form)
	g.err = nil
	g.mode = groupsForm
}

func (g *Groups) openFormEdit(grp *config.ProxyGroup) {
	g.formKind = "edit-group"
	g.editID = grp.ID
	g.form = components.NewForm("编辑代理组",
		[]string{"名称", "类型", "测速URL", "间隔秒"},
		[]string{"name", "type", "url", "interval"},
		[]string{"", "", "", ""})
	g.form.SetValueByKey("name", grp.Name)
	g.form.SetValueByKey("type", grp.Type)
	g.form.SetOptions("type", "select", "urltest")
	g.form.SetValueByKey("url", grp.TestURL)
	if grp.IntervalS > 0 {
		g.form.SetValueByKey("interval", strconv.Itoa(grp.IntervalS))
	}
	configureGroupForm(&g.form)
	g.err = nil
	g.mode = groupsForm
}

func (g *Groups) submitForm() {
	name := strings.TrimSpace(g.form.ValueByKey("name"))
	typ := strings.ToLower(strings.TrimSpace(g.form.ValueByKey("type")))
	testURL := strings.TrimSpace(g.form.ValueByKey("url"))
	interval, _ := strconv.Atoi(strings.TrimSpace(g.form.ValueByKey("interval")))
	if name == "" || typ == "" {
		g.err = fmt.Errorf("名称与类型为必填项")
		return
	}
	if g.formKind == "add-group" {
		grp, err := g.app.Proxy.CreateGroup(name, typ, testURL, interval, nil)
		if err != nil {
			g.err = err
			return
		}
		g.err = nil
		g.status = "代理组已创建，请选择成员"
		g.app.MarkConfigDirty()
		g.reload()
		g.cur = grp
		g.list.SelectKey(strconv.FormatInt(grp.ID, 10))
		g.groupList = g.list
		g.list = components.SimpleList{}
		g.openPick() // 新建后直接进入成员勾选
		return
	}
	grp, err := g.app.DB.GetProxyGroup(g.editID)
	if err != nil {
		g.err = err
		g.mode = groupsList
		return
	}
	grp.Name, grp.Type, grp.TestURL, grp.IntervalS = name, typ, testURL, interval
	if err := g.app.Proxy.UpdateGroup(grp, grp.Members); err != nil {
		g.err = err
		return
	}
	g.err = nil
	g.status = "代理组已保存"
	g.app.MarkConfigDirty()
	g.mode = groupsList
	g.reload()
}

// --- 测速 ---

// testGroupMembers 测速组成员节点（all 展开为全部启用节点，嵌套组递归展开）。
func (g *Groups) testGroupMembers() tea.Cmd {
	if g.cur == nil {
		return nil
	}
	ids, err := g.memberNodeIDs()
	if err != nil {
		g.err = err
		return nil
	}
	if len(ids) == 0 {
		g.err = fmt.Errorf("组内没有可测速的节点")
		return nil
	}
	return g.testMembers(ids)
}

func (g *Groups) testMembers(ids []int64) tea.Cmd {
	if g.busy || len(ids) == 0 {
		return nil
	}
	g.busy = true
	g.status = fmt.Sprintf("正在测速 %d 个节点…", len(ids))
	g.err = nil
	g.retryTask = func() tea.Cmd { return g.testMembers(failedIDs(g.taskResults)) }
	return g.taskN("代理组测速", len(ids), func(report func(itemResult)) tea.Msg {
		return latencyResults(g.app, ids, report)
	})
}

// memberNodeIDs 收集组成员节点 ID（含 all 展开与嵌套组递归，带环保护）。
func (g *Groups) memberNodeIDs() ([]int64, error) {
	groups, err := g.app.DB.ListProxyGroups()
	if err != nil {
		return nil, err
	}
	nodes, err := g.app.DB.ListNodes(0)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*config.ProxyGroup{}
	for _, grp := range groups {
		byID[grp.ID] = grp
	}
	enabled := []*config.Node{}
	for _, n := range nodes {
		if n.Enabled {
			enabled = append(enabled, n)
		}
	}
	seen := map[int64]bool{}
	visitedGroups := map[int64]bool{}
	var walk func(grp *config.ProxyGroup) error
	walk = func(grp *config.ProxyGroup) error {
		if visitedGroups[grp.ID] {
			return nil
		}
		visitedGroups[grp.ID] = true
		for _, mem := range grp.Members {
			switch mem.Type {
			case "all":
				for _, n := range enabled {
					seen[n.ID] = true
				}
			case "node":
				for _, n := range enabled {
					if n.ID == mem.ID {
						seen[n.ID] = true
						break
					}
				}
			case "group":
				if sub, ok := byID[mem.ID]; ok {
					if err := walk(sub); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := walk(g.cur); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	return ids, nil
}

// --- 消息处理 ---

func (g *Groups) onActionDone(msg actionDoneMsg) (Page, tea.Cmd) {
	if !g.accept(msg) {
		return g, nil
	}
	g.busy = false
	if msg.Err != nil {
		g.err = msg.Err
		g.status = ""
	} else {
		g.err = nil
		if msg.Action == "test-latency" {
			g.status = "代理组测速：" + resultSummary(msg.Results) + " · v 详情"
		}
	}
	if g.mode == groupsDetail {
		g.reloadDetail()
	} else {
		g.reload()
	}
	return g, nil
}

func (g *Groups) onConfirm(msg components.ConfirmMsg) (Page, tea.Cmd) {
	if g.mode != groupsConfirm {
		return g, nil
	}
	g.mode = groupsList
	if msg.Confirmed && msg.ID == "delete-group" {
		if err := g.app.Proxy.DeleteGroup(g.confirmID); err != nil {
			g.err = err
		} else {
			g.err = nil
			g.status = "代理组已删除"
			g.app.MarkConfigDirty()
		}
		g.reload()
	}
	return g, nil
}

// --- 渲染 ---

func (g *Groups) View() string {
	g.table()
	g.preview = ""
	if g.mode == groupsDetail {
		g.preview = g.memberDetails()
	}
	if g.mode == groupsList {
		if grp, ok := g.selectedGroup(); ok {
			g.preview = g.groupDetails(grp)
		}
	}

	if g.detailActive {
		return g.detailsView()
	}
	switch g.mode {
	case groupsForm:
		return g.formView(&g.form, g.err)
	case groupsConfirm:
		return g.confirmView(&g.confirm)
	case groupsPick:
		head := fmt.Sprintf("选择成员 · %s · 已选 %d 项", g.cur.Name, len(g.pickOrder))
		if g.pickTyping {
			g.pickSearch.Width = max(5, g.width-10)
			head = "搜索: " + g.pickSearch.View()
		}
		return g.listView(&g.pickList, head, "没有匹配的成员；c 清除搜索", g.statusLine(), "Space 勾选 · / 搜索 · c 清除 · Ctrl+S 保存 · Esc 放弃")
	case groupsDetail:
		head := fmt.Sprintf("代理组 / %s [%s] · %d 个可用成员", g.cur.Name, g.cur.Type, len(g.members))
		runtime := "未运行"
		if g.app.Core.IsRunning() {
			runtime = "读取中，请 r 刷新"
			if g.runtimeAt.Equal(g.app.Core.Status().StartedAt) {
				if info, ok := g.runtime[g.cur.Name]; ok {
					runtime = info.Now
				}
			}
		}
		feedback := "保存选择: " + g.memberLabel(g.cur.Selected) + " · 运行实际: " + runtime + g.statusLine()
		return g.listView(&g.list, head, "暂无可用成员；m 添加成员或启用订阅/节点。", feedback,
			Hints(g))
	default:
		return g.listView(&g.list, "代理组", "暂无代理组，a 新建。", g.statusLine(),
			Hints(g))
	}
}

func (g *Groups) statusLine() string {
	return g.feedback(g.status, g.err)
}

func (g *Groups) selectedGroup() (*config.ProxyGroup, bool) {
	if g.list.Cursor < 0 || g.list.Cursor >= len(g.groups) {
		return nil, false
	}
	return g.groups[g.list.Cursor], true
}

func (g *Groups) curMember() (pickCandidate, bool) {
	// 详情视图光标对应 cur.Members 的位置
	if g.list.Cursor < 0 || g.cur == nil || g.list.Cursor >= len(g.members) {
		return pickCandidate{}, false
	}
	key := g.members[g.list.Cursor].MemberKey()
	return newPickCandidate(key, g.memberLabel(key)), true
}

// Runtime feedback is bound to the core instance that supplied it.
type groupRuntimeMsg struct {
	startedAt time.Time
	proxies   map[string]clashapi.ProxyInfo
}

func (g *Groups) fetchRuntime() tea.Cmd {
	startedAt := g.app.Core.Status().StartedAt
	client := g.app.ClashClient()
	if startedAt.IsZero() || client == nil {
		g.runtime = nil
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(g.app.BackgroundContext(), 3*time.Second)
		defer cancel()
		proxies, _ := client.Proxies(ctx)
		return groupRuntimeMsg{startedAt: startedAt, proxies: proxies}
	}
}
