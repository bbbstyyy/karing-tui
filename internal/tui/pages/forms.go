package pages

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/catalog"
	"github.com/bbbstyyy/karing-tui/internal/subscription"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
)

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

func referenceChoices(app *application.App, typ, current string) []components.Option {
	var choices []components.Option
	kind := ""
	if typ == "geosite" || typ == "geoip" {
		kind = typ
	}
	if typ == "rule_set" {
		if sets, err := app.DB.ListRuleSets(); err == nil {
			for _, rs := range sets {
				if rs.Enabled {
					choices = append(choices, components.Option{Value: rs.Tag, Label: rs.Name + " · " + rs.Tag})
				}
			}
		}
	}
	for _, ref := range catalog.Search(kind, "", 0) {
		value := ref.String()
		if typ != "rule_set" {
			value = ref.Code
		}
		choices = append(choices, components.Option{Value: value, Label: ref.String()})
	}
	// Keep valid legacy tag references selectable when editing an older model.
	for _, v := range strings.Split(current, ",") {
		v = strings.TrimSpace(v)
		if typ == "rule_set" {
			if ref, ok := catalog.Parse(v); ok && v != ref.String() {
				choices = append(choices, components.Option{Value: v, Label: ref.String() + " · 兼容标识 " + v})
			}
		}
	}
	return choices
}

func configureRuleValue(app *application.App, form *components.Form) {
	field := form.Field("value")
	if field == nil {
		return
	}
	typ := form.ValueByKey("type")
	field.When = func(f *components.Form) bool { return f.ValueByKey("type") != "final" }
	if invert := form.Field("invert"); invert != nil {
		invert.When = field.When
	}
	field.Action = ""
	field.Validate = required("规则值")
	field.Choice, field.Multi = false, false
	field.Options = nil
	switch typ {
	case "logical":
		field.Action = "conditions"
		field.Help = "Enter 按条件行编辑；AND 全部满足、OR 任一满足，每行可独立 NOT 取反。"
	case "rule_set", "geosite", "geoip":
		field.Choice, field.Multi = true, true
		field.Options = referenceChoices(app, typ, field.Value())
		field.Help = "Enter 搜索已有规则集或分类；Space 多选，Enter 确认。保存内部引用。"
	case "domain_regex":
		field.Help = "RE2 正则；逗号作为正则的一部分保留，多分支用 |。"
	default:
		field.Help = "可填写多个值，以逗号分隔；保存后 Ctrl+A 应用配置。"
	}
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
