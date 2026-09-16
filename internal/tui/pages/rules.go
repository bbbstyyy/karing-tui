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
	"github.com/bbbstyyy/karing-tui/internal/catalog"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/routing"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
	"github.com/bbbstyyy/karing-tui/internal/validation"
)

// rulesMode Rules 页的视图模式。
type rulesMode int

const (
	rulesGroups  rulesMode = iota // 分流组列表（按层分组渲染）
	rulesGroupRl                  // 分流组规则列表
	rulesForm                     // 表单（分流组/规则/规则集）
	rulesSets                     // 规则集管理
	rulesConfirm                  // 删除确认
	rulesCatalog                  // 内置分类库浏览/挑选
	rulesMove                     // 跨层移动：选择目标层
)

// Rules 分流管理页：分流组列表（按层分组）、规则编辑（排序/启停）、规则集管理与下载、
// 内置分类库浏览与挑选。

// presetGroupNames 是地区预置会创建的分组名，用于在列表里标注来源（7.6）：
// 用户删除预置组后能一眼看出「这组可以按 P 恢复」。嵌入的 JSON 只解析一次。
var presetGroupNames = func() map[string]bool {
	names := map[string]bool{}
	for _, region := range routing.PresetRegions {
		entries, err := routing.LoadPreset(region)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.Group != nil {
				names[entry.Group.Name] = true
			}
		}
	}
	return names
}()

type Rules struct {
	base
	mode            rulesMode
	groups          []*config.RoutingGroup
	cur             *config.RoutingGroup
	sets            []*config.RuleSet
	list            components.SimpleList
	groupList       components.SimpleList
	setList         components.SimpleList
	catalogBackList components.SimpleList
	catBefore       string
	catBeforeList   components.SimpleList

	// 按层渲染（6.5）：groupRows 把列表行映射到 r.groups 的下标，-1 表示层标题行。
	groupRows   []int
	enabledOnly bool // 仅显示启用（7.6）：27 组方案里停用组会淹没 custom 层

	// 跨层移动：moveList 是目标层选择器，moveKinds 与行一一对应。
	moveList   components.SimpleList
	moveBack   components.SimpleList
	moveKinds  []string
	moveTarget *config.RoutingGroup
	moveKind   string // 确认后要移入的层

	// 内置分类库浏览态
	catKind   string          // 当前种类（geosite/geoip/acl）
	catQuery  string          // 搜索词
	catSearch textinput.Model // 搜索输入框
	catTyping bool            // 搜索输入中
	catHits   []catalog.Ref   // 当前筛选结果
	catBack   rulesMode       // 退出分类库后回到的模式

	// catRefsInUse 规则集管理页里列出的「引用中的内置分类」（只读行，排在自定义规则集之后）
	catRefsInUse []catalog.Ref

	form     components.Form
	logical  *logicalEditor
	formKind string // add-rg / edit-rg / add-rule / edit-rule / add-rs
	editID   int64  // edit-rg / edit-rs 目标 ID
	editRule int    // edit-rule 的规则下标

	confirm     components.Confirm
	confirmKind string
	confirmID   int64

	err    error
	status string
	busy   bool
}

// NewRules 创建 Rules 页。
func NewRules(app *application.App) *Rules {
	return &Rules{base: base{app: app}}
}

func (r *Rules) Title() string { return "分流规则" }

// Editing 表单输入与分类库搜索输入状态时拦截全局键位。
func (r *Rules) Editing() bool {
	return r.detailActive || r.mode == rulesForm || r.mode == rulesConfirm || (r.mode == rulesCatalog && r.catTyping)
}

func (r *Rules) Init() tea.Cmd { return nil }

