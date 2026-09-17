package pages

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/catalog"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/subscription"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
)

// kindChoices 分流层选项（按层序列出，排在前面即优先级更高）。
func kindChoices() []components.Option {
	choices := make([]components.Option, 0, len(config.Kinds))
	for _, kind := range config.Kinds {
		choices = append(choices, components.Option{
			Value: kind,
			Label: kind + " · " + config.KindLabel(kind),
		})
	}
	return choices
}

func required(label string) func(string) error {
	return func(v string) error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("请填写%s", label)
		}
		return nil
	}
}

func integer(label string, minValue, maxValue int, optional bool) func(string) error {
	return func(v string) error {
		if optional && strings.TrimSpace(v) == "" {
			return nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < minValue || n > maxValue {
			return fmt.Errorf("%s须为 %d–%d 的整数", label, minValue, maxValue)
		}
		return nil
	}
}

func httpURL(optional bool) func(string) error {
	return func(v string) error {
		v = strings.TrimSpace(v)
		if optional && v == "" {
			return nil
		}
		if !strings.Contains(v, "://") {
			v = "https://" + v
		}
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return fmt.Errorf("请填写有效的 HTTP/HTTPS URL")
		}
		return nil
	}
}

func section(f *components.Form, label string, advanced bool, fields ...string) {
	for _, key := range fields {
		if field := f.Field(key); field != nil {
			field.Section, field.Advanced = label, advanced
		}
	}
}

func fieldIs(key string, values ...string) func(*components.Form) bool {
	return func(f *components.Form) bool {
		value := f.ValueByKey(key)
		for _, v := range values {
			if value == v {
				return true
			}
		}
		return false
	}
}

func proxyChoices(app *application.App, block bool) []components.Option {
	choices := []components.Option{{Value: "DIRECT", Label: "DIRECT · 直连"}}
	if block {
		choices = append(choices, components.Option{Value: "BLOCK", Label: "BLOCK · 拒绝连接"})
	}
	if groups, err := app.DB.ListProxyGroups(); err == nil {
		for _, g := range groups {
			choices = append(choices, components.Option{Value: g.Name, Label: g.Name + " · " + g.Type})
		}
	}
	return choices
}

func ruleTypeChoices(logical bool) []components.Option {
	choices := []components.Option{
		{Value: "domain", Label: "domain · 完整域名"}, {Value: "domain_suffix", Label: "domain_suffix · 域名后缀"},
		{Value: "domain_keyword", Label: "domain_keyword · 域名关键词"}, {Value: "domain_regex", Label: "domain_regex · 正则表达式"},
		{Value: "ip_cidr", Label: "ip_cidr · IP 网段"}, {Value: "geoip", Label: "geoip · IP 分类"},
		{Value: "geosite", Label: "geosite · 域名分类"}, {Value: "rule_set", Label: "rule_set · 规则集或内置分类"},
	}
	if logical {
		choices = append(choices,
			components.Option{Value: "final", Label: "final · 其余全部流量"},
			components.Option{Value: "logical", Label: "logical · AND / OR / NOT 条件组合"})
	}
	return choices
}

// --- 值字段候选项 ---
//
// referenceChoices 把两类完全不同的数据源拼在一起：
//
//   - **静态**：内嵌分类库（geosite ≈1953、geoip ≈278、acl ≈171 条）。它随二进制
//     发行，运行期不会变，只有程序版本/内嵌清单变化才失效，因此可以缓存派生结果。
//   - **动态**：数据库里的规则集（rule_set 类型）。用户随时增删，必须每次现查。
//
// catalog 层已经用 sync.Once 缓存了底层清单，但「分类码 → components.Option」
// 这层转换仍是每次 O(2400)、每条一次 "kind:code" 拼接的重复劳动。放在 TUI 层缓存
// 是因为 catalog 是零依赖叶子包（catalog/catalog.go:10），不能反向引用 components。

// ruleRefKey 区分同一份分类库数据的两种选项形态：geosite / geoip 用分类码本身
// （"cn"），rule_set 用完整引用（"geosite:cn"）。kind 为空表示全部种类。
type ruleRefKey struct {
	kind    string
	refForm bool
}

var (
	ruleRefMu    sync.Mutex
	ruleRefCache = map[ruleRefKey][]components.Option{}
)

// staticRuleRefOptions 返回静态分类库的候选项（进程级缓存，首次命中时构造）。
//
// 返回值是**共享只读切片**：容量已冻结（三段切片表达式），调用方 append 时会另开
// 底层数组，不会写穿缓存；表单也只读 Options（过滤、校验、渲染都只遍历）。
func staticRuleRefOptions(kind string, refForm bool) []components.Option {
	key := ruleRefKey{kind: kind, refForm: refForm}
	ruleRefMu.Lock()
	defer ruleRefMu.Unlock()
	if cached, ok := ruleRefCache[key]; ok {
		return cached
	}
	refs := catalog.Search(kind, "", 0)
	built := make([]components.Option, 0, len(refs))
	for _, ref := range refs {
		value := ref.Code
		if refForm {
			value = ref.String()
		}
		built = append(built, components.Option{Value: value, Label: ref.String()})
	}
	built = built[:len(built):len(built)]
	ruleRefCache[key] = built
	return built
}

