// 地区分流预置方案（对照 karing 的 karing/assets/datas/preset/<region>.json）。
//
// 承载方式与 internal/catalog/data/ 一致：预置文件随二进制嵌入（preset/ 目录，
// 由 scripts/sync-preset.sh 从参考目录同步，不手工抄写 28 条），解析在编译期
// 完成之后按需读取。将来加 ir/ru 只需把文件放进 preset/ 并登记到 PresetRegions。
//
// karing 侧的加载点是 DiversionCustomRulesPreset.getPreset(regionCode)，调用处为
// 新手向导（home_screen.dart）与「自定义分流组 → 预设」入口；加载器实现不在公开仓库，
// 故「region → 文件、无匹配回落 default」是按资源布局推断的约定。
package routing

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/catalog"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

//go:embed preset/*.json
var presetFS embed.FS

// PresetCN 中国大陆地区预置（主用地区，见 checklist Phase 7）。
const PresetCN = "cn"

// PresetRegions 已嵌入的地区预置，按展示顺序排列。
// 结构上按「可容纳多地区」设计：新增地区只需同步文件并登记在这里。
var PresetRegions = []string{PresetCN}

// PresetFinalGroup 地区预置的兜底组名；Kind='final'，目标为 Manual（select 手动选择）。
// 与代理类目标分开配置：karing 的 i18n 里 outboundRuleMode.currentSelected（当前选中）
// 与 outboundRuleMode.urltest（自动选择）是两个不同语义，cn.json 用的是前者；
// 本项目把代理类目标统一映射到 Auto（urltest），把 Manual（select）留给 final 兜底。
const PresetFinalGroup = "Final"

// karing 预置条目的 outbound 取值。
const (
	presetOutboundDirect  = "direct"
	presetOutboundBlock   = "block"
	presetOutboundCurrent = "currentSelected"
)

// presetTargets 预置目标 → 本项目出站/动作，集中为一处常量表。
// 代理类目标一律 Auto；**final 目标单独定**（PresetFinalGroup 用 Manual），两者不共用。
var presetTargets = map[string]string{
	presetOutboundDirect:  "DIRECT",
	presetOutboundBlock:   "BLOCK",
	presetOutboundCurrent: storage.DefaultGroupAuto,
}

// presetRule 是预置文件里的一条。
//
// 只取 rule_set_build_in：`package` / `processName` 字段**不实现**（桌面 TUI 无按
// 应用/按进程分流，4.2 与 4.5 已有范围决定）；`domain_suffix` / `domain_keyword` /
// `ip_cidr` 同理不实现——预置里只有「📢 苹果推送通知」用到它们，而该条没有
// rule_set_build_in，按「跳过无规则条目」处理（28 条 → 27 组，见 checklist 7.1/7.2）。
type presetRule struct {
	Name           string   `json:"name"`
	Outbound       string   `json:"outbound"`
	Switch         bool     `json:"switch"`
	RuleSetBuildIn []string `json:"rule_set_build_in"`
}

type presetFile struct {
	Rules []presetRule `json:"rules"`
}

// PresetEntry 一条预置的映射结果。Skipped 非空表示该条不建组，原因写在里面。
type PresetEntry struct {
	Group     *config.RoutingGroup // Skipped 非空时为 nil
	Index     int                  // 预置文件中的下标（1 基，便于对照 karing）
	Name      string
	Skipped   string   // 跳过原因
	Notes     []string // 逐条偏差（如剔除悬空引用）
	Original  []string // 原始 rule_set_build_in（核对用）
	SourceOfs string   // 目标来源（direct/block/currentSelected）
}

