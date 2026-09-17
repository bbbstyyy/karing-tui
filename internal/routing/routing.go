// Package routing 实现分流组管理：分流组 CRUD、规则校验与排序、
// 默认分流方案初始化。
package routing

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/bbbstyyy/karing-tui/internal/validation"
	"regexp"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/catalog"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// Manager 分流管理器。
type Manager struct {
	DB   *storage.DB
	Logf func(format string, args ...any)

	// afterPlacement 仅供测试使用（C14 失败回滚验证）：每次 placement 写
	// 成功后以 1 起计数调用，返回错误即中止后续写。生产路径恒为 nil。
	afterPlacement func(n int) error

	// writeProbe 仅供测试使用（C14-AUDIT 失败回滚验证）：DeleteGroup 与
	// ApplyPreset(replace) 的事务内每次 DB 写之前以操作名调用，返回错误即中止
	// 整个事务。生产路径恒为 nil。
	writeProbe func(op string) error
}

// NewManager 创建分流管理器。logf 可为 nil。
func NewManager(db *storage.DB, logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{DB: db, Logf: logf}
}

// ruleTypes 合法的规则类型。
var ruleTypes = map[string]bool{
	"domain": true, "domain_suffix": true, "domain_keyword": true, "domain_regex": true,
	"ip_cidr": true, "geoip": true, "geosite": true,
	"rule_set": true, "final": true, "logical": true,
}

// CreateGroup 新建分流组（含规则）。层按规则构成推断（config.SuggestKind）：
// 全部引用同一个分类库种类时落到对应层，否则落 custom 层。
func (m *Manager) CreateGroup(name, target string, rules []config.Rule) (*config.RoutingGroup, error) {
	return m.CreateGroupIn(name, target, "", rules)
}

// CreateGroupIn 新建分流组，kind 显式指定层（空值 → 按规则构成推断）。
// 新组追加到目标层的末尾（Position = 层内最大序号 + 1）。
func (m *Manager) CreateGroupIn(name, target, kind string, rules []config.Rule) (*config.RoutingGroup, error) {
	if strings.TrimSpace(kind) == "" {
		kind = config.SuggestKind(rules)
	}
	if !config.KindValid(kind) {
		return nil, validation.New("kind", "未知分流层 %q（可选: %s）", kind, strings.Join(config.Kinds, " / "))
	}
	kind = config.KindNormalize(kind)
	g := &config.RoutingGroup{
		Name:    strings.TrimSpace(name),
		Target:  strings.TrimSpace(target),
		Kind:    kind,
		Enabled: true,
		Rules:   rules,
	}
	if err := m.validate(g, 0); err != nil {
		return nil, err
	}
	existing, err := m.DB.ListRoutingGroups()
	if err != nil {
		return nil, err
	}
	g.Position = nextLayerPosition(existing, kind)
	if err := m.DB.CreateRoutingGroup(g); err != nil {
		return nil, err
	}
	m.Logf("创建分流组 %q → %s（%s 层，%d 条规则）", g.Name, g.Target, config.KindLabel(g.Kind), len(rules))
	return g, nil
}

// nextLayerPosition 返回某层内可用的下一个序号（层内最大 + 1；空层为 0）。
func nextLayerPosition(groups []*config.RoutingGroup, kind string) int {
	next := 0
	for _, g := range groups {
		if config.KindNormalize(g.Kind) == kind && g.Position >= next {
			next = g.Position + 1
		}
	}
	return next
}

// UpdateGroup 更新分流组（名称/目标/层/规则整体替换）。
// 层发生变化时等价于一次跨层移动：追加到目标层末尾并重排两层的层内序号。
func (m *Manager) UpdateGroup(g *config.RoutingGroup, rules []config.Rule) error {
	g.Name = strings.TrimSpace(g.Name)
	g.Target = strings.TrimSpace(g.Target)
	g.Kind = config.KindNormalize(g.Kind)
	g.Rules = rules
	if err := m.validate(g, g.ID); err != nil {
		return err
	}
	current, err := m.getGroup(g.ID)
	if err != nil {
		return err
	}
	source := config.KindNormalize(current.Kind)
	if source != g.Kind {
		return m.relocate(g, g.Kind, -1)
	}
	if err := m.DB.UpdateRoutingGroup(g); err != nil {
		return err
	}
	m.Logf("修改分流组 %q → %s", g.Name, g.Target)
	return nil
}

