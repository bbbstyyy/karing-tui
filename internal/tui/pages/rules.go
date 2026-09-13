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
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
)

// rulesMode Rules 页的视图模式。
type rulesMode int

const (
	rulesGroups  rulesMode = iota // 分流组列表
	rulesGroupRl                  // 分流组规则列表
	rulesForm                     // 表单（分流组/规则/规则集）
	rulesSets                     // 规则集管理
	rulesConfirm                  // 删除确认
	rulesCatalog                  // 内置分类库浏览/挑选
)

// Rules 分流管理页：分流组列表、规则编辑（排序/启停）、规则集管理与下载、
// 内置分类库浏览与挑选。
type Rules struct {
	base
	mode   rulesMode
	groups []*config.RoutingGroup
	cur    *config.RoutingGroup
	sets   []*config.RuleSet
	list   components.SimpleList

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

func (r *Rules) Title() string { return "Rules" }

// Editing 表单输入与分类库搜索输入状态时拦截全局键位。
func (r *Rules) Editing() bool { return r.mode == rulesForm || (r.mode == rulesCatalog && r.catTyping) }

func (r *Rules) Init() tea.Cmd { return nil }

func (r *Rules) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		r.SetSize(msg.Width, msg.Height)
		return r, nil
	case ActivateMsg:
		r.reload()
		return r, nil
	case actionDoneMsg:
		r.busy = false
		if msg.Err != nil {
			r.err = msg.Err
			r.status = ""
		} else {
			r.err = nil
			switch msg.Action {
			case "download-rulesets":
				r.status = "规则集下载完成"
			case "download-catalog":
				r.status = msg.Data + " 已缓存"
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
	return r, nil
}

func (r *Rules) handleKey(msg tea.KeyMsg) (Page, tea.Cmd) {
	key := msg.String()
	switch r.mode {
	case rulesForm:
		switch key {
		case "esc":
			r.mode = r.formBackMode()
			return r, nil
		case "enter":
			r.submitForm()
			return r, nil
		}
		r.form.Update(msg)
		return r, nil

	case rulesConfirm:
		if consumed, cmd := r.confirm.Update(msg); consumed {
			return r, cmd
		}
		return r, nil

	case rulesCatalog:
		return r.handleCatalogKey(msg)

	case rulesSets:
		if consumed, cmd := r.list.Update(msg); consumed {
			return r, cmd
		}
		switch key {
		case "esc":
			r.mode = rulesGroups
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
					if rs.Enabled {
						ids = append(ids, rs.ID)
					}
				}
				return r, r.downloadAll(ids)
			}
		case " ":
			if rs, ok := r.selectedSet(); ok {
				if err := r.app.Rules.SetEnabled(rs.ID, !rs.Enabled); err != nil {
					r.err = err
				}
				r.reload()
			}
		}
		return r, nil

	case rulesGroupRl:
		if consumed, cmd := r.list.Update(msg); consumed {
			return r, cmd
		}
		switch key {
		case "esc":
			r.mode = rulesGroups
			r.reload()
		case "a":
			r.openForm("add-rule")
		case "c":
			r.openCatalog(rulesGroupRl)
		case "e":
			if idx, ok := r.selectedRuleIdx(); ok {
				r.openFormEditRule(idx)
			}
		case "D":
			if idx, ok := r.selectedRuleIdx(); ok {
				r.cur.Rules = append(r.cur.Rules[:idx], r.cur.Rules[idx+1:]...)
				if err := r.app.Rout.UpdateGroup(r.cur, r.cur.Rules); err != nil {
					r.err = err
					r.reloadGroupRules()
				} else {
					r.err = nil
					r.status = "规则已删除"
					r.reloadGroupRules()
				}
			}
		case " ":
			if idx, ok := r.selectedRuleIdx(); ok {
				r.cur.Rules[idx].Enabled = !r.cur.Rules[idx].Enabled
				if err := r.app.Rout.UpdateGroup(r.cur, r.cur.Rules); err != nil {
					r.err = err
				} else {
					r.err = nil
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
					// 移动后光标跟随
					nxt := idx + delta
					if nxt >= 0 && nxt < len(r.cur.Rules) {
						r.list.Cursor = nxt
					}
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
				r.cur = g
				r.mode = rulesGroupRl
				r.reloadGroupRules()
			}
		case " ":
			if g, ok := r.selectedGroup(); ok {
				if err := r.app.Rout.SetEnabled(g.ID, !g.Enabled); err != nil {
					r.err = err
				}
				r.reload()
			}
		case "R":
			r.mode = rulesSets
			r.reload()
		case "r":
			r.reload()
		}
		return r, nil
	}
}

// --- 数据加载 ---

func (r *Rules) reload() {
	if r.mode == rulesCatalog {
		r.reloadCatalog()
		return
	}
	groups, err := r.app.DB.ListRoutingGroups()
	r.groups, r.err = groups, err
	sets, err := r.app.DB.ListRuleSets()
	if err == nil {
		r.sets = sets
	}
	if r.mode == rulesSets {
		r.list.Height = 0
		r.list.Items = nil
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
			}
		} else {
			r.catRefsInUse = nil
		}
		r.clampCursor(len(r.sets) + len(r.catRefsInUse))
		return
	}
	r.list.Height = 0
	r.list.Items = nil
	for _, g := range groups {
		state := "启用"
		if !g.Enabled {
			state = "停用"
		}
		r.list.Items = append(r.list.Items,
			fmt.Sprintf("%-14s → %-10s %2d 条规则  %s", g.Name, g.Target, len(g.Rules), state))
	}
	r.clampCursor(len(groups))
}

