// Package config 定义内部统一配置模型与 sing-box 配置生成。
// 用户的所有操作都落在这些模型上，sing-box JSON 仅由生成器输出。
package config

import (
	"fmt"
	"strings"
	"time"
)

// Subscription 订阅源。
type Subscription struct {
	ID               int64
	Name             string
	URL              string
	Enabled          bool
	UserAgent        string
	DownloadStrategy string // prefer_proxy / prefer_direct / only_proxy / only_direct
	NodeFilter       string // 可选过滤表达式：逗号分隔，! 前缀表示排除（支持 RE2）
	SortBy           string // name / latency；空值为 name
	AutoTest         bool   // 更新后自动测速（由上层注入测速回调）
	AutoClean        bool   // 自动删除测速失败节点
	TrafficUpload    int64
	TrafficDownload  int64
	TrafficTotal     int64
	ExpireAt         time.Time
	LastUpdated      time.Time // 零值表示从未更新
	NodeCount        int
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// DownloadStrategy 是订阅内容的下载通道策略。
// proxy 使用 Settings.DownloadProxy 指定的用户代理地址；空地址表示没有可用代理。
const (
	DownloadPreferProxy  = "prefer_proxy"
	DownloadPreferDirect = "prefer_direct"
	DownloadOnlyProxy    = "only_proxy"
	DownloadOnlyDirect   = "only_direct"
)

const DefaultDownloadStrategy = DownloadPreferProxy

// NormalizeDownloadStrategy 校验并补全订阅下载策略。
func NormalizeDownloadStrategy(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultDownloadStrategy, nil
	}
	switch v {
	case DownloadPreferProxy, DownloadPreferDirect, DownloadOnlyProxy, DownloadOnlyDirect:
		return v, nil
	default:
		return "", fmt.Errorf("订阅下载通道非法: %q", v)
	}
}