// MoveGroup 在层内上移/下移分流组（delta: -1 上移 / +1 下移）；已到边界时不动。
// 层内顺序即优先级，跨层移动必须用 MoveGroupToKind（显式操作）。
// 重排写入在单个事务中完成（C14），读取也在事务内做——单连接池下事务内
// 不得再经 d.db 取连接。
func (m *Manager) MoveGroup(id int64, delta int) error {
	var moved bool
	var movedName, movedKind string
	err := m.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		all, err := m.DB.ListRoutingGroupsTx(tx)
		if err != nil {
			return err
		}
		target := findGroupByID(all, id)
		if target == nil {
			return storage.ErrNotFound
		}
		members := layerMembers(all, target.Kind, 0)
		idx := indexOfGroup(members, id)
		if idx < 0 {
			return storage.ErrNotFound
		}
		next := idx + delta
		if next < 0 || next >= len(members) {
			return nil
		}
		members[idx], members[next] = members[next], members[idx]
		if err := m.applyLayerOrderTx(tx, target.Kind, members); err != nil {
			return err
		}
		moved, movedName, movedKind = true, target.Name, config.KindLabel(target.Kind)
		return nil
	})
	if err != nil {
		return err
	}
	if moved {
		m.Logf("分流组 %q 在 %s 层内顺序已调整", movedName, movedKind)
	}
	return nil
}

// MoveGroupToKind 跨层移动：把分流组移到目标层，pos < 0 或越界时追加到层尾。
// 跨层会改变该组的优先级归属，故由调用方（TUI/CLI）显式触发并提示用户。
func (m *Manager) MoveGroupToKind(id int64, kind string, pos int) error {
	kind = config.KindNormalize(kind)
	if !config.KindValid(kind) {
		return validation.New("kind", "未知分流层 %q（可选: %s）", kind, strings.Join(config.Kinds, " / "))
	}
	all, err := m.DB.ListRoutingGroups()
	if err != nil {
		return err
	}
	g := findGroupByID(all, id)
	if g == nil {
		return storage.ErrNotFound
	}
	source := config.KindNormalize(g.Kind)
	g.Kind = kind
	if err := m.validate(g, g.ID); err != nil {
		return err
	}
	if source == kind && pos < 0 {
		return nil
	}
	return m.relocate(g, kind, pos)
}

// relocate 把 g 放进 kind 层的 pos 位置（pos < 0 表示层尾），并重排两层的序号。
// 整行更新 + 目标层重排 + 源层去空档在单个事务中完成（C14）：
// 中途失败整体回滚，不留半更新状态。
func (m *Manager) relocate(g *config.RoutingGroup, kind string, pos int) error {
	if err := m.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		return m.relocateTx(tx, g, kind, pos)
	}); err != nil {
		return err
	}
	m.Logf("分流组 %q 已移入 %s 层（第 %d 位）", g.Name, config.KindLabel(kind), g.Position+1)
	return nil
}

// relocateTx 是 relocate 的事务内版本（C14）：必须经 WithTx 提供的事务执行，
// 读取与写入都走传入的 tx（单连接池下事务内不得再经 d.db 取连接）。
// 事务内只做 SQL 与内存计算。
func (m *Manager) relocateTx(tx *sql.Tx, g *config.RoutingGroup, kind string, pos int) error {
	all, err := m.DB.ListRoutingGroupsTx(tx)
	if err != nil {
		return err
	}
	source := ""
	for _, candidate := range all {
		if candidate.ID == g.ID {
			source = config.KindNormalize(candidate.Kind)
			break
		}
	}
	members := layerMembers(all, kind, g.ID)
	if pos < 0 || pos > len(members) {
		pos = len(members)
	}
	ordered := make([]*config.RoutingGroup, 0, len(members)+1)
	ordered = append(ordered, members[:pos]...)
	ordered = append(ordered, g)
	ordered = append(ordered, members[pos:]...)

	g.Kind = kind
	g.Position = pos
	if err := m.DB.UpdateRoutingGroupTx(tx, g); err != nil {
		return err
	}
	if err := m.applyLayerOrderTx(tx, kind, ordered); err != nil {
		return err
	}
	if source != "" && source != kind {
		// 源层被抽走一个成员：重排去掉空档
		if err := m.applyLayerOrderTx(tx, source, layerMembers(all, source, g.ID)); err != nil {
			return err
		}
	}
	return nil
}