// LoadPreset 解析地区预置为分流组映射结果。
//
// 1:1 映射规则（checklist 7.1）：
//   - rule_set_build_in → **一条** rule_set 规则（值以逗号分隔；与多条 rule_set 规则语义等价）
//   - outbound → Target、switch → Enabled、列表下标 → **层内 Position**
//   - Kind 一律 custom（依据见 config/routing_kind.go 的层序说明）
//
// 无法解析的分类引用（如 geoip:bing 这个真悬空引用）会被剔除并记入 Notes，
// 不让单条悬空码使整条预置失败。
func LoadPreset(region string) ([]PresetEntry, error) {
	region = strings.TrimSpace(region)
	if region == "" {
		region = PresetCN
	}
	raw, err := presetFS.ReadFile("preset/" + region + ".json")
	if err != nil {
		return nil, fmt.Errorf("未嵌入地区预置 %q（可选: %s）", region, strings.Join(PresetRegions, " / "))
	}
	var file presetFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("解析地区预置 %s 失败: %w", region, err)
	}
	if len(file.Rules) == 0 {
		return nil, fmt.Errorf("地区预置 %s 没有任何条目", region)
	}

	entries := make([]PresetEntry, 0, len(file.Rules))
	position := 0
	for i, rule := range file.Rules {
		entry := PresetEntry{
			Index:     i + 1,
			Name:      rule.Name,
			Original:  append([]string(nil), rule.RuleSetBuildIn...),
			SourceOfs: rule.Outbound,
		}
		if len(rule.RuleSetBuildIn) == 0 {
			// 空规则组能通过 routing.validate（规则校验是逐条循环），但生成不出任何
			// 路由规则，属静默空转 —— 跳过并在报告里列出，不建组。
			entry.Skipped = "无内置规则集（原条目仅依赖应用包名/进程名或非规则集条件）"
			entries = append(entries, entry)
			continue
		}
		target, ok := presetTargets[strings.TrimSpace(rule.Outbound)]
		if !ok {
			return nil, fmt.Errorf("预置条目 %q 的目标 %q 未映射（支持: %s）",
				rule.Name, rule.Outbound, strings.Join(presetOutboundValues(), " / "))
		}
		refs, notes := resolvableRefs(rule.RuleSetBuildIn)
		entry.Notes = notes
		if len(refs) == 0 {
			entry.Skipped = "全部规则集引用都无法解析"
			entries = append(entries, entry)
			continue
		}
		entry.Group = &config.RoutingGroup{
			Name:     rule.Name,
			Target:   target,
			Kind:     config.KindCustom,
			Position: position,
			Enabled:  rule.Switch,
			Rules: []config.Rule{{
				Type:    "rule_set",
				Value:   strings.Join(refs, ","),
				Enabled: true,
			}},
		}
		position++
		entries = append(entries, entry)
	}
	return entries, nil
}

