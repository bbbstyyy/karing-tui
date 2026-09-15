// Package routing 实现分流组管理：分流组 CRUD、规则校验与排序、
// 默认分流方案初始化。
package routing

import (
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

// CreateGroup 新建分流组（含规则）。
func (m *Manager) CreateGroup(name, target string, rules []config.Rule) (*config.RoutingGroup, error) {
	g := &config.RoutingGroup{
		Name:     strings.TrimSpace(name),
		Target:   strings.TrimSpace(target),
		Enabled:  true,
		Position: int(^uint(0) >> 1), // 新组排到最后
		Rules:    rules,
	}
	if err := m.validate(g, 0); err != nil {
		return nil, err
	}
	// position 取现有最大值 +1
	existing, err := m.DB.ListRoutingGroups()
	if err != nil {
		return nil, err
	}
	max := -1
	for _, e := range existing {
		if e.Position > max {
			max = e.Position
		}
	}
	g.Position = max + 1
	if err := m.DB.CreateRoutingGroup(g); err != nil {
		return nil, err
	}
	m.Logf("创建分流组 %q → %s（%d 条规则）", g.Name, g.Target, len(rules))
	return g, nil
}

// UpdateGroup 更新分流组（名称/目标/规则整体替换）。
func (m *Manager) UpdateGroup(g *config.RoutingGroup, rules []config.Rule) error {
	g.Name = strings.TrimSpace(g.Name)
	g.Target = strings.TrimSpace(g.Target)
	g.Rules = rules
	if err := m.validate(g, g.ID); err != nil {
		return err
	}
	if err := m.DB.UpdateRoutingGroup(g); err != nil {
		return err
	}
	m.Logf("修改分流组 %q → %s", g.Name, g.Target)
	return nil
}

// DeleteGroup 删除分流组（规则级联删除）。
func (m *Manager) DeleteGroup(id int64) error {
	g, err := m.getGroup(id)
	if err != nil {
		return err
	}
	if err := m.DB.DeleteRoutingGroup(id); err != nil {
		return err
	}
	m.Logf("删除分流组 %q", g.Name)
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

// validate 校验分流组：名称唯一、目标合法、规则合法。
func (m *Manager) validate(g *config.RoutingGroup, selfID int64) error {
	if g.Name == "" {
		return validation.New("name", "分流组名称不能为空")
	}
	if g.Target == "" {
		return validation.New("target", "分流目标不能为空")
	}
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
	if g.Enabled && hasActiveFinal(g) {
		for _, e := range existing {
			if e.ID != selfID && e.Enabled && hasActiveFinal(e) {
				return validation.New("type", "活动 final 分流组只能有一个（已有分流组 %q）", e.Name)
			}
		}
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
			if len(g.Rules) != 1 || i != len(g.Rules)-1 {
				return validation.New("type", "final 规则必须是组内唯一规则")
			}
			continue
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

func hasActiveFinal(g *config.RoutingGroup) bool {
	if g == nil || !g.Enabled {
		return false
	}
	for _, r := range g.Rules {
		if r.Enabled && r.Type == "final" {
			return true
		}
	}
	return false
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