// applyLayerOrderTx 按给定顺序把 kind 层的 position 重排为连续值 0..n-1，
// 只更新定位列（kind/kind_rank/position），不触碰规则（C14：必须在 WithTx
// 提供的事务内执行）。afterPlacement 测试注入缝同样在事务内生效——注入错误
// 会让整个外层事务回滚。
func (m *Manager) applyLayerOrderTx(tx *sql.Tx, kind string, ordered []*config.RoutingGroup) error {
	n := 0
	for i, g := range ordered {
		if g == nil {
			continue
		}
		if config.KindNormalize(g.Kind) == kind && g.Position == i {
			continue
		}
		if err := m.DB.SetRoutingGroupPlacementTx(tx, g.ID, kind, i); err != nil {
			return err
		}
		g.Kind = kind
		g.Position = i
		n++
		if m.afterPlacement != nil {
			if err := m.afterPlacement(n); err != nil {
				return err
			}
		}
	}
	return nil
}

// layerMembers 取 kind 层的成员（按层内顺序），skipID 非 0 时排除该组。
func layerMembers(groups []*config.RoutingGroup, kind string, skipID int64) []*config.RoutingGroup {
	out := make([]*config.RoutingGroup, 0, len(groups))
	for _, g := range groups {
		if g == nil || g.ID == skipID {
			continue
		}
		if config.KindNormalize(g.Kind) == kind {
			out = append(out, g)
		}
	}
	return out
}

func findGroupByID(groups []*config.RoutingGroup, id int64) *config.RoutingGroup {
	for _, g := range groups {
		if g != nil && g.ID == id {
			return g
		}
	}
	return nil
}

func indexOfGroup(groups []*config.RoutingGroup, id int64) int {
	for i, g := range groups {
		if g != nil && g.ID == id {
			return i
		}
	}
	return -1
}

// DeleteGroup 删除分流组（规则级联删除），并把该层剩余成员的层内序号去空档。
// 删除与重排在单个事务中完成（C14-AUDIT）：中途失败整体回滚，不会留下
// 组已删、序号未去空档的半状态。
func (m *Manager) DeleteGroup(id int64) error {
	var name string
	err := m.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		all, err := m.DB.ListRoutingGroupsTx(tx)
		if err != nil {
			return err
		}
		g := findGroupByID(all, id)
		if g == nil {
			return storage.ErrNotFound
		}
		name = g.Name
		kind := config.KindNormalize(g.Kind)
		if m.writeProbe != nil {
			if err := m.writeProbe("DeleteRoutingGroup"); err != nil {
				return err
			}
		}
		if err := m.DB.DeleteRoutingGroupTx(tx, id); err != nil {
			return err
		}
		if m.writeProbe != nil {
			if err := m.writeProbe("applyLayerOrder"); err != nil {
				return err
			}
		}
		// all 是删除前快照，重排时跳过被删组本身
		return m.applyLayerOrderTx(tx, kind, layerMembers(all, kind, id))
	})
	if err != nil {
		return err
	}
	m.Logf("删除分流组 %q", name)
	return nil
}

// SetEnabled 启用/停用分流组。
func (m *Manager) SetEnabled(id int64, enabled bool) error {
	g, err := m.getGroup(id)
	if err != nil {
		return err
	}
	g.Enabled = enabled
	if err := m.validate(g, g.ID); err != nil {
		return err
	}
	if err := m.DB.UpdateRoutingGroup(g); err != nil {
		return err
	}
	state := "启用"
	if !enabled {
		state = "停用"
	}
	m.Logf("分流组 %q 已%s", g.Name, state)
	return nil
}