func (r *Rules) Update(msg tea.Msg) (Page, tea.Cmd) {
	if !r.Editing() {
		if handled, cmd := r.handleRetry(msg); handled {
			return r, cmd
		}
	}
	if r.detailActive || !r.Editing() {
		if r.handleDetails(msg, r.err) {
			return r, nil
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		r.SetSize(msg.Width, msg.Height)
		return r, nil
	case ActivateMsg:
		r.reload()
		return r, nil
	case actionDoneMsg:
		if !r.accept(msg) {
			return r, nil
		}
		r.busy = false
		if msg.Err != nil {
			r.err = msg.Err
			r.status = ""
		} else {
			r.err = nil
			switch msg.Action {
			case "download-rulesets":
				r.status = "规则集下载：" + resultSummary(msg.Results) + " · v 详情"
			case "download-catalog":
				r.status = msg.Data + " 已缓存"
			case "apply-preset":
				r.status = msg.Data + " · Ctrl+A 应用最新配置"
			}
		}
		if r.mode == rulesCatalog {
			r.reloadCatalog()
		} else {
			r.reload()
		}
		return r, nil
	case components.ConfirmMsg:
		return r.onConfirm(msg)
	case tea.KeyMsg:
		return r.handleKey(msg)
	}
	if r.mode == rulesForm {
		return r, r.form.Update(msg)
	}
	return r, nil
}

func (r *Rules) handleKey(msg tea.KeyMsg) (Page, tea.Cmd) {
	key := commandKey(r, msg)
	if !r.Editing() && key == "r" {
		r.reload()
		return r, nil
	}
	if (key == "[" || key == "]") && (r.mode == rulesGroups || r.mode == rulesSets) {
		if r.mode == rulesGroups {
			r.groupList = r.list
			r.list = r.setList
			r.mode = rulesSets
		} else {
			r.setList = r.list
			r.list = r.groupList
			r.mode = rulesGroups
		}
		r.reload()
		return r, nil
	}
	switch r.mode {
	case rulesForm:
		if r.logical != nil {
			action, cmd := r.logical.update(msg)
			if action == "save" {
				r.form.SetValueByKey("value", r.logical.value())
				r.logical = nil
				r.err = nil
			}
			if action == "cancel" {
				r.logical = nil
			}
			return r, cmd
		}
		action, cmd := r.form.Handle(msg)
		configureRuleValue(r.app, &r.form)
		switch action {
		case "conditions":
			r.logical = newLogicalEditor(r.app, r.form.ValueByKey("value"))
		case "cancel":
			r.mode = r.formBackMode()
		case "save":
			r.submitForm()
		}
		return r, cmd

	case rulesConfirm:
		if consumed, cmd := r.confirm.Update(msg); consumed {
			return r, cmd
		}
		return r, nil

	case rulesMove:
		if consumed, cmd := r.moveList.Update(msg); consumed {
			return r, cmd
		}
		switch key {
		case "esc":
			r.closeMoveLayer()
		case "enter":
			r.confirmMoveLayer()
		}
		return r, nil

	case rulesCatalog:
		return r.handleCatalogKey(msg)

	case rulesSets:
		if consumed, cmd := r.list.Update(msg); consumed {
			return r, cmd
		}
		switch key {
		case "enter", "alt+enter":
			if rs, ok := r.selectedSet(); ok {
				r.openDetails("规则集详情", fmt.Sprintf("名称: %s\nTag: %s\n格式: %s\nURL: %s\n启用: %v", rs.Name, rs.Tag, rs.Format, rs.URL, rs.Enabled))
			} else if ref, ok := r.selectedSetCatalog(); ok {
				r.openDetails("内置分类详情", fmt.Sprintf("分类: %s\nTag: %s", ref.String(), ref.Tag()))
			}
		case "esc", "[":
			r.mode = rulesGroups
			r.list = r.groupList
			r.reload()
		case "a":
			r.openForm("add-rs")
		case "c":
			r.openCatalog(rulesSets)
		case "d":
			if rs, ok := r.selectedSet(); ok {
				r.confirm = components.NewConfirm("delete-rs", fmt.Sprintf("删除规则集 %q？", rs.Name))
				r.confirmKind = "delete-rs"
				r.confirmID = rs.ID
				r.mode = rulesConfirm
			}
		case "u":
			if rs, ok := r.selectedSet(); ok && !r.busy {
				return r, r.downloadRuleSets([]int64{rs.ID}, "正在下载规则集…")
			}
			if ref, ok := r.selectedSetCatalog(); ok && !r.busy {
				return r, r.downloadCatalog(ref)
			}
		case "U":
			if !r.busy {
				ids := make([]int64, 0, len(r.sets))
				for _, rs := range r.sets {
					ids = append(ids, rs.ID)
				}
				return r, r.downloadAll(ids)
			}
		case " ":
			if rs, ok := r.selectedSet(); ok {
				if err := r.app.Rules.SetEnabled(rs.ID, !rs.Enabled); err != nil {
					r.err = err
				} else {
					r.app.MarkConfigDirty()
				}
				r.reload()
			}
		}
		return r, nil

	case rulesGroupRl:
		if key == "enter" || key == "alt+enter" {
			if idx, ok := r.selectedRuleIdx(); ok {
				r.openDetails("规则详情", ruleDetails(r.cur.Rules[idx]))
			}
			return r, nil
		}
		if consumed, cmd := r.list.Update(msg); consumed {
			return r, cmd
		}
		switch key {
		case "esc":
			r.mode = rulesGroups
			r.list = r.groupList
			r.reload()
		case "a":
			r.openForm("add-rule")
		case "c":
			r.openCatalog(rulesGroupRl)
		case "e":
			if idx, ok := r.selectedRuleIdx(); ok {
				r.openFormEditRule(idx)
			}
		case "D", "d":
			if idx, ok := r.selectedRuleIdx(); ok {
				rule := r.cur.Rules[idx]
				r.confirm = components.NewConfirm("delete-rule", fmt.Sprintf("从分流组 %q 删除规则 %s = %s？", r.cur.Name, rule.Type, rule.Value))
				r.confirmID, r.confirmKind = rule.ID, "delete-rule"
				r.mode = rulesConfirm
			}
		case " ":
			if idx, ok := r.selectedRuleIdx(); ok {
				r.cur.Rules[idx].Enabled = !r.cur.Rules[idx].Enabled
				if err := r.app.Rout.UpdateGroup(r.cur, r.cur.Rules); err != nil {
					r.err = err
				} else {
					r.err = nil
					r.app.MarkConfigDirty()
				}
				r.reloadGroupRules()
			}
		case "J", "K":
			if idx, ok := r.selectedRuleIdx(); ok {
				delta := -1
				if key == "J" {
					delta = 1
				}
				if err := r.app.Rout.MoveRule(r.cur.ID, r.cur.Rules[idx].ID, delta); err != nil {
					r.err = err
				} else {
					r.err = nil
					r.app.MarkConfigDirty()
					r.status = "规则顺序已保存，光标保持在同一规则"
				}
				r.reloadGroupRules()
			}
		}
		return r, nil

	default: // rulesGroups
		if consumed, cmd := r.list.Update(msg); consumed {
			return r, cmd
		}
		switch key {
		case "alt+enter":
			if group, ok := r.selectedGroup(); ok {
				r.openDetails("分流组详情", fmt.Sprintf("名称: %s\n层: %s\n目标: %s\n规则: %d 条\n启用: %v\n层内序号: %d\n\n优先级 = (层序, 层内序号, ID)；层序: %s",
					group.Name, config.KindLabel(group.Layer()), group.Target, len(group.Rules), group.Enabled, group.Position+1,
					strings.Join(config.Kinds, " < ")))
			}
		case "a":
			r.openForm("add-rg")
		case "e":
			if g, ok := r.selectedGroup(); ok {
				r.openFormEditGroup(g)
			}
		case "d":
			if g, ok := r.selectedGroup(); ok {
				r.confirm = components.NewConfirm("delete-rg", fmt.Sprintf("删除分流组 %q 及其规则？", g.Name))
				r.confirmKind = "delete-rg"
				r.confirmID = g.ID
				r.mode = rulesConfirm
			}
		case "enter":
			if g, ok := r.selectedGroup(); ok {
				r.groupList = r.list
				r.list = components.SimpleList{}
				r.cur = g
				r.mode = rulesGroupRl
				r.reloadGroupRules()
			}
		case " ":
			if g, ok := r.selectedGroup(); ok {
				if err := r.app.Rout.SetEnabled(g.ID, !g.Enabled); err != nil {
					r.err = err
				} else {
					r.app.MarkConfigDirty()
					if g.Layer() == config.KindFinal && g.Enabled {
						r.status = "已停用 final 兜底组：未匹配流量将直连而非走代理"
					}
				}
				r.reload()
			}
		case "J", "K":
			// 层内上移/下移（与规则列表同款交换语义，边界处不动作）
			if g, ok := r.selectedGroup(); ok {
				delta := -1
				if key == "J" {
					delta = 1
				}
				if err := r.app.Rout.MoveGroup(g.ID, delta); err != nil {
					r.err = err
				} else {
					r.err = nil
					r.app.MarkConfigDirty()
					r.status = fmt.Sprintf("%s 层内顺序已保存（层内顺序即优先级）", config.KindLabel(g.Layer()))
				}
				r.reload()
			}
		case "m":
			if g, ok := r.selectedGroup(); ok {
				r.openMoveLayer(g)
			}
		case "P":
			// 导入 / 恢复地区预置（7.4 / 7.6）：merge 语义，不改写已存在的组
			if r.busy {
				return r, nil
			}
			r.confirm = components.NewConfirm("apply-preset",
				fmt.Sprintf("导入地区预置 %s？\n27 个分组全部归入 自定义分流组（custom 层），层内顺序即优先级；\n同名分组会被跳过，不会改写你已有的修改。", strings.Join(routing.PresetRegions, " / ")))
			r.confirmKind = "apply-preset"
			r.mode = rulesConfirm
		case "f":
			r.enabledOnly = !r.enabledOnly
			if r.enabledOnly {
				r.status = "仅显示启用的分流组（再按 f 显示全部）"
			} else {
				r.status = "显示全部分流组"
			}
			r.reload()
		case "]":
			r.groupList = r.list
			r.list = components.SimpleList{}
			r.mode = rulesSets
			r.reload()
		case "r":
			r.reload()
		}
		return r, nil
	}
}

// --- 跨层移动（6.5）---

// openMoveLayer 打开「移动到其他层」选择器。跨层移动会改变该组的优先级归属，
// 故必须显式操作并在确认弹窗里说明后果，不能靠层内上移/下移"移出去"。
func (r *Rules) openMoveLayer(g *config.RoutingGroup) {
	r.moveBack = r.list
	r.moveTarget = g
	current := g.Layer()
	r.moveKinds = nil
	r.moveList = components.SimpleList{}
	rows := make([][]string, 0, len(config.Kinds))
	for _, kind := range config.Kinds {
		count, enabled := 0, 0
		for _, other := range r.groups {
			if other.Layer() != kind {
				continue
			}
			count++
			if other.Enabled {
				enabled++
			}
		}
		mark := ""
		if kind == current {
			mark = "（当前）"
		}
		note := ""
		switch kind {
		case config.KindFinal:
			note = "兜底 · 不可选「不处理」"
		case config.KindCustom:
			note = "手工规则优先于此层之下所有层"
		}
		rows = append(rows, []string{
			fmt.Sprintf("%s%s", config.KindLabel(kind), mark),
			fmt.Sprintf("%d 组 / 启用 %d", count, enabled),
			note,
		})
		r.moveKinds = append(r.moveKinds, kind)
	}
	keys := make([]string, len(r.moveKinds))
	for i, kind := range r.moveKinds {
		keys[i] = "layer:" + kind
	}
	r.moveList.SetTable([]components.Column{
		col("层", 18, 0, false),
		col("组数", 12, 0, false),
		col("说明", 30, 1, false),
	}, rows, keys)
	r.moveList.SelectKey("layer:" + current)
	r.status = ""
	r.mode = rulesMove
}

// closeMoveLayer 退出层选择器，回到分流组列表。
func (r *Rules) closeMoveLayer() {
	r.list = r.moveBack
	r.moveTarget = nil
	r.moveKinds = nil
	r.mode = rulesGroups
	r.reload()
}

// confirmMoveLayer 对选中的目标层给出明确提示后再执行移动。
func (r *Rules) confirmMoveLayer() {
	if r.moveTarget == nil {
		return
	}
	i := r.moveList.Cursor
	if i < 0 || i >= len(r.moveKinds) {
		return
	}
	target := r.moveKinds[i]
	g := r.moveTarget
	if target == g.Layer() {
		r.status = fmt.Sprintf("%s 已在 %s 层", g.Name, config.KindLabel(target))
		return
	}
	r.moveKind = target
	r.confirm = components.NewConfirm("move-layer",
		fmt.Sprintf("把分流组 %q 移到 %s 层？\n该组的优先级归属会随之改变（层序：%s）。",
			g.Name, config.KindLabel(target), strings.Join(config.Kinds, " < ")))
	r.confirmKind = "move-layer"
	r.confirmID = g.ID
	r.mode = rulesConfirm
}

// --- 数据加载 ---

func (r *Rules) reload() {
	if r.mode == rulesCatalog {
		r.reloadCatalog()
		return
	}
	groups, err := r.app.DB.ListRoutingGroups()
	r.groups = groups
	if err != nil {
		r.err = err
	}
	sets, err := r.app.DB.ListRuleSets()
	if err == nil {
		r.sets = sets
	}
	viewMode := r.mode
	if viewMode == rulesForm {
		viewMode = r.formBackMode()
	}
	if viewMode == rulesConfirm {
		switch r.confirmKind {
		case "delete-rule":
			viewMode = rulesGroupRl
		case "delete-rs":
			viewMode = rulesSets
		default:
			viewMode = rulesGroups
		}
	}
	if viewMode == rulesGroupRl {
		r.reloadGroupRules()
		return
	}
	selected := r.list.SelectedKey()
	if viewMode == rulesSets {
		r.list.Height = 0
		r.list.Items = nil
		r.list.Keys = nil
		for _, rs := range r.sets {
			cached := "未缓存"
			if rs.CachedPath != "" {
				cached = "已缓存"
				if !rs.UpdatedAt.IsZero() {
					cached = "缓存 " + rs.UpdatedAt.Format("01-02 15:04")
				}
			}
			state := "启用"
			if !rs.Enabled {
				state = "停用"
			}
			r.list.Items = append(r.list.Items,
				fmt.Sprintf("%-24s %-28s %-4s %-12s %s", rs.Tag, rs.Name, rs.Format, cached, state))
			r.list.Keys = append(r.list.Keys, fmt.Sprintf("set:%d", rs.ID))
		}
		// 被规则引用的内置分类：不在 rulesets 表里，但同样会进生成的配置——
		// 一并列出（只读，标 [内置]），否则用户在此页看不到自己正在用的规则集。
		if refs, err := r.app.Rules.ReferencedCatalog(); err == nil {
			r.catRefsInUse = refs
			for _, ref := range refs {
				cached := "未缓存"
				if r.app.Rules.CatalogCached(ref) {
					cached = "已缓存"
				}
				r.list.Items = append(r.list.Items,
					fmt.Sprintf("%-24s %-28s %-4s %-12s %s", ref.Tag(), ref.String(), "srs", cached, "[内置]"))
				r.list.Keys = append(r.list.Keys, ref.String())
			}
		} else {
			r.catRefsInUse = nil
		}
		r.list.SelectKey(selected)
		return
	}
	r.list.Height = 0
	r.list.Items = nil
	r.list.Keys = nil
	r.buildGroupList()
	r.list.SelectKey(selected)
}

// buildGroupList 按层重建分流组列表（6.5）：层标题是不可选中行，层内为可选中行。
// 层序固定 custom → geosite → geoip → acl → final（final 恒置底）；空层也显示标题，
// 这样用户能把规则移进去（若视觉过重，采用折叠风格而非隐藏整层）。
func (r *Rules) buildGroupList() {
	rows := make([][]string, 0, len(r.groups))
	items := make([]string, 0, len(r.groups))
	keys := make([]string, 0, len(r.groups))
	groupRows := make([]int, 0, len(r.groups))
	selectable := make([]bool, 0, len(r.groups))

	indexOf := make(map[int64]int, len(r.groups))
	for i, g := range r.groups {
		indexOf[g.ID] = i
	}
	for _, layer := range config.GroupByKind(r.groups) {
		enabled := 0
		for _, g := range layer.Groups {
			if g.Enabled {
				enabled++
			}
		}
		head := fmt.Sprintf("%s · %d 组 / 启用 %d", config.KindLabel(layer.Kind), len(layer.Groups), enabled)
		if layer.Kind == config.KindFinal {
			head += " · 兜底 · 不可选「不处理」"
		}
		rows = append(rows, []string{head})
		items = append(items, styles.Heading.Render(head))
		keys = append(keys, "")
		groupRows = append(groupRows, -1)
		selectable = append(selectable, false)

		pos := 0
		for _, g := range layer.Groups {
			if r.enabledOnly && !g.Enabled {
				continue
			}
			pos++
			source := ""
			if presetGroupNames[g.Name] {
				source = "预置"
			}
			rows = append(rows, []string{
				fmt.Sprintf("%d. %s", pos, g.Name),
				g.Target,
				fmt.Sprintf("%d 条", len(g.Rules)),
				enabledLabel(g.Enabled),
				source,
			})
			items = append(items, "")
			keys = append(keys, fmt.Sprintf("group:%d", g.ID))
			groupRows = append(groupRows, indexOf[g.ID])
			selectable = append(selectable, true)
		}
	}
	r.groupRows = groupRows
	r.list.SetTableSelectable([]components.Column{
		col("分流组（层内序号）", 20, 0, false),
		col("目标", 12, 0, false),
		col("规则", 5, 0, true),
		col("状态", 4, 0, false),
		col("来源", 6, 3, false), // 窄屏隐藏；预置组删除后可按 P 恢复
	}, rows, keys, selectable)
}

// reloadGroupRules 重建当前分流组的规则列表。
func (r *Rules) reloadGroupRules() {
	if r.cur == nil {
		return
	}
	selected := r.list.SelectedKey()
	if g, err := r.app.Rout.DB.ListRoutingGroups(); err == nil {
		for _, gg := range g {
			if gg.ID == r.cur.ID {
				r.cur = gg
				break
			}
		}
	}
	r.list.Items = nil
	r.list.Keys = nil
	for _, rule := range r.cur.Rules {
		state := "启用"
		if !rule.Enabled {
			state = "停用"
		}
		typ := rule.Type
		val := rule.Value
		switch rule.Type {
		case "final":
			val = "（其余全部流量）"
		case "logical":
			typ = "logical:" + rule.Mode
			val = config.FormatLogicalExpr(rule.Mode, rule.Conditions)
		}
		mark := ""
		if rule.Invert {
			mark = "!"
		}
		r.list.Items = append(r.list.Items,
			fmt.Sprintf("%-15s %s%-40s %s", typ, mark, val, state))
		r.list.Keys = append(r.list.Keys, fmt.Sprintf("rule:%d", rule.ID))
	}
	r.list.SelectKey(selected)
}

// selectedGroup 返回光标所在的分流组。光标落在层标题上时返回 false：
// 标题行不是可操作对象，不能对它删除/编辑/启停。
func (r *Rules) selectedGroup() (*config.RoutingGroup, bool) {
	if r.list.Cursor < 0 || r.list.Cursor >= len(r.groupRows) {
		return nil, false
	}
	idx := r.groupRows[r.list.Cursor]
	if idx < 0 || idx >= len(r.groups) {
		return nil, false
	}
	return r.groups[idx], true
}

// selectedSet 返回光标所在的自定义规则集。光标落在只读的内置分类行上时返回 false，
// 避免把 [内置] 行当成可删除/可启停的表记录。
func (r *Rules) selectedSet() (*config.RuleSet, bool) {
	if r.list.Cursor < 0 || r.list.Cursor >= len(r.sets) {
		return nil, false
	}
	return r.sets[r.list.Cursor], true
}

// selectedSetCatalog 返回光标所在的只读内置分类行。
func (r *Rules) selectedSetCatalog() (catalog.Ref, bool) {
	i := r.list.Cursor - len(r.sets)
	if i < 0 || i >= len(r.catRefsInUse) {
		return catalog.Ref{}, false
	}
	return r.catRefsInUse[i], true
}

func (r *Rules) selectedRuleIdx() (int, bool) {
	if r.list.Cursor < 0 || r.cur == nil || r.list.Cursor >= len(r.cur.Rules) {
		return 0, false
	}
	return r.list.Cursor, true
}

// --- 内置分类库（4.3）---

// openCatalog 进入内置分类库浏览。back 是退出后返回的模式：
// 从规则列表进入时选中分类可直接加为当前分流组的规则，从规则集管理进入时仅浏览。
func (r *Rules) openCatalog(back rulesMode) {
	r.catalogBackList = r.list
	r.list = components.SimpleList{}
	r.catBack = back
	if r.catKind == "" {
		r.catKind = catalog.KindGeosite
	}
	r.catSearch = textinput.New()
	r.catSearch.Placeholder = "输入关键词过滤，Enter 确认，Esc 清空"
	r.catSearch.CharLimit = 0
	r.catSearch.SetValue(r.catQuery)
	r.catTyping = false
	r.err = nil
	r.mode = rulesCatalog
	r.list.Cursor = 0
	r.reloadCatalog()
}

// reloadCatalog 按当前种类与搜索词重建分类列表，并标注缓存与引用状态。
func (r *Rules) reloadCatalog() {
	selected := r.list.SelectedKey()
	r.catHits = catalog.Search(r.catKind, r.catQuery, 0)

	// 已被规则引用的分类（用于标注），失败不致命
	referenced := map[string]bool{}
	if refs, err := r.app.Rules.ReferencedCatalog(); err == nil {
		for _, ref := range refs {
			referenced[ref.Tag()] = true
		}
	}

	r.list.Height = r.catalogViewHeight()
	r.list.Items = nil
	r.list.Keys = nil
	for _, ref := range r.catHits {
		cached := r.app.Rules.CatalogCached(ref)
		mark := "  "
		if referenced[ref.Tag()] {
			mark = "* " // 已被规则引用
		}
		state := "未缓存"
		if cached {
			state = "已缓存"
		}
		ipHint := ""
		if ref.NeedsResolve() {
			ipHint = " [IP类]"
		}
		r.list.Items = append(r.list.Items,
			fmt.Sprintf("%s%-42s %-8s%s", mark, ref.String(), state, ipHint))
		r.list.Keys = append(r.list.Keys, ref.String())
	}
	r.list.SelectKey(selected)
}

// catalogViewHeight 分类列表可见行数：给标题/搜索框/状态栏/帮助行留出空间。
func (r *Rules) catalogViewHeight() int {
	h := r.height - 9
	if h < 5 {
		h = 5
	}
	return h
}

func (r *Rules) selectedCatalogRef() (catalog.Ref, bool) {
	if r.list.Cursor < 0 || r.list.Cursor >= len(r.catHits) {
		return catalog.Ref{}, false
	}
	return r.catHits[r.list.Cursor], true
}

func (r *Rules) handleCatalogKey(msg tea.KeyMsg) (Page, tea.Cmd) {
	key := msg.String()

	// 搜索输入态：只有 Enter/Esc 退出输入，其余按键交给输入框
	if r.catTyping {
		switch key {
		case "enter":
			r.catTyping = false
			r.catSearch.Blur()
			r.catQuery = strings.TrimSpace(r.catSearch.Value())
			r.list.Cursor = 0
			r.reloadCatalog()
			return r, nil
		case "esc":
			r.catTyping = false
			r.catSearch.Blur()
			r.catQuery = r.catBefore
			r.list = r.catBeforeList
			r.catSearch.SetValue(r.catQuery)
			r.reloadCatalog()
			return r, nil
		}
		var cmd tea.Cmd
		r.catSearch, cmd = r.catSearch.Update(msg)
		// 边输入边过滤
		r.catQuery = strings.TrimSpace(r.catSearch.Value())
		r.list.Cursor = 0
		r.reloadCatalog()
		return r, cmd
	}

	if consumed, cmd := r.list.Update(msg); consumed {
		return r, cmd
	}
	switch key {
	case "esc":
		r.mode = r.catBack
		r.list = r.catalogBackList
		if r.catBack == rulesGroupRl {
			r.reloadGroupRules()
		} else {
			r.reload()
		}
	case "/":
		r.catBefore = r.catQuery
		r.catBeforeList = r.list
		r.catTyping = true
		r.catSearch.Focus()
		return r, textinput.Blink
	case "]", "[":
		// 循环切换 geosite → geoip → acl
		for i, k := range catalog.Kinds {
			if k == r.catKind {
				delta := 1
				if key == "[" {
					delta = len(catalog.Kinds) - 1
				}
				r.catKind = catalog.Kinds[(i+delta)%len(catalog.Kinds)]
				break
			}
		}
		r.list.Cursor = 0
		r.reloadCatalog()
	case "enter":
		// 加为当前分流组的 rule_set 规则（仅从规则列表进入时可用）
		if r.catBack != rulesGroupRl || r.cur == nil {
			if ref, ok := r.selectedCatalogRef(); ok {
				r.openDetails("内置分类详情", r.catalogDetails(ref))
			}
			return r, nil
		}
		ref, ok := r.selectedCatalogRef()
		if !ok {
			return r, nil
		}
		r.addCatalogRule(ref)
	case "u":
		// 下载/更新选中分类的缓存
		if ref, ok := r.selectedCatalogRef(); ok && !r.busy {
			return r, r.downloadCatalog(ref)
		}
	case "alt+enter":
		if ref, ok := r.selectedCatalogRef(); ok {
			r.openDetails("内置分类详情", r.catalogDetails(ref))
		}
	case "c":
		r.catQuery = ""
		r.catSearch.SetValue("")
		r.reloadCatalog()
	}
	return r, nil
}

// addCatalogRule 把选中的分类作为 rule_set 规则追加到当前分流组。
func (r *Rules) addCatalogRule(ref catalog.Ref) {
	for _, rule := range r.cur.Rules {
		if rule.Type == "rule_set" {
			for _, v := range strings.Split(rule.Value, ",") {
				if strings.TrimSpace(v) == ref.String() {
					r.status = fmt.Sprintf("%s 已在分流组 %q 中", ref, r.cur.Name)
					return
				}
			}
		}
	}
	rules := append(r.cur.Rules, config.Rule{
		Type: "rule_set", Value: ref.String(), Enabled: true,
	})
	if err := r.app.Rout.UpdateGroup(r.cur, rules); err != nil {
		r.err = err
		r.reloadGroupRules()
		return
	}
	r.err = nil
	r.status = fmt.Sprintf("已添加 %s → 分流组 %q（缺缓存会在生成配置时自动下载）", ref, r.cur.Name)
	r.app.MarkConfigDirty()
	r.reloadCatalog()
}

// downloadCatalog 异步下载单个分类的规则集缓存。
func (r *Rules) downloadCatalog(ref catalog.Ref) tea.Cmd {
	return r.downloadWork(nil, []catalog.Ref{ref}, false)
}

// --- 表单 ---

func (r *Rules) openForm(kind string) {
	r.formKind = kind
	switch kind {
	case "add-rg":
		r.form = components.NewForm("新建分流组",
			[]string{"名称", "目标", "层"},
			[]string{"name", "target", "kind"},
			[]string{"如 流媒体", "DIRECT / BLOCK / 代理组名", "层序即优先级：custom < geosite < geoip < acl < final"})
	case "add-rule":
		r.form = components.NewForm("添加规则（"+r.cur.Name+"）",
			[]string{"类型", "值", "反转"},
			[]string{"type", "value", "invert"},
			[]string{"domain/domain_suffix/domain_keyword/domain_regex/ip_cidr/geoip/geosite/rule_set/final/logical", "多值逗号分隔；rule_set 可填内置分类如 geosite:cn（按 c 挑选）；domain_regex 整个值即一条正则；logical 用 条件&&条件", "true/false，默认 false"})
	case "add-rs":
		r.form = components.NewForm("添加规则集",
			[]string{"名称", "Tag", "URL", "格式"},
			[]string{"name", "tag", "url", "format"},
			[]string{"如 我的规则", "如 my-rules", "远程 .srs/.json 地址", "srs 或 json，留空 srs"})
	}
	r.form.Reset()
	r.err = nil
	r.configureForm()
	r.mode = rulesForm
}

func (r *Rules) openFormEditGroup(g *config.RoutingGroup) {
	r.formKind = "edit-rg"
	r.editID = g.ID
	r.form = components.NewForm("编辑分流组",
		[]string{"名称", "目标", "层"},
		[]string{"name", "target", "kind"},
		[]string{"", "DIRECT / BLOCK / 代理组名", "改层会改变该组的优先级归属"})
	r.form.SetValueByKey("name", g.Name)
	r.form.SetValueByKey("target", g.Target)
	r.form.SetValueByKey("kind", g.Layer())
	r.err = nil
	r.configureForm()
	r.mode = rulesForm
}

func (r *Rules) openFormEditRule(idx int) {
	r.formKind = "edit-rule"
	r.editRule = idx
	rule := r.cur.Rules[idx]
	r.form = components.NewForm("编辑规则（"+r.cur.Name+"）",
		[]string{"类型", "值", "反转"},
		[]string{"type", "value", "invert"},
		[]string{"domain/domain_suffix/domain_keyword/domain_regex/ip_cidr/geoip/geosite/rule_set/final/logical", "多值逗号分隔；rule_set 可填内置分类如 geosite:cn（按 c 挑选）；domain_regex 整个值即一条正则；logical 用 条件&&条件", "true/false，默认 false"})
	r.form.SetValueByKey("type", rule.Type)
	if rule.Type == "logical" {
		r.form.SetValueByKey("value", config.FormatLogicalExpr(rule.Mode, rule.Conditions))
	} else {
		r.form.SetValueByKey("value", rule.Value)
	}
	r.form.SetValueByKey("invert", strconv.FormatBool(rule.Invert))
	r.err = nil
	r.configureForm()
	r.mode = rulesForm
}

func (r *Rules) formBackMode() rulesMode {
	switch r.formKind {
	case "add-rg", "edit-rg":
		return rulesGroups
	case "add-rs":
		return rulesSets
	default:
		return rulesGroupRl
	}
}

func (r *Rules) submitForm() {
	switch r.formKind {
	case "add-rg", "edit-rg":
		r.submitGroupForm()
	case "add-rule", "edit-rule":
		r.submitRuleForm()
	case "add-rs":
		r.submitRulesetForm()
	}
}

func (r *Rules) submitGroupForm() {
	name := r.form.ValueByKey("name")
	target := strings.TrimSpace(r.form.ValueByKey("target"))
	kind := config.KindNormalize(r.form.ValueByKey("kind"))
	if strings.EqualFold(target, "DIRECT") || strings.EqualFold(target, "BLOCK") {
		target = strings.ToUpper(target)
	}
	if name == "" || target == "" {
		field := "name"
		if name != "" {
			field = "target"
		}
		r.err = validation.New(field, "名称与目标为必填项")
		return
	}
	if r.formKind == "add-rg" {
		if _, err := r.app.Rout.CreateGroupIn(name, target, kind, nil); err != nil {
			r.err = err
			return
		}
		r.status = fmt.Sprintf("分流组已创建（%s 层），enter 添加规则", config.KindLabel(kind))
	} else {
		g, err := r.app.DB.GetRoutingGroup(r.editID)
		if err != nil {
			r.err = err
			r.mode = rulesGroups
			return
		}
		g.Name, g.Target, g.Kind = name, target, kind
		if err := r.app.Rout.UpdateGroup(g, g.Rules); err != nil {
			r.err = err
			return
		}
		r.status = "分流组已保存"
	}
	r.err = nil
	r.app.MarkConfigDirty()
	r.mode = rulesGroups
	r.reload()
}

func (r *Rules) submitRuleForm() {
	typ := strings.ToLower(strings.TrimSpace(r.form.ValueByKey("type")))
	value := strings.TrimSpace(r.form.ValueByKey("value"))
	invert := false
	if v := strings.TrimSpace(r.form.ValueByKey("invert")); v != "" && typ != "final" {
		var err error
		if invert, err = strconv.ParseBool(v); err != nil {
			r.err = validation.New("invert", "反转须为 true 或 false")
			return
		}
	}
	rule := config.Rule{Type: typ, Value: value, Invert: invert, Enabled: true}
	if typ == "logical" {
		mode, conds, err := config.ParseLogicalExpr(value)
		if err != nil {
			r.err = validation.At("value", err)
			return
		}
		rule.Value, rule.Mode, rule.Conditions = "", mode, conds
	}
	if r.formKind == "edit-rule" {
		rule.ID = r.cur.Rules[r.editRule].ID
		rule.Enabled = r.cur.Rules[r.editRule].Enabled
		r.cur.Rules[r.editRule] = rule
	} else {
		r.cur.Rules = append(r.cur.Rules, rule)
	}
	if err := r.app.Rout.UpdateGroup(r.cur, r.cur.Rules); err != nil {
		r.err = err
		// 失败时回滚内存改动
		r.reloadGroupRules()
		return
	}
	r.err = nil
	r.status = "规则已保存"
	r.app.MarkConfigDirty()
	r.mode = rulesGroupRl
	r.reloadGroupRules()
}

func (r *Rules) submitRulesetForm() {
	name := r.form.ValueByKey("name")
	tag := r.form.ValueByKey("tag")
	url := r.form.ValueByKey("url")
	format := r.form.ValueByKey("format")
	if _, err := r.app.Rules.AddRuleSet(name, tag, url, format); err != nil {
		r.err = err
		return
	}
	r.err = nil
	r.status = "规则集已添加，按 u 下载缓存"
	r.app.MarkConfigDirty()
	r.mode = rulesSets
	r.reload()
}

// --- 异步下载 ---

// applyPresetTask 在后台执行地区预置对齐，随后生成并校验配置。
// 逐条结果走与其他任务相同的回执通道（v 查看、f 重试语义不适用，故只给汇总）。
func (r *Rules) applyPresetTask() tea.Cmd {
	r.busy = true
	r.err = nil
	r.status = "正在导入地区预置…"
	region := routing.PresetCN
	if len(routing.PresetRegions) > 0 {
		region = routing.PresetRegions[0]
	}
	return r.taskN("地区预置导入", 0, func(report func(itemResult)) tea.Msg {
		ctx, cancel := context.WithTimeout(r.app.BackgroundContext(), 30*time.Minute)
		defer cancel()
		result, err := r.app.Rout.ApplyPreset(region, routing.PresetMerge)
		if err != nil {
			return actionDoneMsg{Action: "apply-preset", Data: region, Err: err}
		}
		results := make([]itemResult, 0, len(result.Items))
		for _, item := range result.Items {
			row := itemResult{ID: 0, Key: item.Name, Name: item.Name, State: item.Action, Detail: item.Detail}
			results = append(results, row)
		}
		// 预置只新增了内置分类引用：生成一次配置，把缺缓存的部分按需补齐（失败不致命，
		// 生成器会回退 remote，由 sing-box 启动时下载）
		if genErr := r.app.GenerateConfig(ctx); genErr != nil {
			results = append(results, itemResult{Name: "生成配置", State: "失败", Detail: genErr.Error()})
		} else if checkErr := r.app.CheckConfig(ctx); checkErr != nil {
			results = append(results, itemResult{
				Name: "校验配置", State: "失败",
				Detail: checkErr.Error() + "（可能是规则集未下载，执行 ruleset update 后重试）",
			})
		}
		return actionDoneMsg{Action: "apply-preset", Data: result.Summary(), Results: results}
	})
}

func (r *Rules) downloadRuleSets(ids []int64, doing string) tea.Cmd {
	return r.downloadWork(ids, nil, false)
}

func (r *Rules) downloadAll(ids []int64) tea.Cmd {
	refs, err := r.app.Rules.ReferencedCatalog()
	if err != nil {
		r.err = err
		return nil
	}
	return r.downloadWork(ids, refs, true)
}

func (r *Rules) downloadWork(ids []int64, refs []catalog.Ref, enabledOnly bool) tea.Cmd {
	if r.busy {
		return nil
	}
	if len(ids)+len(refs) == 0 {
		r.status = "没有需要下载的规则集"
		return nil
	}
	r.busy = true
	r.err = nil
	r.status = "规则集下载进行中"
	r.retryTask = func() tea.Cmd {
		var retryIDs []int64
		var retryRefs []catalog.Ref
		for _, result := range r.taskResults {
			if result.State != "失败" {
				continue
			}
			if result.ID != 0 {
				retryIDs = append(retryIDs, result.ID)
			} else if ref, ok := catalog.Parse(result.Key); ok {
				retryRefs = append(retryRefs, ref)
			}
		}
		return r.downloadWork(retryIDs, retryRefs, true)
	}
	return r.taskN("规则集下载", len(ids)+len(refs), func(report func(itemResult)) tea.Msg {
		ctx, cancel := context.WithTimeout(r.app.BackgroundContext(), 30*time.Minute)
		defer cancel()
		var results []itemResult
		record := func(row itemResult, err error) {
			if err != nil {
				row.State, row.Detail = "失败", err.Error()
			} else if row.State == "成功" {
				row.Detail = "缓存已更新；应用配置后生效"
				r.app.MarkConfigDirty()
			}
			results = append(results, row)
			report(row)
		}
		for _, id := range ids {
			row := itemResult{ID: id, Name: fmt.Sprintf("规则集 %d", id), State: "成功"}
			sets, err := r.app.DB.ListRuleSets()
			var rs *config.RuleSet
			if err == nil {
				for _, candidate := range sets {
					if candidate.ID == id {
						rs = candidate
						break
					}
				}
				if rs == nil {
					err = fmt.Errorf("规则集已删除，请刷新列表")
				}
			}
			if err == nil {
				row.Name = rs.Name
				if enabledOnly && !rs.Enabled {
					row.State, row.Detail = "跳过", "规则集已停用，未发起下载"
				} else {
					err = r.app.Rules.Download(ctx, id)
				}
			}
			record(row, err)
		}
		for _, ref := range refs {
			record(itemResult{Key: ref.String(), Name: ref.String(), State: "成功"}, r.app.Rules.DownloadCatalog(ctx, ref))
		}
		return actionDoneMsg{Action: "download-rulesets", Results: results, Err: resultError(results)}
	})
}

func (r *Rules) onConfirm(msg components.ConfirmMsg) (Page, tea.Cmd) {
	if r.mode != rulesConfirm {
		return r, nil
	}
	if msg.ID == "delete-rule" {
		r.mode = rulesGroupRl
		if msg.Confirmed {
			r.reloadGroupRules()
			for i, rule := range r.cur.Rules {
				if rule.ID == r.confirmID {
					rules := append([]config.Rule(nil), r.cur.Rules[:i]...)
					rules = append(rules, r.cur.Rules[i+1:]...)
					if err := r.app.Rout.UpdateGroup(r.cur, rules); err != nil {
						r.err = err
					} else {
						r.err = nil
						r.status = "规则已删除"
						r.app.MarkConfigDirty()
					}
					break
				}
			}
		}
		r.reloadGroupRules()
		return r, nil
	}
	if r.confirmKind == "move-layer" {
		target, group := r.moveKind, r.moveTarget
		r.list = r.moveBack
		r.moveTarget, r.moveKinds = nil, nil
		r.mode = rulesGroups
		if msg.Confirmed && msg.ID == r.confirmKind {
			name := "分组"
			if group != nil {
				name = group.Name
				if err := r.app.Rout.MoveGroupToKind(group.ID, target, -1); err != nil {
					r.err = err
					r.status = ""
					r.reload()
					return r, nil
				}
				r.err = nil
				r.app.MarkConfigDirty()
				r.status = fmt.Sprintf("%s 已移入 %s 层（层尾）", name, config.KindLabel(target))
			}
		}
		r.reload()
		return r, nil
	}
	if r.confirmKind == "apply-preset" {
		r.mode = rulesGroups
		if msg.Confirmed && msg.ID == r.confirmKind {
			return r, r.applyPresetTask()
		}
		r.reload()
		return r, nil
	}
	backTo := rulesGroups
	if r.confirmKind == "delete-rs" {
		backTo = rulesSets
	}
	r.mode = backTo
	if msg.Confirmed && msg.ID == r.confirmKind {
		switch r.confirmKind {
		case "delete-rg":
			if err := r.app.Rout.DeleteGroup(r.confirmID); err != nil {
				r.err = err
			} else {
				r.err = nil
				r.status = "分流组已删除"
				r.app.MarkConfigDirty()
			}
		case "delete-rs":
			if err := r.app.Rules.DeleteRuleSet(r.confirmID); err != nil {
				r.err = err
			} else {
				r.err = nil
				r.status = "规则集已删除"
				r.app.MarkConfigDirty()
			}
		}
	}
	r.reload()
	return r, nil
}

// --- 渲染 ---

func (r *Rules) View() string {
	r.table()
	r.preview = r.selectionDetails()
	if r.detailActive {
		return r.detailsView()
	}
	switch r.mode {
	case rulesForm:
		if r.logical != nil {
			return r.logical.view(r.width, r.height)
		}
		return r.formView(&r.form, r.err)
	case rulesConfirm:
		return r.confirmView(&r.confirm)
	case rulesCatalog:
		return r.catalogView()
	case rulesMove:
		head := "移动到其他层 · 层序即优先级（上→下）"
		if r.moveTarget != nil {
			head = fmt.Sprintf("把 %q 移动到其他层（当前 %s）· 层序即优先级（上→下）",
				r.moveTarget.Name, config.KindLabel(r.moveTarget.Layer()))
		}
		return r.listView(&r.moveList, head,
			"没有可选的层。", r.statusLine(), "Enter 选择目标层 · Esc 取消")
	case rulesSets:
		return r.listView(&r.list, "分流组  [规则集] · [/] 切换", "暂无规则集，c 浏览分类库。", r.statusLine(), Hints(r))
	case rulesGroupRl:
		return r.listView(&r.list, fmt.Sprintf("分流 / %s → %s · 顺序即优先级", r.cur.Name, r.cur.Target),
			"暂无规则，a 添加。", r.statusLine(), Hints(r))
	default:
		head := "[分流组]  规则集 · [/] 切换 · 层序即优先级"
		if r.enabledOnly {
			head += " · 仅显示启用（f 显示全部）"
		}
		return r.listView(&r.list, head, "暂无分流组，a 新建。", r.statusLine(), Hints(r))
	}
}

func (r *Rules) catalogView() string {
	head := fmt.Sprintf("分类库 / [%s] · %d 条 · [/] 种类 · %s", r.catKind, len(r.catHits), r.catQuery)
	if r.catBack == rulesGroupRl && r.cur != nil {
		head += "\n添加到: " + r.cur.Name + " → " + r.cur.Target
	}
	if r.catTyping {
		r.catSearch.Width = max(5, r.width-10)
		head += "\n搜索: " + r.catSearch.View()
	}
	hint := "/ 搜索 · [/] 种类 · u 下载 · Esc 返回"
	if r.catBack == rulesGroupRl && r.cur != nil {
		hint = "Enter 加入 " + r.cur.Name + " · " + hint
	}
	return r.listView(&r.list, head, "无匹配分类，/ 修改搜索。", r.statusLine(), hint)
}

func (r *Rules) statusLine() string {
	return r.feedback(r.status, r.err)
}

func (r *Rules) configureForm() {
	r.form.SetChoices("type", ruleTypeChoices(true))
	r.form.SetChoices("target", proxyChoices(r.app, true))
	// 层：新建时默认 custom（层序最高、语义最安全）。规则构成对应的建议值由
	// config.SuggestKind 在「不带 --kind 直接建组」的路径给出（CLI / 模型层），
	// 表单这里以层序提示为主，用户可显式改。
	if field := r.form.Field("kind"); field != nil {
		r.form.SetChoices("kind", kindChoices())
		field.Help = "层序即优先级：" + strings.Join(config.Kinds, " < ") + "。混合种类的规则组请留在 custom 层。"
		field.Validate = func(value string) error {
			if !config.KindValid(value) {
				return fmt.Errorf("未知层 %q（可选 %s）", value, strings.Join(config.Kinds, " / "))
			}
			return nil
		}
	}
	configureRuleValue(r.app, &r.form)
	for _, key := range []string{"name", "tag", "target"} {
		if field := r.form.Field(key); field != nil {
			field.Validate = required(field.Label)
		}
	}
	if field := r.form.Field("target"); field != nil {
		field.Validate = func(value string) error {
			if strings.EqualFold(value, "DIRECT") || strings.EqualFold(value, "BLOCK") {
				r.form.SetValueByKey("target", strings.ToUpper(value))
			}
			return required("目标")(value)
		}
	}
	if field := r.form.Field("url"); field != nil {
		field.Validate = httpURL(false)
	}
	r.form.Begin()
}
