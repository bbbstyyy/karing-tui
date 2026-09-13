package routing

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// DetectResult 是 route test 的规则命中结果。
// Group/Rule 在命中显式规则或兜底规则时非空；UnknownRules 列出无法在本地
// 展开的规则集条件，调用方应把它们作为不确定性提示展示给用户。
type DetectResult struct {
	Input         string
	Normalized    string
	IsIP          bool
	Matched       bool
	Fallback      bool
	Group         *config.RoutingGroup
	Rule          *config.Rule
	RuleIndex     int
	Target        string
	PrivateDirect bool
	UnknownRules  []string
}

// Detect 按 sing-box 配置生成所使用的顺序检查分流规则。
// 目前规则集二进制（srs）无法由本包直接展开，因此 geoip/geosite/rule_set
// 条件会记录为未知并继续检查后续规则；其余可解释条件可用于离线检测。
func Detect(groups []*config.RoutingGroup, input string) (DetectResult, error) {
	return DetectWithPrivateDirect(groups, input, false)
}

// DetectWithPrivateDirect 与 Detect 相同，但可选择模拟生成器自动插入的
// ip_is_private → direct 规则。CLI 应传入应用设置中的 private_direct 值。
func DetectWithPrivateDirect(groups []*config.RoutingGroup, input string, privateDirect bool) (DetectResult, error) {
	normalized, isIP, err := normalizeInput(input)
	if err != nil {
		return DetectResult{}, err
	}
	result := DetectResult{Input: input, Normalized: normalized, IsIP: isIP, RuleIndex: -1}
	if privateDirect && isIP {
		ip := net.ParseIP(normalized)
		// 与 sing-box network.IsPublicAddr 对齐：私有、回环、链路本地、
		// 未指定及组播地址均属于 ip_is_private。
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
			result.Matched = true
			result.Target = "DIRECT"
			result.PrivateDirect = true
			return result, nil
		}
	}
	groups = SortGroupsByPosition(groups)

	// 生成器对 final 的语义是全局兜底：取配置顺序中第一个 final。
	var fallbackGroup *config.RoutingGroup
	var fallbackRule *config.Rule
	var fallbackIndex int
	for _, group := range groups {
		if group == nil || !group.Enabled {
			continue
		}
		for i := range group.Rules {
			rule := &group.Rules[i]
			if rule.Enabled && rule.Type == "final" && fallbackGroup == nil {
				fallbackGroup, fallbackRule, fallbackIndex = group, rule, i
			}
		}
	}

	for _, group := range groups {
		if group == nil || !group.Enabled {
			continue
		}
		for i := range group.Rules {
			rule := &group.Rules[i]
			if !rule.Enabled || rule.Type == "final" {
				continue
			}
			matched, known := matchRule(rule, normalized, isIP)
			if !known {
				result.UnknownRules = append(result.UnknownRules, ruleDescription(group, i, rule))
				continue
			}
			if matched {
				result.Matched = true
				result.Group, result.Rule, result.RuleIndex, result.Target = group, rule, i, group.Target
				return result, nil
			}
		}
	}

	if fallbackGroup != nil {
		result.Matched = true
		result.Fallback = true
		result.Group, result.Rule, result.RuleIndex, result.Target = fallbackGroup, fallbackRule, fallbackIndex, fallbackGroup.Target
	}
	return result, nil
}

func normalizeInput(input string) (string, bool, error) {
	v := strings.TrimSpace(input)
	if v == "" {
		return "", false, fmt.Errorf("域名或 IP 不能为空")
	}
	if ip := net.ParseIP(v); ip != nil {
		return ip.String(), true, nil
	}
	// DNS 名称不区分大小写；接受完整限定名末尾的点。
	v = strings.TrimSuffix(strings.ToLower(v), ".")
	if len(v) > 253 || strings.ContainsAny(v, " /\\") || !strings.Contains(v, ".") {
		return "", false, fmt.Errorf("输入 %q 不是有效的域名或 IP 地址", input)
	}
	for _, label := range strings.Split(v, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", false, fmt.Errorf("输入 %q 不是有效的域名或 IP 地址", input)
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
				return "", false, fmt.Errorf("输入 %q 不是有效的域名或 IP 地址", input)
			}
		}
	}
	return v, false, nil
}