// MoveRule 在组内移动规则位置（delta: -1 上移 / +1 下移）；顺序即优先级。
func (m *Manager) MoveRule(groupID int64, ruleID int64, delta int) error {
	g, err := m.getGroup(groupID)
	if err != nil {
		return err
	}
	idx := -1
	for i := range g.Rules {
		if g.Rules[i].ID == ruleID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("规则不存在")
	}
	next := idx + delta
	if next < 0 || next >= len(g.Rules) {
		return nil // 已在边界，无需移动
	}
	g.Rules[idx], g.Rules[next] = g.Rules[next], g.Rules[idx]
	if err := m.DB.UpdateRoutingGroup(g); err != nil {
		return err
	}
	m.Logf("分流组 %q 规则顺序已调整", g.Name)
	return nil
}

// validate 校验分流组：名称唯一、目标合法、层与 final 结构合法、规则合法。
//
// final 的结构性约束（checklist 6.3，替代原先「final 必须是组内唯一规则」的模糊表述）：
//   - kind='final' 的组必须且仅有一条 final 规则；
//   - kind≠'final' 的组不得含 final 规则；
//   - 全库 kind='final' 的组最多一个。
//
// final 组允许停用：停用即「无活动 final」，生成器回退 route.final=direct，
// 未匹配流量会**直连而非走代理**（提示文案由界面负责）。
func (m *Manager) validate(g *config.RoutingGroup, selfID int64) error {
	if g.Name == "" {
		return validation.New("name", "分流组名称不能为空")
	}
	if g.Target == "" {
		return validation.New("target", "分流目标不能为空")
	}
	if !config.KindValid(g.Kind) {
		return validation.New("kind", "未知分流层 %q（可选: %s）", g.Kind, strings.Join(config.Kinds, " / "))
	}
	g.Kind = config.KindNormalize(g.Kind)

	// 名称唯一
	existing, err := m.DB.ListRoutingGroups()
	if err != nil {
		return err
	}
	for _, e := range existing {
		if e.ID != selfID && e.Name == g.Name {
			return validation.New("name", "分流组名称 %q 已存在", g.Name)
		}
	}

	finalRules := 0
	for i := range g.Rules {
		if g.Rules[i].Type == "final" {
			finalRules++
		}
	}
	if g.Kind == config.KindFinal {
		if finalRules != 1 || len(g.Rules) != 1 {
			return validation.New("type",
				"kind=final 的组必须且仅有一条 final 规则；当前组 %q 有 %d 条规则（其中 final %d 条）",
				g.Name, len(g.Rules), finalRules)
		}
		for _, e := range existing {
			if e.ID != selfID && config.KindNormalize(e.Kind) == config.KindFinal {
				return validation.New("type",
					"final 层最多只能有一个分流组（已有 %q）；请把 %q 改到其他层",
					e.Name, g.Name)
			}
		}
	} else if finalRules > 0 {
		return validation.New("type",
			"组 %q 含 final 规则但不在 final 层；final 规则只能出现在 kind=final 的组里（把该组移到 final 层，或删除这条规则）",
			g.Name)
	}

	// 目标：DIRECT/BLOCK 或代理组名
	if g.Target != "DIRECT" && g.Target != "BLOCK" {
		groups, err := m.DB.ListProxyGroups()
		if err != nil {
			return err
		}
		found := false
		for _, pg := range groups {
			if pg.Name == g.Target {
				found = true
				break
			}
		}
		if !found {
			return validation.New("target", "目标 %q 不是 DIRECT/BLOCK，也不是已存在的代理组", g.Target)
		}
	}
	// 规则合法性
	for i := range g.Rules {
		r := &g.Rules[i]
		if !ruleTypes[r.Type] {
			return validation.New("type", "未知规则类型 %q", r.Type)
		}
		if r.Type == "final" {
			continue // 结构约束已在上方按 Kind 校验
		}
		if r.Type == "logical" {
			if err := m.validateLogical(r); err != nil {
				return fmt.Errorf("分流组 %q: %w", g.Name, err)
			}
			continue
		}
		if strings.TrimSpace(r.Value) == "" {
			return validation.New("value", "规则 %s 的值不能为空", r.Type)
		}
		if r.Type == "domain_regex" {
			if err := checkRegex(r.Value); err != nil {
				return err
			}
		}
		if r.Type == "rule_set" {
			if err := m.checkRuleSetTags(r.Value); err != nil {
				return err
			}
		}
	}
	return nil
}