// resolvableRefs 过滤出可被分类库解析的引用，并按原顺序去重。
// 顺带把「剔除」这一偏差记录下来——不得静默丢弃。
func resolvableRefs(values []string) ([]string, []string) {
	out := make([]string, 0, len(values))
	var notes []string
	seen := map[string]bool{}
	for _, value := range values {
		v := strings.TrimSpace(value)
		if v == "" {
			continue
		}
		if !catalog.IsRef(v) {
			notes = append(notes, fmt.Sprintf("已剔除无法解析的引用 %s（内置分类库中没有该码）", v))
			continue
		}
		// 归一为统一写法（预置里混用 geosite:apple 与 apple@ads 这类简写）
		ref, _ := catalog.Parse(v)
		canonical := ref.String()
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	return out, notes
}

func presetOutboundValues() []string {
	out := make([]string, 0, len(presetTargets))
	for key := range presetTargets {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// PresetMode 预置对齐方式。
type PresetMode int

const (
	// PresetMerge 按名称跳过已存在的组，可重复执行（也是「恢复默认预设」的语义）。
	PresetMerge PresetMode = iota
	// PresetReplace 先删除现有全部分流组再写入，需二次确认。
	PresetReplace
)

func (m PresetMode) String() string {
	if m == PresetReplace {
		return "replace"
	}
	return "merge"
}

// ParsePresetMode 解析命令行传入的对齐方式。
func ParsePresetMode(value string) (PresetMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "merge":
		return PresetMerge, nil
	case "replace":
		return PresetReplace, nil
	default:
		return PresetMerge, fmt.Errorf("未知对齐方式 %q（可选 merge / replace）", value)
	}
}

// PresetItem 逐条执行结果，与界面任务回执的口径一致（新增 / 跳过 / 失败 / 偏差）。
type PresetItem struct {
	Name   string
	Action string
	Detail string
}

// PresetReport 一次预置对齐的完整结果。
type PresetReport struct {
	Region  string
	Mode    PresetMode
	Items   []PresetItem
	Added   int
	Skipped int
	Failed  int
}

// Summary 返回一句话汇总。
func (r *PresetReport) Summary() string {
	return fmt.Sprintf("地区预置 %s（%s）：新增 %d · 跳过 %d · 失败 %d",
		r.Region, r.Mode, r.Added, r.Skipped, r.Failed)
}

// FailedItems 返回失败项的说明，供界面提示可执行的操作。
func (r *PresetReport) FailedItems() []string {
	var out []string
	for _, item := range r.Items {
		if item.Action == "失败" {
			out = append(out, fmt.Sprintf("%s: %s", item.Name, item.Detail))
		}
	}
	return out
}

// ApplyPreset 把地区预置写入当前库。
//
// merge：按名称跳过已存在的组，可重复执行——契约即逐条增量，单条失败
// 记入报告并继续，不做事务包裹（这是有意的，见 applyPresetReplaceTx 注释）。
// replace：先删除现有全部分流组再写入——用户视角是一次「全量替换」操作，
// 必须全成或全败，因此在单个事务中完成（C14-AUDIT）。
// 两种模式都会在写入前确认 Auto / Manual 代理组存在（缺失就地补建），
// 并保证全库最多一个 kind='final' 的兜底组。
//
// 已装用户不会被自动改写：本函数只由显式的对齐入口调用（CLI route preset / TUI 入口）。
func (m *Manager) ApplyPreset(region string, mode PresetMode) (*PresetReport, error) {
	entries, err := LoadPreset(region)
	if err != nil {
		return nil, err
	}
	report := &PresetReport{Region: strings.TrimSpace(region), Mode: mode}
	if report.Region == "" {
		report.Region = PresetCN
	}

	// 目标必须先存在，否则写入的分流组会指向不存在的出站
	if note, err := m.ensurePresetProxyGroups(); err != nil {
		return nil, err
	} else if note != "" {
		report.Items = append(report.Items, PresetItem{Name: "代理组", Action: "补齐", Detail: note})
	}

	existing, err := m.DB.ListRoutingGroups()
	if err != nil {
		return nil, err
	}
	if mode == PresetReplace {
		if err := m.applyPresetReplaceTx(entries, report); err != nil {
			return nil, err
		}
		m.Logf("%s", report.Summary())
		return report, nil
	}

	byName := make(map[string]bool, len(existing))
	hasFinal := false
	for _, g := range existing {
		byName[g.Name] = true
		if config.KindNormalize(g.Kind) == config.KindFinal {
			hasFinal = true
		}
	}

	for _, entry := range entries {
		for _, note := range entry.Notes {
			report.Items = append(report.Items, PresetItem{Name: entry.Name, Action: "偏差", Detail: note})
		}
		if entry.Skipped != "" {
			report.Skipped++
			report.Items = append(report.Items, PresetItem{Name: entry.Name, Action: "跳过", Detail: entry.Skipped})
			continue
		}
		if byName[entry.Group.Name] {
			report.Skipped++
			report.Items = append(report.Items, PresetItem{
				Name: entry.Group.Name, Action: "跳过", Detail: "同名分流组已存在（merge 模式不改写）",
			})
			continue
		}
		if err := m.DB.CreateRoutingGroup(entry.Group); err != nil {
			report.Failed++
			report.Items = append(report.Items, PresetItem{Name: entry.Group.Name, Action: "失败", Detail: err.Error()})
			continue
		}
		byName[entry.Group.Name] = true
		report.Added++
		detail := fmt.Sprintf("→ %s · %s 层 · 层内第 %d 位 · %s",
			entry.Group.Target, config.KindLabel(entry.Group.Kind), entry.Group.Position+1, enabledLabel(entry.Group.Enabled))
		report.Items = append(report.Items, PresetItem{Name: entry.Group.Name, Action: "新增", Detail: detail})
	}

	// 兜底组：kind=final 必须且仅有一条 final 规则
	switch {
	case hasFinal:
		report.Items = append(report.Items, PresetItem{
			Name: PresetFinalGroup, Action: "跳过", Detail: "已有 final 层兜底组（不改写其目标）",
		})
	case byName[PresetFinalGroup]:
		report.Skipped++
		report.Items = append(report.Items, PresetItem{
			Name: PresetFinalGroup, Action: "跳过", Detail: "同名分流组已存在但不是 final 层",
		})
	default:
		final := &config.RoutingGroup{
			Name: PresetFinalGroup, Target: storage.DefaultGroupManual, Kind: config.KindFinal,
			Position: 0, Enabled: true,
			Rules: []config.Rule{{Type: "final", Enabled: true}},
		}
		if err := m.DB.CreateRoutingGroup(final); err != nil {
			report.Failed++
			report.Items = append(report.Items, PresetItem{Name: final.Name, Action: "失败", Detail: err.Error()})
			break
		}
		report.Added++
		report.Items = append(report.Items, PresetItem{
			Name: final.Name, Action: "新增",
			Detail: fmt.Sprintf("→ %s · final 层（兜底）· 未在 %s 中选择节点时走成员列表首个节点",
				final.Target, final.Target),
		})
	}

	m.Logf("%s", report.Summary())
	return report, nil
}

// applyPresetReplaceTx 在单个事务中完成 replace 模式：删除现有全部分流组 →
// 逐条写入预置组 → 建 final 兜底组。任一步失败整体回滚——replace 是一次
// 带二次确认的破坏性操作，绝不留下「删了一半、写了一半」的状态
// （原实现中途失败会丢光用户分组且只写入部分预置，C14-AUDIT 修复）。
// merge 模式不事务化：其契约就是逐条、可重复、单条失败记入报告。
func (m *Manager) applyPresetReplaceTx(entries []PresetEntry, report *PresetReport) error {
	return m.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		existing, err := m.DB.ListRoutingGroupsTx(tx)
		if err != nil {
			return err
		}
		for _, g := range existing {
			if m.writeProbe != nil {
				if err := m.writeProbe("DeleteRoutingGroup"); err != nil {
					return err
				}
			}
			if err := m.DB.DeleteRoutingGroupTx(tx, g.ID); err != nil {
				return fmt.Errorf("删除现有分流组 %q 失败: %w", g.Name, err)
			}
		}
		report.Items = append(report.Items, PresetItem{
			Name: "现有分流组", Action: "删除",
			Detail: fmt.Sprintf("已删除 %d 个（replace 模式）", len(existing)),
		})

		byName := map[string]bool{}
		for _, entry := range entries {
			for _, note := range entry.Notes {
				report.Items = append(report.Items, PresetItem{Name: entry.Name, Action: "偏差", Detail: note})
			}
			if entry.Skipped != "" {
				report.Skipped++
				report.Items = append(report.Items, PresetItem{Name: entry.Name, Action: "跳过", Detail: entry.Skipped})
				continue
			}
			if byName[entry.Group.Name] {
				// 预置文件内部同名：跳过而不是让 UNIQUE 冲突炸掉整个事务
				report.Skipped++
				report.Items = append(report.Items, PresetItem{
					Name: entry.Group.Name, Action: "跳过", Detail: "预置内同名条目重复，仅写入首个",
				})
				continue
			}
			if m.writeProbe != nil {
				if err := m.writeProbe("CreateRoutingGroup"); err != nil {
					return err
				}
			}
			if err := m.DB.CreateRoutingGroupTx(tx, entry.Group); err != nil {
				return fmt.Errorf("写入分流组 %q 失败: %w", entry.Group.Name, err)
			}
			byName[entry.Group.Name] = true
			report.Added++
			detail := fmt.Sprintf("→ %s · %s 层 · 层内第 %d 位 · %s",
				entry.Group.Target, config.KindLabel(entry.Group.Kind), entry.Group.Position+1, enabledLabel(entry.Group.Enabled))
			report.Items = append(report.Items, PresetItem{Name: entry.Group.Name, Action: "新增", Detail: detail})
		}

		// 兜底组：replace 删光了现有组，final 层必然为空（除非预置里恰有同名组）
		if byName[PresetFinalGroup] {
			report.Skipped++
			report.Items = append(report.Items, PresetItem{
				Name: PresetFinalGroup, Action: "跳过", Detail: "同名分流组已存在但不是 final 层",
			})
			return nil
		}
		final := &config.RoutingGroup{
			Name: PresetFinalGroup, Target: storage.DefaultGroupManual, Kind: config.KindFinal,
			Position: 0, Enabled: true,
			Rules: []config.Rule{{Type: "final", Enabled: true}},
		}
		if m.writeProbe != nil {
			if err := m.writeProbe("CreateRoutingGroup"); err != nil {
				return err
			}
		}
		if err := m.DB.CreateRoutingGroupTx(tx, final); err != nil {
			return fmt.Errorf("写入兜底组 %q 失败: %w", final.Name, err)
		}
		report.Added++
		report.Items = append(report.Items, PresetItem{
			Name: final.Name, Action: "新增",
			Detail: fmt.Sprintf("→ %s · final 层（兜底）· 未在 %s 中选择节点时走成员列表首个节点",
				final.Target, final.Target),
		})
		return nil
	})
}