func matchRule(rule *config.Rule, input string, isIP bool) (bool, bool) {
	if rule.Type == "logical" {
		if len(rule.Conditions) == 0 || (rule.Mode != "and" && rule.Mode != "or") {
			return false, true
		}
		unknown := false
		matched := rule.Mode == "and"
		for i := range rule.Conditions {
			m, k := matchCondition(&rule.Conditions[i], input, isIP)
			if !k {
				unknown = true
				continue
			}
			if rule.Mode == "and" && !m {
				return applyInvert(false, rule.Invert), true
			}
			if rule.Mode == "or" && m {
				return applyInvert(true, rule.Invert), true
			}
			if rule.Mode == "or" {
				matched = false
			}
		}
		if rule.Mode == "and" {
			if unknown {
				return false, false
			}
			return applyInvert(matched, rule.Invert), true
		}
		if unknown {
			return false, false
		}
		return applyInvert(false, rule.Invert), true
	}
	cond := config.RuleCondition{Type: rule.Type, Value: rule.Value, Invert: rule.Invert}
	return matchCondition(&cond, input, isIP)
}

func applyInvert(value, invert bool) bool {
	if invert {
		return !value
	}
	return value
}

func matchCondition(cond *config.RuleCondition, input string, isIP bool) (bool, bool) {
	values := splitValues(cond.Value)
	var matched bool
	switch cond.Type {
	case "domain":
		if !isIP {
			for _, value := range values {
				if strings.EqualFold(strings.TrimSuffix(value, "."), input) {
					matched = true
					break
				}
			}
		}
	case "domain_suffix":
		if !isIP {
			for _, value := range values {
				value = strings.TrimPrefix(strings.ToLower(strings.TrimSuffix(value, ".")), ".")
				if input == value || strings.HasSuffix(input, "."+value) {
					matched = true
					break
				}
			}
		}
	case "domain_keyword":
		if !isIP {
			for _, value := range values {
				if strings.Contains(input, strings.ToLower(value)) {
					matched = true
					break
				}
			}
		}
	case "domain_regex":
		if !isIP {
			// domain_regex 是逗号敏感的单值条件，不能调用 splitValues。
			if re, err := regexp.Compile(strings.TrimSpace(cond.Value)); err == nil {
				matched = re.MatchString(input)
			} else {
				return false, true
			}
		}
	case "ip_cidr":
		if isIP {
			ip := net.ParseIP(input)
			for _, value := range values {
				if _, network, err := net.ParseCIDR(value); err == nil && network.Contains(ip) {
					matched = true
					break
				}
			}
		}
	case "geoip", "geosite", "rule_set":
		return false, false
	default:
		return false, true
	}
	return applyInvert(matched, cond.Invert), true
}

func splitValues(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func ruleDescription(group *config.RoutingGroup, index int, rule *config.Rule) string {
	value := rule.Value
	if rule.Type == "logical" {
		value = config.FormatLogicalExpr(rule.Mode, rule.Conditions)
	}
	if value == "" {
		return fmt.Sprintf("%s #%d %s", group.Name, index+1, rule.Type)
	}
	return fmt.Sprintf("%s #%d %s=%s", group.Name, index+1, rule.Type, value)
}

// SortGroupsByPosition 返回按分流组 Position、ID 排序的副本，供不来自 DB 的调用方使用。
func SortGroupsByPosition(groups []*config.RoutingGroup) []*config.RoutingGroup {
	out := append([]*config.RoutingGroup(nil), groups...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].ID < out[j].ID
	})
	return out
}