// activeFinalGroup 返回当前库中含活动 final 规则的组；没有则返回 nil。
// 生成器（config.generate）对 final 的语义是「全局唯一兜底」，这里用于状态提示。
func (m *Manager) activeFinalGroup() (*config.RoutingGroup, error) {
	groups, err := m.DB.ListRoutingGroups()
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if !g.Enabled {
			continue
		}
		for _, r := range g.Rules {
			if r.Enabled && r.Type == "final" {
				return g, nil
			}
		}
	}
	return nil, nil
}

// validateLogical 校验逻辑规则的组合方式与子条件。
func (m *Manager) validateLogical(r *config.Rule) error {
	if r.Mode != "and" && r.Mode != "or" {
		return validation.New("type", "逻辑规则组合方式须为 and 或 or，当前 %q", r.Mode)
	}
	if len(r.Conditions) == 0 {
		return validation.New("value", "逻辑规则至少需要一个子条件")
	}
	for _, c := range r.Conditions {
		if !config.CondTypeValid(c.Type) {
			return validation.New("type", "未知逻辑条件类型 %q", c.Type)
		}
		if strings.TrimSpace(c.Value) == "" {
			return validation.New("value", "逻辑条件 %s 的值不能为空", c.Type)
		}
		if c.Type == "domain_regex" {
			if err := checkRegex(c.Value); err != nil {
				return err
			}
		}
		if c.Type == "rule_set" {
			if err := m.checkRuleSetTags(c.Value); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateCondition uses the same checks as saving a logical routing rule.
func (m *Manager) ValidateCondition(condition config.RuleCondition) error {
	return m.validateLogical(&config.Rule{Type: "logical", Mode: "and", Conditions: []config.RuleCondition{condition}})
}

// checkRegex 校验 domain_regex 的值可编译；非法正则会让 sing-box 启动失败，
// 在写入前就拒绝。Go 的 regexp 与 sing-box 同为 RE2 语法，校验结果一致。
func checkRegex(value string) error {
	if _, err := regexp.Compile(strings.TrimSpace(value)); err != nil {
		// 用 %s 而非 %q：正则里的反斜杠会被 %q 二次转义，难以对照
		return validation.New("value", "正则表达式 %s 非法: %w", value, err)
	}
	return nil
}

// checkRuleSetTags 校验 rule_set 规则引用的规则集存在（多值逗号分隔）。
// 值可以是 rulesets 表里的自定义规则集 tag，也可以是内置分类引用
// （"geosite:cn" 或派生 tag "geosite-cn"，见 internal/catalog）——后者无需预先添加。
func (m *Manager) checkRuleSetTags(value string) error {
	sets, err := m.DB.ListRuleSets()
	if err != nil {
		return err
	}
	tags := map[string]bool{}
	for _, rs := range sets {
		tags[rs.Tag] = true
	}
	for _, v := range strings.Split(value, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if tags[v] || catalog.IsRef(v) {
			continue
		}
		if kind, code, found := strings.Cut(v, ":"); found && catalog.KindValid(strings.ToLower(kind)) {
			return validation.New("value", "内置分类 %q 中没有 %q 这个分类码", strings.ToLower(kind), code)
		}
		return validation.New("value", "规则集 %q 不存在：请在规则集管理中添加，或使用内置分类（如 geosite:cn / geoip:jp / acl:ChinaDomain）", v)
	}
	return nil
}

func (m *Manager) getGroup(id int64) (*config.RoutingGroup, error) {
	list, err := m.DB.ListRoutingGroups()
	if err != nil {
		return nil, err
	}
	for _, g := range list {
		if g.ID == id {
			return g, nil
		}
	}
	return nil, storage.ErrNotFound
}
