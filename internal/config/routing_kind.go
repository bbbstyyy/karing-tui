package config

import (
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/catalog"
)

// 分流层（Kind）——移植 karing 的「层序」模型。
//
// karing 侧证据：`server_manager.dart` 的 `diversionGroupSorted()` 以硬编码的调用顺序
// 拼接分组 —— customGroups → geositeGroups → geoipGroups → aclGroups → finalGroups，
// 层内再由 sortCompare 按 groupIndex 排序。层 ID 即字面量常量
// （"custom" / "geosite" / "geoip" / "acl" / "final"）。
//
// **引入层序是为了给「手工规则」与「分类库规则」分层，不是为了重新归类预置。**
// 预置方案（如 cn.json 的 27 组）里的组往往是混合种类，一律归 `custom` 层：
// 若按「首个规则集种类」把它们散到 geosite/geoip/acl 层，就会破坏预置原本的
// 扁平优先级（详见 checklist 6.4 的反例）。请勿「顺手优化」这条推论。
const (
	KindCustom  = "custom"  // 用户显式配置的精细规则（含全部预置方案）
	KindGeosite = "geosite" // 按域名分类库勾选生成的组
	KindGeoIP   = "geoip"   // 按 IP 分类库勾选生成的组
	KindACL     = "acl"     // 按 ACL 分类库勾选生成的组
	KindFinal   = "final"   // 兜底组，固定置底、最多一个
)

// Kinds 按层序（优先级从高到低）列出全部层，也是界面上的展示顺序。
var Kinds = []string{KindCustom, KindGeosite, KindGeoIP, KindACL, KindFinal}

// kindRanks 层序号，层序常量与层序号的唯一事实源；SQLite 侧的冗余列
// routing_groups.kind_rank 由 KindRank 计算后落库，排序键为 (kind_rank, position, id)。
var kindRanks = map[string]int{
	KindCustom:  0,
	KindGeosite: 1,
	KindGeoIP:   2,
	KindACL:     3,
	KindFinal:   4,
}

// kindLabels 层标题文案，对齐 karing 的 i18n：
// meta.diversionCustomGroup=自定义分流组、meta.rulesetGeoSite=GeoSite、
// meta.rulesetGeoIp=GeoIP、meta.rulesetAcl=ACL、routeFinal=final。
var kindLabels = map[string]string{
	KindCustom:  "自定义分流组",
	KindGeosite: "GeoSite",
	KindGeoIP:   "GeoIP",
	KindACL:     "ACL",
	KindFinal:   "final",
}

// KindNormalize 把空值与非法值统一为 custom：老数据（迁移前）与调用方漏填时
// 落到层序最高的 custom 层最安全——它不会把手工规则挤到分类库规则之后。
func KindNormalize(kind string) string {
	k := strings.TrimSpace(strings.ToLower(kind))
	if k == "" {
		return KindCustom
	}
	if _, ok := kindRanks[k]; ok {
		return k
	}
	return KindCustom
}

// KindValid 报告层名是否合法。
func KindValid(kind string) bool {
	_, ok := kindRanks[strings.TrimSpace(strings.ToLower(kind))]
	return ok
}

// KindRank 返回层序号；未知层按 custom（0）处理。
func KindRank(kind string) int {
	return kindRanks[KindNormalize(kind)]
}

// KindLabel 返回层标题文案；未知层回退层名本身。
func KindLabel(kind string) string {
	if label, ok := kindLabels[kind]; ok {
		return label
	}
	return kind
}

// SuggestKind 按规则构成推断新组的层（checklist 6.4 的建议值，用户可改）：
//   - 全部引用同一个分类库种类 → 该种类层
//   - 混合种类、含非规则集条件（domain / ip_cidr / final …）、或引用了自定义规则集
//     （内容不可静态识别）→ custom 层（层序最高，语义最安全）
//
// 永不返回 final：final 层是结构性的（kind=final 必须且仅有一条 final 规则），
// 由用户显式设置或迁移回填，不从规则构成推断。
func SuggestKind(rules []Rule) string {
	if len(rules) == 0 {
		return KindCustom
	}
	found := ""
	for i := range rules {
		r := &rules[i]
		if r.Type != "rule_set" {
			return KindCustom
		}
		for _, value := range splitRuleSetValues(r.Value) {
			ref, ok := catalog.Parse(value)
			if !ok {
				return KindCustom // 自定义规则集：内部条件未知
			}
			if found == "" {
				found = ref.Kind
			} else if found != ref.Kind {
				return KindCustom
			}
		}
	}
	if found == "" {
		return KindCustom
	}
	return found
}

// splitRuleSetValues 拆分 rule_set 的多值（逗号分隔），去空白项。
func splitRuleSetValues(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// KindLayer 某一层及其成员组（顺序即层内优先级）。
type KindLayer struct {
	Kind   string
	Groups []*RoutingGroup
}

// GroupByKind 按层聚合分流组，返回顺序恒为层序（config.Kinds），空层保留
// （len(Groups)==0），便于界面显示层标题、也便于把规则移进空层。
// 输入顺序不影响结果：层内沿用输入顺序（调用方通常传入已按
// (kind_rank, position, id) 排序的列表）。
func GroupByKind(groups []*RoutingGroup) []KindLayer {
	byKind := map[string][]*RoutingGroup{}
	for _, g := range groups {
		if g == nil {
			continue
		}
		kind := KindNormalize(g.Kind)
		byKind[kind] = append(byKind[kind], g)
	}
	out := make([]KindLayer, 0, len(Kinds))
	for _, kind := range Kinds {
		out = append(out, KindLayer{Kind: kind, Groups: byKind[kind]})
	}
	return out
}