// ensurePresetProxyGroups 确保预置用到的默认代理组（Auto / Manual）存在。
// 用户可能删掉过 Manual，此时不能静默写入一个无效 target；就地补建并如实报告。
func (m *Manager) ensurePresetProxyGroups() (string, error) {
	groups, err := m.DB.ListProxyGroups()
	if err != nil {
		return "", err
	}
	names := make(map[string]bool, len(groups))
	for _, g := range groups {
		names[g.Name] = true
	}
	var created []string
	if !names[storage.DefaultGroupAuto] {
		if err := m.DB.CreateProxyGroup(&config.ProxyGroup{
			Name: storage.DefaultGroupAuto, Type: "urltest", TestURL: config.DefaultTestURL,
			IntervalS: 300, Members: []config.ProxyGroupMember{{Type: "all"}},
		}); err != nil {
			return "", fmt.Errorf("补建默认代理组 %s 失败: %w", storage.DefaultGroupAuto, err)
		}
		created = append(created, storage.DefaultGroupAuto)
	}
	if !names[storage.DefaultGroupManual] {
		if err := m.DB.CreateProxyGroup(&config.ProxyGroup{
			Name: storage.DefaultGroupManual, Type: "select",
			Members: []config.ProxyGroupMember{{Type: "all"}},
		}); err != nil {
			return "", fmt.Errorf("补建默认代理组 %s 失败: %w", storage.DefaultGroupManual, err)
		}
		created = append(created, storage.DefaultGroupManual)
	}
	if len(created) == 0 {
		return "", nil
	}
	return "补建缺失的代理组: " + strings.Join(created, "、"), nil
}

// enabledLabel 把启用状态转为展示文案。
func enabledLabel(enabled bool) string {
	if enabled {
		return "启用"
	}
	return "停用"
}
