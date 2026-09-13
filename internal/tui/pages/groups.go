package pages

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
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
type pickCandidate struct {
	key   string // "all" / "group:<id>" / "node:<id>"
	label string
}

// Groups 代理组管理页：组 CRUD、成员勾选（跨订阅节点/嵌套组/全部节点）、
// select 组当前选中持久化、组成员测速。
type Groups struct {
	base
	mode   groupsMode
	groups []*config.ProxyGroup
	nodes  []*config.Node
	subs   []*config.Subscription
	cur    *config.ProxyGroup
	list   components.SimpleList

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

func (g *Groups) Title() string { return "Proxy Groups" }

func (g *Groups) Init() tea.Cmd { return nil }

func (g *Groups) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		g.SetSize(msg.Width, msg.Height)
		return g, nil
	case ActivateMsg:
		g.reload()
		return g, nil
	case actionDoneMsg:
		return g.onActionDone(msg)
	case components.ConfirmMsg:
		return g.onConfirm(msg)
	case tea.KeyMsg:
		return g.handleKey(msg)
	}
	return g, nil
}

func (g *Groups) handleKey(msg tea.KeyMsg) (Page, tea.Cmd) {
	key := msg.String()
	switch g.mode {
	case groupsForm:
		switch key {
		case "esc":
			g.mode = groupsList
			return g, nil
		case "enter":
			g.submitForm()
			return g, nil
		}
		g.form.Update(msg)
		return g, nil

	case groupsConfirm:
		if consumed, cmd := g.confirm.Update(msg); consumed {
			return g, cmd
		}
		return g, nil

	case groupsPick:
		if consumed, cmd := g.pickList.Update(msg); consumed {
			return g, cmd
		}
		switch key {
		case " ":
			g.togglePick()
			return g, nil
		case "enter":
			g.confirmPick()
			return g, nil
		case "esc":
			g.mode = groupsDetail
			g.reloadDetail()
		}
		return g, nil

	case groupsDetail:
		if consumed, cmd := g.list.Update(msg); consumed {
			return g, cmd
		}
		switch key {
		case "esc":
			g.mode = groupsList
			g.reload()
		case "m":
			g.openPick()
		case "s":
			if !g.busy {
				return g, g.testGroupMembers()
			}
		case " ", "enter":
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
				g.status = "已选择 " + c.label
				g.reload()
				g.reloadDetail()
			}
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
				g.cur = grp
				g.mode = groupsDetail
				g.reloadDetail()
			}
		case "r":
			g.reload()
		}
		return g, nil
	}
}

// --- 数据加载 ---

func (g *Groups) reload() {
	groups, err := g.app.DB.ListProxyGroups()
	g.groups, g.err = groups, err
	nodes, err := g.app.DB.ListNodes(0)
	if err == nil {
		g.nodes = nodes
	}
	subs, err := g.app.DB.ListSubscriptions()
	if err == nil {
		g.subs = subs
	}
	g.list.Items = nil
	for _, grp := range groups {
		selected := ""
		if grp.Type == "select" {
			selected = "当前:" + g.memberLabel(grp.Selected)
		}
		g.list.Items = append(g.list.Items,
			fmt.Sprintf("%-16s [%-7s] %3d 成员  %s", grp.Name, grp.Type, len(grp.Members), selected))
	}
	g.clampListCursor(len(groups))
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
	g.list.Items = nil
	for _, mem := range g.cur.Members {
		label := g.memberLabel(mem.MemberKey())
		marker := ""
		if g.cur.Type == "select" && mem.MemberKey() == g.cur.Selected {
			marker = "● 当前 "
		}
		g.list.Items = append(g.list.Items, marker+label)
	}
	g.clampListCursor(len(g.cur.Members))
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
	g.cands = []pickCandidate{{key: "all", label: "全部节点（动态包含所有启用节点）"}}
	for _, grp := range g.groups {
		if g.cur != nil && grp.ID == g.cur.ID {
			continue
		}
		g.cands = append(g.cands, pickCandidate{
			key:   "group:" + strconv.FormatInt(grp.ID, 10),
			label: "[组] " + grp.Name + " (" + grp.Type + ")",
		})
	}
	for _, n := range g.nodes {
		src := "手动"
		for _, s := range g.subs {
			if s.ID == n.SubscriptionID {
				src = s.Name
				break
			}
		}
		g.cands = append(g.cands, pickCandidate{
			key:   "node:" + strconv.FormatInt(n.ID, 10),
			label: fmt.Sprintf("%s (%s:%d) [%s]", n.Name, n.Protocol, n.Port, src),
		})
	}
}

// refreshPickItems 按勾选状态重建勾选列表项（已勾选的排前面，保持顺序）。
func (g *Groups) refreshPickItems() {
	g.pickList.Items = nil
	// 已勾选的按 pickOrder 顺序
	shown := map[string]bool{}
	for _, k := range g.pickOrder {
		if c, ok := g.candByKey(k); ok {
			g.pickList.Items = append(g.pickList.Items, "[x] "+c.label)
			shown[k] = true
		}
	}
	for _, c := range g.cands {
		if !shown[c.key] {
			g.pickList.Items = append(g.pickList.Items, "[ ] "+c.label)
		}
	}
	g.clampPickCursor(len(g.pickList.Items))
}