// reloadGroupRules 重建当前分流组的规则列表。
func (r *Rules) reloadGroupRules() {
	if r.cur == nil {
		return
	}
	if g, err := r.app.Rout.DB.ListRoutingGroups(); err == nil {
		for _, gg := range g {
			if gg.ID == r.cur.ID {
				r.cur = gg
				break
			}
		}
	}
	r.list.Items = nil
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
	}
	r.clampCursor(len(r.cur.Rules))
}

func (r *Rules) selectedGroup() (*config.RoutingGroup, bool) {
	if r.list.Cursor < 0 || r.list.Cursor >= len(r.groups) {
		return nil, false
	}
	return r.groups[r.list.Cursor], true
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

func (r *Rules) clampCursor(n int) {
	if r.list.Cursor >= n {
		r.list.Cursor = n - 1
	}
	if r.list.Cursor < 0 {
		r.list.Cursor = 0
	}
}

// --- 内置分类库（4.3）---

// openCatalog 进入内置分类库浏览。back 是退出后返回的模式：
// 从规则列表进入时选中分类可直接加为当前分流组的规则，从规则集管理进入时仅浏览。
func (r *Rules) openCatalog(back rulesMode) {
	r.catBack = back
	if r.catKind == "" {
		r.catKind = catalog.KindGeosite
	}
	r.catSearch = textinput.New()
	r.catSearch.Placeholder = "输入关键词过滤，Enter 确认，Esc 清空"
	r.catSearch.CharLimit = 64
	r.catSearch.SetValue(r.catQuery)
	r.catTyping = false
	r.err = nil
	r.mode = rulesCatalog
	r.list.Cursor = 0
	r.reloadCatalog()
}

// reloadCatalog 按当前种类与搜索词重建分类列表，并标注缓存与引用状态。
func (r *Rules) reloadCatalog() {
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
	for _, ref := range r.catHits {
		cached := r.app.Rules.CatalogCached(ref)
		mark := "  "
		if referenced[ref.Tag()] {
			mark = "✓ " // 已被规则引用
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
	}
	r.clampCursor(len(r.catHits))
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
			r.catSearch.SetValue(r.catQuery)
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
		r.list.Height = 0
		r.list.Cursor = 0
		if r.catBack == rulesGroupRl {
			r.reloadGroupRules()
		} else {
			r.reload()
		}
	case "/":
		r.catTyping = true
		r.catSearch.Focus()
		return r, textinput.Blink
	case "tab":
		// 循环切换 geosite → geoip → acl
		for i, k := range catalog.Kinds {
			if k == r.catKind {
				r.catKind = catalog.Kinds[(i+1)%len(catalog.Kinds)]
				break
			}
		}
		r.list.Cursor = 0
		r.reloadCatalog()
	case "enter":
		// 加为当前分流组的 rule_set 规则（仅从规则列表进入时可用）
		if r.catBack != rulesGroupRl || r.cur == nil {
			r.status = "浏览模式：分类无需添加，在分流组规则里按 c 挑选即可直接加为规则"
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
	r.reloadCatalog()
}

// downloadCatalog 异步下载单个分类的规则集缓存。
func (r *Rules) downloadCatalog(ref catalog.Ref) tea.Cmd {
	r.busy = true
	r.status = fmt.Sprintf("正在下载 %s…", ref)
	r.err = nil
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		return actionDoneMsg{
			Action: "download-catalog",
			Data:   ref.String(),
			Err:    r.app.Rules.DownloadCatalog(ctx, ref),
		}
	}
}

// --- 表单 ---

func (r *Rules) openForm(kind string) {
	r.formKind = kind
	switch kind {
	case "add-rg":
		r.form = components.NewForm("新建分流组",
			[]string{"名称", "目标"},
			[]string{"name", "target"},
			[]string{"如 流媒体", "DIRECT / BLOCK / 代理组名"})
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
	r.mode = rulesForm
}

func (r *Rules) openFormEditGroup(g *config.RoutingGroup) {
	r.formKind = "edit-rg"
	r.editID = g.ID
	r.form = components.NewForm("编辑分流组",
		[]string{"名称", "目标"},
		[]string{"name", "target"},
		[]string{"", "DIRECT / BLOCK / 代理组名"})
	r.form.SetValueByKey("name", g.Name)
	r.form.SetValueByKey("target", g.Target)
	r.err = nil
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
	target := strings.ToUpper(strings.TrimSpace(r.form.ValueByKey("target")))
	if name == "" || target == "" {
		r.err = fmt.Errorf("名称与目标为必填项")
		return
	}
	if r.formKind == "add-rg" {
		if _, err := r.app.Rout.CreateGroup(name, target, nil); err != nil {
			r.err = err
			return
		}
		r.status = "分流组已创建，enter 添加规则"
	} else {
		g, err := r.app.DB.GetRoutingGroup(r.editID)
		if err != nil {
			r.err = err
			r.mode = rulesGroups
			return
		}
		g.Name, g.Target = name, target
		if err := r.app.Rout.UpdateGroup(g, g.Rules); err != nil {
			r.err = err
			return
		}
		r.status = "分流组已保存"
	}
	r.err = nil
	r.mode = rulesGroups
	r.reload()
}

func (r *Rules) submitRuleForm() {
	typ := strings.ToLower(strings.TrimSpace(r.form.ValueByKey("type")))
	value := strings.TrimSpace(r.form.ValueByKey("value"))
	invert := false
	if v := strings.TrimSpace(r.form.ValueByKey("invert")); v != "" {
		var err error
		if invert, err = strconv.ParseBool(v); err != nil {
			r.err = fmt.Errorf("反转须为 true 或 false")
			return
		}
	}
	rule := config.Rule{Type: typ, Value: value, Invert: invert, Enabled: true}
	if typ == "logical" {
		mode, conds, err := config.ParseLogicalExpr(value)
		if err != nil {
			r.err = err
			return
		}
		rule.Value, rule.Mode, rule.Conditions = "", mode, conds
	}
	if r.formKind == "edit-rule" {
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
	r.mode = rulesSets
	r.reload()
}

// --- 异步下载 ---

func (r *Rules) downloadRuleSets(ids []int64, doing string) tea.Cmd {
	r.busy = true
	r.status = doing
	r.err = nil
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		var firstErr error
		for _, id := range ids {
			if err := r.app.Rules.Download(ctx, id); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return actionDoneMsg{Action: "download-rulesets", Err: firstErr}
	}
}

// downloadAll 更新全部启用的自定义规则集，以及被规则引用到的内置分类缓存。
func (r *Rules) downloadAll(ids []int64) tea.Cmd {
	refs, err := r.app.Rules.ReferencedCatalog()
	if err != nil {
		r.err = err
		return nil
	}
	if len(ids) == 0 && len(refs) == 0 {
		r.status = "没有需要下载的规则集"
		return nil
	}
	r.busy = true
	r.status = fmt.Sprintf("正在下载 %d 个自定义规则集 + %d 个内置分类…", len(ids), len(refs))
	r.err = nil
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		var firstErr error
		for _, id := range ids {
			if err := r.app.Rules.Download(ctx, id); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if err := r.app.Rules.UpdateCatalogCached(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
		return actionDoneMsg{Action: "download-rulesets", Err: firstErr}
	}
}

func (r *Rules) onConfirm(msg components.ConfirmMsg) (Page, tea.Cmd) {
	if r.mode != rulesConfirm {
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
			}
		case "delete-rs":
			if err := r.app.Rules.DeleteRuleSet(r.confirmID); err != nil {
				r.err = err
			} else {
				r.err = nil
				r.status = "规则集已删除"
			}
		}
	}
	r.reload()
	return r, nil
}

// --- 渲染 ---

func (r *Rules) View() string {
	switch r.mode {
	case rulesForm:
		// 表单态也要渲染校验错误，否则提交被拒时用户看不到原因
		// （如 domain_regex 正则非法、规则集不存在）
		view := r.form.View()
		if r.err != nil {
			view += "\n" + styles.Err.Render(r.err.Error())
		}
		return view
	case rulesConfirm:
		return r.confirm.View()
	case rulesCatalog:
		return r.catalogView()
	case rulesSets:
		head := styles.Title.Render("规则集") +
			styles.Dim.Render(fmt.Sprintf(" — 自定义 %d · 引用中的内置分类 %d", len(r.sets), len(r.catRefsInUse))) + "\n"
		body := r.list.View("暂无规则集。按 c 浏览内置分类库。")
		return head + body + r.statusLine() +
			styles.Dim.Render("\nc 内置分类库 · u 下载选中 · U 全部更新 · a 添加自定义 · d 删除 · space 启停 · esc 返回")
	case rulesGroupRl:
		title := styles.Title.Render("规则") +
			styles.Dim.Render(fmt.Sprintf(" — %s → %s（%d 条，顺序即优先级）", r.cur.Name, r.cur.Target, len(r.cur.Rules)))
		body := r.list.View("暂无规则，按 a 添加。")
		return title + "\n" + body + r.statusLine() +
			styles.Dim.Render("\na 添加 · c 分类库挑选 · e 编辑 · D 删除 · space 启停 · J 下移 · K 上移 · esc 返回")
	default:
		head := styles.Title.Render("分流规则") + "\n"
		body := r.list.View("暂无分流组。")
		return head + body + r.statusLine() +
			styles.Dim.Render("\na 新建分流组 · e 编辑 · enter 规则 · space 启停 · R 规则集管理 · r 刷新")
	}
}

// catalogView 渲染内置分类库：种类标签页 + 搜索框 + 分类列表。
func (r *Rules) catalogView() string {
	// 视窗高度在这里取，进入分类库时由根布局同步给所有页面。
	// 可能还是旧值（甚至为 0）；View 在每次重绘前执行，取到的是最新尺寸。
	r.list.Height = r.catalogViewHeight()

	var b strings.Builder
	b.WriteString(styles.Title.Render("内置分类库"))

	// 种类标签页
	tabs := make([]string, 0, len(catalog.Kinds))
	for _, k := range catalog.Kinds {
		label := fmt.Sprintf("%s(%d)", k, catalog.Count(k))
		if k == r.catKind {
			tabs = append(tabs, styles.Accent.Render("["+label+"]"))
		} else {
			tabs = append(tabs, styles.Dim.Render(" "+label+" "))
		}
	}
	b.WriteString(" " + strings.Join(tabs, " ") + "\n")

	// 搜索框
	if r.catTyping {
		b.WriteString("搜索: " + r.catSearch.View() + "\n")
	} else if r.catQuery != "" {
		b.WriteString(styles.Dim.Render("搜索: ") + r.catQuery +
			styles.Dim.Render(fmt.Sprintf("  （%d 条匹配，按 / 修改）", len(r.catHits))) + "\n")
	} else {
		b.WriteString(styles.Dim.Render(fmt.Sprintf("按 / 搜索  共 %d 条", len(r.catHits))) + "\n")
	}

	empty := "无匹配分类，按 / 换个关键词。"
	b.WriteString(r.list.View(empty))
	b.WriteString(r.statusLine())

	help := "\n/ 搜索 · Tab 切换种类 · u 下载缓存 · esc 返回"
	if r.catBack == rulesGroupRl && r.cur != nil {
		help = fmt.Sprintf("\nEnter 加入分流组 %q · / 搜索 · Tab 切换种类 · u 下载缓存 · esc 返回", r.cur.Name)
	}
	b.WriteString(styles.Dim.Render(help))
	return b.String()
}

func (r *Rules) statusLine() string {
	var b string
	if r.busy {
		b += "\n" + styles.Accent.Render(r.status)
	} else if r.status != "" {
		b += "\n" + styles.Ok.Render(r.status)
	}
	if r.err != nil {
		b += "\n" + styles.Err.Render("错误: "+r.err.Error())
	}
	return b
}