// ruleSetChoices 读取数据库里的规则集。动态数据，每次调用都重新查询——
// 表单打开时新建的规则集必须立刻可见。
func ruleSetChoices(app *application.App) []components.Option {
	sets, err := app.DB.ListRuleSets()
	if err != nil {
		return nil
	}
	var choices []components.Option
	for _, rs := range sets {
		if rs.Enabled {
			choices = append(choices, components.Option{Value: rs.Tag, Label: rs.Name + " · " + rs.Tag})
		}
	}
	return choices
}

// legacyRuleRefChoices 保留旧模型里合法的派生 tag 引用（"geosite-cn" 这类写法），
// 使编辑老数据时原值仍可选中。只在 rule_set 类型下有意义。
func legacyRuleRefChoices(typ, current string) []components.Option {
	if typ != "rule_set" {
		return nil
	}
	var choices []components.Option
	for _, v := range strings.Split(current, ",") {
		v = strings.TrimSpace(v)
		if ref, ok := catalog.Parse(v); ok && v != ref.String() {
			choices = append(choices, components.Option{Value: v, Label: ref.String() + " · 兼容标识 " + v})
		}
	}
	return choices
}

func referenceChoices(app *application.App, typ, current string) []components.Option {
	kind, refForm := "", false
	switch typ {
	case "geosite", "geoip":
		kind = typ
	case "rule_set":
		refForm = true
	}
	static := staticRuleRefOptions(kind, refForm)

	var dynamic []components.Option
	if typ == "rule_set" {
		dynamic = ruleSetChoices(app)
	}
	legacy := legacyRuleRefChoices(typ, current)
	if len(dynamic) == 0 && len(legacy) == 0 {
		// 纯静态路径：直接交出共享缓存，零分配。
		return static
	}
	// 顺序与改动前一致：数据库条目 → 分类库条目 → 兼容引用。
	choices := make([]components.Option, 0, len(dynamic)+len(static)+len(legacy))
	choices = append(choices, dynamic...)
	choices = append(choices, static...)
	return append(choices, legacy...)
}

// syncRuleValueOnTypeChange 在「类型」字段真的变化时才重配值字段。
//
// 规则类表单的每一个按键都会走到这里，因此它必须便宜：类型没变就直接返回。
// 值字段自身的变化（多选增删、继续输入）不会改变值字段的行为或选项构成，
// 无需任何重配——改动前每敲一个键都要跑一次 configureRuleValue，对
// rule_set / geosite / geoip 而言就是一次 DB 查询加一次分类库全量物化。
func syncRuleValueOnTypeChange(app *application.App, form *components.Form, previousType string) {
	if form.ValueByKey("type") == previousType {
		return
	}
	configureRuleValue(app, form)
}

// configureRuleValue 重配规则表单的值字段。只应在两种时机调用：
//   - 表单刚打开（configureForm / logicalEditor.open）；
//   - 用户切换了「类型」字段（经 syncRuleValueOnTypeChange）。
func configureRuleValue(app *application.App, form *components.Form) {
	field := form.Field("value")
	if field == nil {
		return
	}
	configureRuleValueField(form, field)
	rebuildRuleValueOptions(app, form, field)
}

// configureRuleValueField 只设置依赖「类型」的字段行为，不构造选项列表——
// 这部分没有 I/O，便宜。
func configureRuleValueField(form *components.Form, field *components.FormField) {
	field.When = func(f *components.Form) bool { return f.ValueByKey("type") != "final" }
	if invert := form.Field("invert"); invert != nil {
		invert.When = field.When
	}
	field.Action = ""
	field.Validate = required("规则值")
	field.Choice, field.Multi = false, false
	// 经 SetFieldOptions 而非直接赋值：Form 侧的选项搜索键缓存（C19）依赖
	// optionsChanged 失效，直接改 field.Options 会让过滤读到旧选项。
	form.SetFieldOptions(field.Key, nil)
	switch form.ValueByKey("type") {
	case "logical":
		field.Action = "conditions"
		field.Help = "Enter 按条件行编辑；AND 全部满足、OR 任一满足，每行可独立 NOT 取反。"
	case "rule_set", "geosite", "geoip":
		field.Choice, field.Multi = true, true
		field.Help = "Enter 搜索已有规则集或分类；Space 多选，Enter 确认。保存内部引用。"
	case "domain_regex":
		field.Help = "RE2 正则；逗号作为正则的一部分保留，多分支用 |。"
	default:
		field.Help = "可填写多个值，以逗号分隔；保存后 Ctrl+A 应用配置。"
	}
}