func (g *Groups) candByKey(key string) (pickCandidate, bool) {
	for _, c := range g.cands {
		if c.key == key {
			return c, true
		}
	}
	return pickCandidate{}, false
}

// pickCursorKey 当前光标位置对应的候选键（列表前段是已勾选项）。
func (g *Groups) pickCursorKey() (string, bool) {
	i := g.pickList.Cursor
	if i < 0 || i >= len(g.pickList.Items) {
		return "", false
	}
	// 前段为已勾选项（顺序同 pickOrder），其后为未勾选候选
	if i < len(g.pickOrder) {
		return g.pickOrder[i], true
	}
	idx := i - len(g.pickOrder)
	unchecked := make([]string, 0, len(g.cands))
	shown := map[string]bool{}
	for _, k := range g.pickOrder {
		shown[k] = true
	}
	for _, c := range g.cands {
		if !shown[c.key] {
			unchecked = append(unchecked, c.key)
		}
	}
	if idx >= len(unchecked) {
		return "", false
	}
	return unchecked[idx], true
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
	g.status = fmt.Sprintf("代理组 %q 成员已保存（%d 项）", g.cur.Name, len(members))
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
	g.form.SetValueByKey("url", grp.TestURL)
	if grp.IntervalS > 0 {
		g.form.SetValueByKey("interval", strconv.Itoa(grp.IntervalS))
	}
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
		g.reload()
		g.cur = grp
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
	g.busy = true
	g.status = fmt.Sprintf("正在测速 %d 个节点…", len(ids))
	g.err = nil
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		_, err := g.app.Proxy.TestLatency(ctx, ids, "", 3000)
		return actionDoneMsg{Action: "test-latency", Err: err}
	}
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
	g.busy = false
	if msg.Err != nil {
		g.err = msg.Err
		g.status = ""
	} else {
		g.err = nil
		if msg.Action == "test-latency" {
			g.status = "测速完成"
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
		}
		g.reload()
	}
	return g, nil
}

// --- 渲染 ---

func (g *Groups) View() string {
	switch g.mode {
	case groupsForm:
		return g.form.View()
	case groupsConfirm:
		return g.confirm.View()
	case groupsPick:
		head := styles.Title.Render("选择成员") +
			styles.Dim.Render(fmt.Sprintf(" — %s（space 勾选/取消 · enter 完成 · esc 放弃）", g.cur.Name))
		return head + "\n" + g.pickList.View("没有可选择的成员。") + g.statusLine()
	case groupsDetail:
		head := styles.Title.Render("代理组") +
			styles.Dim.Render(fmt.Sprintf(" — %s [%s]（%d 成员）", g.cur.Name, g.cur.Type, len(g.cur.Members)))
		body := g.list.View("组内暂无成员，按 m 勾选。")
		hint := "\nm 勾选成员 · s 测速组成员"
		if g.cur.Type == "select" {
			hint += " · space/enter 设为当前"
		}
		return head + "\n" + body + g.statusLine() + styles.Dim.Render(hint+" · esc 返回")
	default:
		head := styles.Title.Render("代理组") + "\n"
		body := g.list.View("暂无代理组。")
		return head + body + g.statusLine() +
			styles.Dim.Render("\na 新建 · e 编辑 · enter/m 成员与选择 · d 删除 · r 刷新")
	}
}

func (g *Groups) statusLine() string {
	var b string
	if g.busy {
		b += "\n" + styles.Accent.Render(g.status)
	} else if g.status != "" {
		b += "\n" + styles.Ok.Render(g.status)
	}
	if g.err != nil {
		b += "\n" + styles.Err.Render("错误: "+g.err.Error())
	}
	return b
}

func (g *Groups) selectedGroup() (*config.ProxyGroup, bool) {
	if g.list.Cursor < 0 || g.list.Cursor >= len(g.groups) {
		return nil, false
	}
	return g.groups[g.list.Cursor], true
}

func (g *Groups) curMember() (pickCandidate, bool) {
	// 详情视图光标对应 cur.Members 的位置
	if g.list.Cursor < 0 || g.cur == nil || g.list.Cursor >= len(g.cur.Members) {
		return pickCandidate{}, false
	}
	key := g.cur.Members[g.list.Cursor].MemberKey()
	return pickCandidate{key: key, label: g.memberLabel(key)}, true
}

func (g *Groups) clampListCursor(n int) {
	if g.list.Cursor >= n {
		g.list.Cursor = n - 1
	}
	if g.list.Cursor < 0 {
		g.list.Cursor = 0
	}
}

func (g *Groups) clampPickCursor(n int) {
	if g.pickList.Cursor >= n {
		g.pickList.Cursor = n - 1
	}
	if g.pickList.Cursor < 0 {
		g.pickList.Cursor = 0
	}
}