// Node 统一节点模型，来自订阅或手动添加。
// 协议专属字段（uuid、password、sni 等）统一放在 Metadata，保持 schema 稳定。
type Node struct {
	ID             int64
	SubscriptionID int64 // 0 表示手动节点
	Name           string
	Protocol       string // shadowsocks / vmess / vless / trojan / hysteria2 / tuic ...
	Server         string
	Port           int
	TLS            bool
	Transport      string // "" / ws / grpc / h2 / httpupgrade / xhttp
	Enabled        bool
	LatencyMS      int64 // -1 表示未测速或测速失败
	LastTested     time.Time
	Metadata       map[string]any
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ManualSubscriptionID 是手动节点的 subscription_id 取值。
const ManualSubscriptionID int64 = 0

// DefaultTestURL 默认延迟测试地址（204 无内容，测纯连接耗时）。
const DefaultTestURL = "http://www.gstatic.com/generate_204"

// DefaultLatencyTimeoutMS 单节点/单代理组延迟探测的默认超时（毫秒）。
//
// 两条测速路径必须共用同一个默认值：代理组测速走运行中核心的 clash API，
// 独立节点测速走一次性核心的 /proxies/{tag}/delay。超时不一致时，跨境或首次
// 解析较慢的节点会在一处成功、在另一处超时，用户看到的是「同一节点两个结论」。
const DefaultLatencyTimeoutMS = 5000

// ProxyGroup 代理组；成员可为节点、嵌套代理组或特殊值 all（全部启用节点）。
type ProxyGroup struct {
	ID        int64
	Name      string
	Type      string // select / urltest（sing-box 仅支持这两种组）
	TestURL   string
	IntervalS int
	Members   []ProxyGroupMember
	Selected  string // select 组的当前选中成员："node:<id>" / "group:<id>"；空表示未选
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ProxyGroupMember 代理组成员引用。
type ProxyGroupMember struct {
	Type     string // node / group / all
	ID       int64  // all 时无意义
	Position int
}

// MemberKey 成员引用键，格式 "type:id"（all 为 "all"），用于去重与选中持久化。
func (m ProxyGroupMember) MemberKey() string {
	if m.Type == "all" {
		return "all"
	}
	return fmt.Sprintf("%s:%d", m.Type, m.ID)
}

// RoutingGroup 分流组：一组规则共同指向一个目标（代理组名 / DIRECT / BLOCK）。
//
// Kind 是层（见 routing_kind.go），Position 是**层内序号**而非全局序号：
// 生成配置时的优先级 = (层序 KindRank, 层内 Position, ID)。层内顺序即优先级。
type RoutingGroup struct {
	ID       int64
	Name     string
	Target   string
	Kind     string // custom / geosite / geoip / acl / final；空值按 custom 处理
	Position int    // 层内序号
	Enabled  bool
	Rules    []Rule
}

// Layer 返回归一化后的层名（空值与非法值按 custom）。
func (g *RoutingGroup) Layer() string { return KindNormalize(g.Kind) }

// HasFinalRule 报告组内是否含 final 规则（不论启用状态）。
// 引入 Kind 后 final 语义收紧为结构性约束（见 routing.Manager.validate）。
func (g *RoutingGroup) HasFinalRule() bool {
	if g == nil {
		return false
	}
	for _, r := range g.Rules {
		if r.Type == "final" {
			return true
		}
	}
	return false
}

// Rule 分流规则，属于某个分流组；顺序即优先级。
// Type 为 logical 时是逻辑规则：由 Conditions 按 Mode（and/or）组合，
// Invert 对整个逻辑表达式取反（NOT）；此时 Value 未用。
type Rule struct {
	ID             int64
	RoutingGroupID int64
	Type           string // domain / domain_suffix / domain_keyword / domain_regex / ip_cidr / geoip / geosite / rule_set / final / logical
	Value          string // 多值以逗号分隔；logical 时未用
	Invert         bool
	Enabled        bool
	Position       int

	Mode       string          // logical: "and" / "or"
	Conditions []RuleCondition // logical: 子条件（仅匹配字段，不可再嵌套逻辑规则）
}

// RuleCondition 逻辑规则的子条件。
type RuleCondition struct {
	Type   string // domain / domain_suffix / domain_keyword / domain_regex / ip_cidr / geoip / geosite / rule_set
	Value  string // 多值以逗号分隔
	Invert bool
}

// condTypes 逻辑条件允许的类型（不含 final）。
var condTypes = map[string]bool{
	"domain": true, "domain_suffix": true, "domain_keyword": true, "domain_regex": true,
	"ip_cidr": true, "geoip": true, "geosite": true, "rule_set": true,
}

// CondTypeValid 报告类型是否可作为逻辑规则的子条件。
func CondTypeValid(typ string) bool { return condTypes[typ] }

// ParseLogicalExpr 解析逻辑规则表达式："条件 && 条件"（and）/ "条件 || 条件"（or），
// 条件形如 "类型=值"，"!" 前缀表示反转（NOT）。单条件默认按 and。
func ParseLogicalExpr(s string) (mode string, conds []RuleCondition, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil, fmt.Errorf("逻辑规则表达式不能为空")
	}
	sep := ""
	for _, candidate := range []string{"&&", "||"} {
		if strings.Contains(s, candidate) {
			if sep != "" {
				return "", nil, fmt.Errorf("逻辑规则不能混用 && 与 ||")
			}
			sep = candidate
		}
	}
	if sep == "" {
		sep = "&&" // 单条件
	}
	mode = "and"
	if sep == "||" {
		mode = "or"
	}
	for _, part := range strings.Split(s, sep) {
		part = strings.TrimSpace(part)
		if part == "" {
			return "", nil, fmt.Errorf("逻辑规则存在空条件")
		}
		var cond RuleCondition
		if rest, ok := strings.CutPrefix(part, "!"); ok {
			cond.Invert = true
			part = strings.TrimSpace(rest)
		}
		typ, val, ok := strings.Cut(part, "=")
		if !ok {
			return "", nil, fmt.Errorf("条件 %q 缺少 =（应为 类型=值）", part)
		}
		cond.Type = strings.ToLower(strings.TrimSpace(typ))
		cond.Value = strings.TrimSpace(val)
		if !condTypes[cond.Type] {
			return "", nil, fmt.Errorf("未知条件类型 %q", cond.Type)
		}
		if cond.Value == "" {
			return "", nil, fmt.Errorf("条件 %s 的值不能为空", cond.Type)
		}
		conds = append(conds, cond)
	}
	return mode, conds, nil
}

// FormatLogicalExpr 把逻辑规则格式化为表达式（编辑回显与列表展示用）。
func FormatLogicalExpr(mode string, conds []RuleCondition) string {
	sep := " && "
	if mode == "or" {
		sep = " || "
	}
	parts := make([]string, 0, len(conds))
	for _, c := range conds {
		s := c.Type + "=" + c.Value
		if c.Invert {
			s = "!" + s
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, sep)
}

// RuleSet 规则集（远程 .srs/.json 或本地文件）。
type RuleSet struct {
	ID         int64
	Name       string
	Tag        string
	SourceType string // remote / local
	Format     string // srs / json
	URL        string
	Enabled    bool
	CachedPath string
	UpdatedAt  time.Time
}

// DNSServer DNS 服务器条目，生成 sing-box 1.12+ 新版格式。
// Address 按类型填写：udp/tcp 填 "主机:端口" 或 "主机"（默认 53）；
// tls/https/quic/h3 填服务器主机或完整 URL；local 填 "-"。
type DNSServer struct {
	ID              int64
	Tag             string
	Type            string // udp / tcp / tls / https / quic / h3 / local
	Address         string // 服务器地址（不含类型前缀）
	AddressResolver string // 服务器地址为域名时用于解析的 DNS tag
	Detour          string // 出站 detour tag
	Enabled         bool
	Position        int
}

// DNSRule DNS 规则：按域名/规则集指定 DNS 服务器。
type DNSRule struct {
	ID       int64
	Type     string // domain / domain_suffix / domain_keyword / rule_set
	Value    string // 多值逗号分隔；rule_set 为规则集 Tag
	Server   string // DNS 服务器 tag
	Enabled  bool
	Position int
}

// DNSConfig DNS 全局配置。
type DNSConfig struct {
	Servers       []DNSServer
	Rules         []DNSRule // 按域名/规则集指定 DNS 服务器
	Strategy      string    // prefer_ipv4 / prefer_ipv6 / ipv4_only / ipv6_only
	FakeIPEnabled bool
	FakeIPRange   string // 如 198.18.0.0/15
	Final         string // 默认 DNS 服务器 tag
}

// Settings 应用级设置，持久化在 settings 键值表。
type Settings struct {
	MixedPort      int    // mixed 入站监听端口
	AllowLAN       bool   // 监听 0.0.0.0 或仅 127.0.0.1
	DownloadProxy  string // 订阅/规则集/sing-box 下载代理，如 http://127.0.0.1:7890
	LogLevel       string // sing-box 日志级别
	ClashAPIPort   int    // clash API 监听端口（0 = 关闭）；Dashboard 依此查询流量与组状态
	ClashAPISecret string // clash API 鉴权密钥，空表示不鉴权（仅本机访问）

	// PrivateDirect 目标为私有地址（内网/回环/链路本地）的连接直连，规则置于用户规则之前。
	PrivateDirect bool
	// ResolveIPRules 为 IP 类条件（ip_cidr/geoip/IP 规则集）自动插入 resolve 动作，
	// 使其对域名连接也生效。代价：resolve 之后的连接改用本机解析出的 IP 拨号，
	// 走代理时代理侧收不到域名（详见 generator.buildRoute）。默认关闭。
	ResolveIPRules bool

	AutoUpdateMinutes int // 订阅自动更新间隔（分钟，0 = 关闭）；TUI 启动时读取
}

// MaxAutoUpdateMinutes is the largest interval representable by
// time.Duration. Keeping this bound in the model lets storage and the TUI
// reject values before they can overflow when converted to a duration.
const MaxAutoUpdateMinutes = 153722867

// DefaultSettings 返回默认设置。
func DefaultSettings() Settings {
	return Settings{
		MixedPort:     2080,
		AllowLAN:      false,
		DownloadProxy: "",
		LogLevel:      "info",
		ClashAPIPort:  9090,

		PrivateDirect:  true,  // 内网直连：无副作用，纯 IP 连接即可命中
		ResolveIPRules: false, // 默认关闭：会改变走代理流量的拨号地址，需用户显式开启

		AutoUpdateMinutes: 0,
	}
}