// rebuildRuleValueOptions 构造依赖类型的选项列表：rule_set 要查一次 DB，
// geosite / geoip 要物化一次分类库。因此只在类型变化或表单打开时执行，
// 不参与按键热路径。
func rebuildRuleValueOptions(app *application.App, form *components.Form, field *components.FormField) {
	if !field.Choice {
		return
	}
	// 经 SetFieldOptions 写入：让 Form 的选项搜索键缓存随之失效（C19）。
	form.SetFieldOptions(field.Key, referenceChoices(app, form.ValueByKey("type"), field.Value()))
}

func (p *Profiles) configureSubForm() {
	f := &p.form
	f.Field("name").Validate = required("订阅名称")
	f.Field("url").Validate = httpURL(false)
	f.Field("filter").Validate = subscription.ValidateNodeFilter
	section(f, "订阅", false, "name", "url")
	section(f, "下载与更新", true, "ua", "download_strategy", "filter", "sort", "autotest", "autoclean")
	f.SaveLabel = "仅保存"
	f.ExtraAction, f.ExtraLabel = "save-update", "保存并更新"
	f.Begin()
}

func (p *Profiles) configureNodeForm() {
	f := &p.form
	f.SetChoices("protocol", []components.Option{
		{Value: "shadowsocks", Label: "Shadowsocks · 密码与加密方法"}, {Value: "vmess", Label: "VMess · UUID 认证"},
		{Value: "vless", Label: "VLESS · UUID 认证"}, {Value: "trojan", Label: "Trojan · TLS 密码认证"},
		{Value: "hysteria2", Label: "Hysteria 2 · QUIC 密码认证"}, {Value: "hysteria", Label: "Hysteria · QUIC 认证"},
		{Value: "tuic", Label: "TUIC · QUIC，UUID 与密码"}, {Value: "socks", Label: "SOCKS · 通用代理"},
		{Value: "socks5", Label: "SOCKS5 · 用户名与密码可选"}, {Value: "http", Label: "HTTP · 用户名与密码可选"},
		{Value: "ssh", Label: "SSH · 用户名与密码或私钥"}, {Value: "shadowtls", Label: "ShadowTLS · TLS 伪装"},
		{Value: "anytls", Label: "AnyTLS · TLS 密码认证"}, {Value: "naive", Label: "Naive · HTTPS 用户名与密码"},
	})
	f.SetChoices("transport", []components.Option{
		{Value: "", Label: "默认传输"}, {Value: "ws", Label: "WebSocket"},
		{Value: "grpc", Label: "gRPC"}, {Value: "http", Label: "HTTP"},
		{Value: "httpupgrade", Label: "HTTPUpgrade"},
	})
	f.Field("server").Validate = required("服务器")
	f.Field("port").Validate = integer("端口", 1, 65535, false)
	f.Field("method").When = fieldIs("protocol", "shadowsocks", "vmess")
	f.Field("method").Help = "Shadowsocks 填服务端加密方法；VMess 留空使用 auto。"
	f.Field("uuid").When = fieldIs("protocol", "tuic")
	f.Field("user").When = fieldIs("protocol", "socks", "socks5", "http", "ssh", "naive")
	f.Field("key_path").When = fieldIs("protocol", "ssh")
	f.Field("transport").When = fieldIs("protocol", "vmess", "vless", "trojan")
	for _, key := range []string{"host", "path"} {
		f.Field(key).When = func(f *components.Form) bool { return f.ValueByKey("transport") != "" }
	}
	f.Field("sni").When = func(f *components.Form) bool {
		return f.ValueByKey("tls") == "true" || mandatoryTLS(f.ValueByKey("protocol"))
	}
	section(f, "节点与认证", false, "name", "protocol", "server", "port", "cred", "method", "uuid", "user")
	section(f, "TLS 与传输", true, "sni", "transport", "host", "path", "tls", "key_path")
	f.Begin()
}

func mandatoryTLS(protocol string) bool {
	switch protocol {
	case "trojan", "hysteria2", "hysteria", "tuic", "anytls", "shadowtls", "naive":
		return true
	}
	return false
}

func configureGroupForm(f *components.Form) {
	f.SetChoices("type", []components.Option{
		{Value: "select", Label: "Select · 手动选择"},
		{Value: "urltest", Label: "URLTest · 自动测速选择"},
	})
	f.Field("name").Validate = required("代理组名称")
	f.Field("url").When = fieldIs("type", "urltest")
	f.Field("url").Validate = httpURL(true)
	f.Field("interval").When = fieldIs("type", "urltest")
	f.Field("interval").Validate = integer("测速间隔", 1, 86400, true)
	section(f, "代理组", false, "name", "type")
	section(f, "自动测速", true, "url", "interval")
	f.Field("url").Help = "留空使用默认 HTTP 204 测速地址；应用后由核心定时测速。"
	f.Field("interval").Help = "单位秒；留空默认 300 秒，Ctrl+A 后生效。"
	f.Begin()
}
