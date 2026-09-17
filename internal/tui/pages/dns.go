package pages

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/dns"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
)

// dnsMode DNS 页的视图模式。
type dnsMode int

const (
	dnsServers dnsMode = iota // DNS 服务器列表
	dnsRules                  // DNS 规则列表
	dnsOptions                // 全局选项表单（策略/FakeIP/final）
	dnsForm                   // 服务器/规则表单
	dnsConfirm                // 删除确认
	dnsGlobal                 // 全局选项概览
)

// DNSPage DNS 配置页：服务器管理、DNS 规则、FakeIP 与策略。
type DNSPage struct {
	base
	mode   dnsMode
	cfg    config.DNSConfig
	err    error
	status string

	list       components.SimpleList
	serverList components.SimpleList
	ruleList   components.SimpleList

	// servers / rules 是 DNS 配置的唯一内存副本，只在 reload()（Update 阶段）
	// 写入；View、预览与所有选择器一律读它。改动前 table()、selectionDetails()
	// 与 selectedServer()/selectedRule() 各自查一次 SQLite，同一次渲染会重复
	// 2–3 次 ListServers() / ListRules()。
	servers []*config.DNSServer
	rules   []*config.DNSRule

	form        components.Form
	formKind    string // add-server / edit-server / add-rule / edit-rule
	editID      int64
	optionsBack dnsMode

	confirm   components.Confirm
	confirmID int64
}

// NewDNS 创建 DNS 页。
func NewDNS(app *application.App) *DNSPage {
	return &DNSPage{base: base{app: app}}
}

func (d *DNSPage) Title() string { return "DNS" }

// Editing 表单输入状态时拦截全局键位。
func (d *DNSPage) Editing() bool {
	return d.detailActive || d.mode == dnsForm || d.mode == dnsOptions || d.mode == dnsConfirm
}

func (d *DNSPage) Init() tea.Cmd { return nil }

func (d *DNSPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	if d.detailActive || !d.Editing() {
		if d.handleDetails(msg, d.err) {
			return d, nil
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		d.SetSize(msg.Width, msg.Height)
		// 只按新宽度重建列表；resize 不查库（宽度只影响内存里的列宽计算）。
		d.rebuildDNSList()
		return d, nil
	case ActivateMsg:
		d.reload()
		return d, nil
	case components.ConfirmMsg:
		return d.onConfirm(msg)
	case tea.KeyMsg:
		return d.handleKey(msg)
	}
	if d.mode == dnsForm || d.mode == dnsOptions {
		return d, d.form.Update(msg)
	}
	return d, nil
}

func (d *DNSPage) handleKey(msg tea.KeyMsg) (Page, tea.Cmd) {
	key := commandKey(d, msg)
	if !d.Editing() && (key == "[" || key == "]") {
		if d.mode == dnsServers {
			d.serverList = d.list
		}
		if d.mode == dnsRules {
			d.ruleList = d.list
		}
		tabs := []dnsMode{dnsServers, dnsRules, dnsGlobal}
		index := 0
		for i, mode := range tabs {
			if mode == d.mode {
				index = i
			}
		}
		delta := 1
		if key == "[" {
			delta = -1
		}
		d.mode = tabs[(index+delta+len(tabs))%len(tabs)]
		if d.mode == dnsServers {
			d.list = d.serverList
		}
		if d.mode == dnsRules {
			d.list = d.ruleList
		}
		d.reload()
		return d, nil
	}
	if key == "enter" || key == "alt+enter" {
		if d.mode == dnsRules {
			if rules, i, ok := d.selectedRule(); ok {
				r := rules[i]
				d.openDetails("DNS 规则", fmt.Sprintf("类型: %s\n条件: %s\n服务器: %s\n启用: %v", r.Type, r.Value, r.Server, r.Enabled))
				return d, nil
			}
		}
		if d.mode == dnsServers {
			if servers, i, ok := d.selectedServer(); ok {
				s := servers[i]
				d.openDetails("DNS 服务器", fmt.Sprintf("名称: %s\n类型: %s\n地址: %s\n解析 DNS: %s\n出站: %s", s.Tag, s.Type, s.Address, s.AddressResolver, s.Detour))
				return d, nil
			}
		}
	}

	switch d.mode {
	case dnsGlobal:
		switch key {
		case "e", "enter", "F":
			d.openOptions()
		case "r":
			d.reload()
		case "esc":
			d.mode = dnsServers
			d.list = d.serverList
			d.reload()
		}
		return d, nil
	case dnsForm:
		action, cmd := d.form.Handle(msg)
		if d.formBackMode() == dnsRules {
			configureRuleValue(d.app, &d.form)
		}
		switch action {
		case "cancel":
			d.mode = d.formBackMode()
		case "save":
			d.submitForm()
		}
		return d, cmd

	case dnsOptions:
		action, cmd := d.form.Handle(msg)
		switch action {
		case "cancel":
			d.mode = d.optionsBack
			d.reload()
		case "save":
			d.submitOptions()
		}
		return d, cmd

	case dnsConfirm:
		if consumed, cmd := d.confirm.Update(msg); consumed {
			return d, cmd
		}
		return d, nil

	case dnsRules:
		if consumed, cmd := d.list.Update(msg); consumed {
			return d, cmd
		}
		switch key {
		case "esc":
			d.ruleList = d.list
			d.list = d.serverList
			d.mode = dnsServers
			d.reload()
		case "a":
			d.openForm("add-rule")
		case "e":
			if rules, i, ok := d.selectedRule(); ok {
				rule := rules[i]
				d.form = components.NewForm("编辑 DNS 规则",
					[]string{"类型", "值", "服务器"},
					[]string{"type", "value", "server"},
					[]string{"domain/domain_suffix/domain_keyword/rule_set", "多值逗号分隔；rule_set 填 Tag", "DNS 服务器 Tag"})
				d.formKind = "edit-rule"
				d.editID = rule.ID
				d.form.SetValueByKey("type", rule.Type)
				d.form.SetValueByKey("value", rule.Value)
				d.form.SetValueByKey("server", rule.Server)
				d.err = nil
				d.configureForm()
				d.mode = dnsForm
			}
		case "D", "d":
			if rules, i, ok := d.selectedRule(); ok {
				d.confirm = components.NewConfirm("delete-dns-rule", fmt.Sprintf("删除 DNS 规则 %s = %s → %s？", rules[i].Type, rules[i].Value, rules[i].Server))
				d.confirmID = rules[i].ID
				d.mode = dnsConfirm
			}
		case " ":
			if rules, i, ok := d.selectedRule(); ok {
				if err := d.app.DNS.SetRuleEnabled(rules[i].ID, !rules[i].Enabled); err != nil {
					d.err = err
				} else {
					d.app.MarkConfigDirty()
				}
				d.reload()
			}
		case "J", "K":
			if rules, i, ok := d.selectedRule(); ok {
				delta := -1
				if key == "J" {
					delta = 1
				}
				if err := d.app.DNS.MoveRule(rules[i].ID, delta); err != nil {
					d.err = err
				} else {
					d.app.MarkConfigDirty()
					d.status = "规则顺序已保存，光标保持在同一规则"
				}
				d.reload()
			}
		case "r":
			d.reload()
		case "]", "F":
			d.openOptions()
		case "[":
			d.ruleList = d.list
			d.list = d.serverList
			d.mode = dnsServers
			d.reload()
		}
		return d, nil

	default: // dnsServers
		if consumed, cmd := d.list.Update(msg); consumed {
			return d, cmd
		}
		switch key {
		case "a":
			d.openForm("add-server")
		case "e":
			if servers, i, ok := d.selectedServer(); ok {
				s := servers[i]
				d.form = components.NewForm("编辑 DNS 服务器",
					[]string{"名称", "类型", "地址", "域名解析 DNS", "出站代理组"},
					[]string{"tag", "type", "address", "resolver", "detour"},
					[]string{"", "udp/tcp/tls/https/quic/h3/local", "地址或 URL", "地址为域名时的解析 DNS", "出站 tag，留空直连"})
				d.formKind = "edit-server"
				d.editID = s.ID
				d.form.SetValueByKey("tag", s.Tag)
				d.form.SetValueByKey("type", s.Type)
				d.form.SetValueByKey("address", s.Address)
				d.form.SetValueByKey("resolver", s.AddressResolver)
				d.form.SetValueByKey("detour", s.Detour)
				d.err = nil
				d.configureForm()
				d.mode = dnsForm
			}
		case "d":
			if servers, i, ok := d.selectedServer(); ok {
				d.confirm = components.NewConfirm("delete-server",
					fmt.Sprintf("删除 DNS 服务器 %q？", servers[i].Tag))
				d.confirmID = servers[i].ID
				d.mode = dnsConfirm
			}
		case " ":
			if servers, i, ok := d.selectedServer(); ok {
				if err := d.app.DNS.SetServerEnabled(servers[i].ID, !servers[i].Enabled); err != nil {
					d.err = err
				} else {
					d.app.MarkConfigDirty()
				}
				d.reload()
			}
		case "R", "]":
			d.serverList = d.list
			d.list = d.ruleList
			d.mode = dnsRules
			d.reload()
		case "F":
			d.openOptions()
		case "r":
			d.reload()
		}
		return d, nil
	}
}

// --- 数据加载 ---

// reload 是 DNS 页唯一访问数据库的入口，负责刷新内存缓存后再重建列表。
//
// 它只在 Update 阶段被调用（激活、切 tab、增删改、显式刷新），不参与渲染：
// 这样 View() 的调用链里没有 DB / 文件 / 网络访问，也不会同一次渲染重复查库。
func (d *DNSPage) reload() {
	cfg, err := d.app.DNS.LoadConfig()
	d.cfg = cfg
	if err != nil {
		d.err = err
	}
	// 服务器与规则一次读齐：两个子视图互相切换（[ / ]）与右侧预览都要用，
	// 读一次即可覆盖，不必在切 tab 时再查一遍。
	if servers, err := d.app.DNS.ListServers(); err != nil {
		d.err = err
	} else {
		d.servers = servers
	}
	if rules, err := d.app.DNS.ListRules(); err != nil {
		d.err = err
	} else {
		d.rules = rules
	}
	d.rebuildDNSList()
}

// rebuildDNSList 只基于内存缓存、当前宽度与模式重建列表项，不访问数据库。
//
// 与 reload 的分工是刻意的：resize、切 tab、光标移动都只走这里。
func (d *DNSPage) rebuildDNSList() {
	selected := d.list.SelectedKey()
	mode := d.mode
	switch mode {
	case dnsForm:
		mode = d.formBackMode()
	case dnsOptions:
		mode = d.optionsBack
	case dnsConfirm:
		mode = dnsServers
		if d.confirm.ID == "delete-dns-rule" {
			mode = dnsRules
		}
	}
	if mode == dnsGlobal {
		return
	}
	d.list.Items = nil
	d.list.Keys = nil
	if mode == dnsRules {
		for _, r := range d.rules {
			state := "启用"
			if !r.Enabled {
				state = "停用"
			}
			d.list.Items = append(d.list.Items,
				fmt.Sprintf("%s %s → %s %s", components.Pad(r.Type, 15), components.Pad(r.Value, max(10, d.mainWidth()-40)), r.Server, state))
			d.list.Keys = append(d.list.Keys, fmt.Sprintf("rule:%d", r.ID))
		}
		d.list.SelectKey(selected)
		return
	}
	for _, s := range d.servers {
		state := "启用"
		if !s.Enabled {
			state = "停用"
		}
		final := ""
		if s.Tag == d.cfg.Final {
			final = "  ● 默认"
		}
		resolver := ""
		if s.AddressResolver != "" {
			resolver = " via " + s.AddressResolver
		}
		addr := s.Address
		if s.Type == "local" {
			addr = "（系统）"
		}
		d.list.Items = append(d.list.Items,
			fmt.Sprintf("%-10s %-6s %-28s%s %s%s", s.Tag, s.Type, addr, resolver, state, final))
		d.list.Keys = append(d.list.Keys, fmt.Sprintf("server:%d", s.ID))
	}
	d.list.SelectKey(selected)
}

// selectedServer / selectedRule 读内存缓存，不做任何查询——它们会被
// 键处理与 View 两侧同时调用，每帧重复查库正是本项要消除的问题。
func (d *DNSPage) selectedServer() ([]*config.DNSServer, int, bool) {
	if d.list.Cursor < 0 || d.list.Cursor >= len(d.servers) {
		return nil, 0, false
	}
	return d.servers, d.list.Cursor, true
}

func (d *DNSPage) selectedRule() ([]*config.DNSRule, int, bool) {
	if d.list.Cursor < 0 || d.list.Cursor >= len(d.rules) {
		return nil, 0, false
	}
	return d.rules, d.list.Cursor, true
}

// --- 表单 ---

func (d *DNSPage) openForm(kind string) {
	d.formKind = kind
	switch kind {
	case "add-server":
		d.form = components.NewForm("添加 DNS 服务器",
			[]string{"名称", "类型", "地址", "域名解析 DNS", "出站代理组"},
			[]string{"tag", "type", "address", "resolver", "detour"},
			[]string{"如 remote2", "udp/tcp/tls/https/quic/h3/local", "地址或 URL（local 留空）", "地址为域名时的解析 DNS", "出站 tag，留空直连"})
	case "add-rule":
		d.form = components.NewForm("添加 DNS 规则",
			[]string{"类型", "值", "服务器"},
			[]string{"type", "value", "server"},
			[]string{"domain/domain_suffix/domain_keyword/rule_set", "多值逗号分隔；rule_set 填 Tag", "DNS 服务器 Tag"})
	}
	d.form.Reset()
	d.err = nil
	d.configureForm()
	d.mode = dnsForm
}

func (d *DNSPage) openOptions() {
	d.optionsBack = d.mode
	if d.optionsBack != dnsRules && d.optionsBack != dnsGlobal {
		d.optionsBack = dnsServers
	}
	d.formKind = "options"
	d.form = components.NewForm("DNS 全局选项",
		[]string{"策略", "FakeIP", "FakeIP 网段", "默认(final)"},
		[]string{"strategy", "fakeip", "range", "final"},
		[]string{"prefer_ipv4/prefer_ipv6/ipv4_only/ipv6_only", "true/false", dns.DefaultFakeIPRange, "默认 DNS 服务器 Tag"})
	d.form.SetValueByKey("strategy", d.cfg.Strategy)
	d.form.SetValueByKey("fakeip", fmt.Sprintf("%v", d.cfg.FakeIPEnabled))
	d.form.SetValueByKey("range", d.cfg.FakeIPRange)
	d.form.SetValueByKey("final", d.cfg.Final)
	d.err = nil
	d.configureForm()
	d.mode = dnsOptions
}

func (d *DNSPage) formBackMode() dnsMode {
	if d.formKind == "add-rule" || d.formKind == "edit-rule" {
		return dnsRules
	}
	return dnsServers
}

func (d *DNSPage) submitForm() {
	switch d.formKind {
	case "add-server":
		if _, err := d.app.DNS.AddServer(
			d.form.ValueByKey("tag"), d.form.ValueByKey("type"),
			d.form.ValueByKey("address"), d.form.ValueByKey("resolver"),
			d.form.ValueByKey("detour")); err != nil {
			d.err = err
			return
		}
		d.status = "DNS 服务器已添加"
	case "edit-server":
		servers, i, ok := d.serverByID(d.editID)
		if !ok {
			d.mode = dnsServers
			return
		}
		s := servers[i]
		s.Tag = d.form.ValueByKey("tag")
		s.Type = d.form.ValueByKey("type")
		s.Address = d.form.ValueByKey("address")
		s.AddressResolver = d.form.ValueByKey("resolver")
		s.Detour = d.form.ValueByKey("detour")
		if err := d.app.DNS.UpdateServer(s); err != nil {
			d.err = err
			return
		}
		d.status = "DNS 服务器已保存"
	case "add-rule":
		if _, err := d.app.DNS.AddRule(
			d.form.ValueByKey("type"), d.form.ValueByKey("value"),
			d.form.ValueByKey("server")); err != nil {
			d.err = err
			return
		}
		d.status = "DNS 规则已添加"
	case "edit-rule":
		rules, i, ok := d.ruleByID(d.editID)
		if !ok {
			d.mode = dnsRules
			return
		}
		r := rules[i]
		r.Type = d.form.ValueByKey("type")
		r.Value = d.form.ValueByKey("value")
		r.Server = d.form.ValueByKey("server")
		if err := d.app.DNS.UpdateRule(r); err != nil {
			d.err = err
			return
		}
		d.status = "DNS 规则已保存"
	}
	d.err = nil
	d.mode = d.formBackMode()
	d.app.MarkConfigDirty()
	d.reload()
}

func (d *DNSPage) submitOptions() {
	fakeIP, err := strconv.ParseBool(strings.TrimSpace(d.form.ValueByKey("fakeip")))
	if err != nil {
		d.err = fmt.Errorf("FakeIP 须为 true/false")
		return
	}
	if err := d.app.DNS.SaveOptions(
		d.form.ValueByKey("strategy"), fakeIP,
		d.form.ValueByKey("range"), d.form.ValueByKey("final")); err != nil {
		d.err = err
		return
	}
	d.err = nil
	d.status = "DNS 全局选项已保存"
	d.app.MarkConfigDirty()
	d.mode = d.optionsBack
	d.reload()
}

func (d *DNSPage) onConfirm(msg components.ConfirmMsg) (Page, tea.Cmd) {
	if d.mode != dnsConfirm {
		return d, nil
	}
	d.mode = dnsServers
	if msg.ID == "delete-dns-rule" {
		d.mode = dnsRules
		if msg.Confirmed {
			if err := d.app.DNS.DeleteRule(d.confirmID); err != nil {
				d.err = err
			} else {
				d.err = nil
				d.status = "DNS 规则已删除"
				d.app.MarkConfigDirty()
			}
		}
		d.reload()
		return d, nil
	}
	if msg.Confirmed && msg.ID == "delete-server" {
		if err := d.app.DNS.DeleteServer(d.confirmID); err != nil {
			d.err = err
		} else {
			d.err = nil
			d.status = "DNS 服务器已删除"
			d.app.MarkConfigDirty()
		}
		d.reload()
	}
	return d, nil
}

// --- 渲染 ---

func (d *DNSPage) View() string {
	d.table()
	d.preview = d.selectionDetails()
	if d.detailActive {
		return d.detailsView()
	}
	switch d.mode {
	case dnsForm, dnsOptions:
		return d.formView(&d.form, d.err)
	case dnsConfirm:
		return d.confirmView(&d.confirm)
	case dnsGlobal:
		fakeip := enabledLabel(d.cfg.FakeIPEnabled)
		body := fmt.Sprintf("策略: %s\nFakeIP: %s\n默认 DNS: %s", d.cfg.Strategy, fakeip, orEmpty(d.cfg.Final))
		if d.cfg.FakeIPEnabled {
			body += "\nFakeIP 网段: " + d.cfg.FakeIPRange
		}
		body += "\n\ne / Enter 编辑全局选项；保存后 Ctrl+A 应用。\n[ 返回 DNS 规则 · ] 返回服务器"
		footer := components.Wrap(Hints(d), d.width)
		if feedback := d.statusLine(); feedback != "" {
			footer = components.Wrap(feedback, d.width) + "\n" + footer
		}
		return "服务器  DNS 规则  [全局选项]\n" + components.Fit(components.Wrap(body, d.width), d.width, max(1, d.height-1-len(strings.Split(footer, "\n")))) + "\n" + footer
	case dnsRules:
		return d.listView(&d.list, "服务器  [DNS 规则]  全局选项 · [/] 切换", "暂无规则，a 添加。", d.statusLine(),
			Hints(d))
	default:
		head := fmt.Sprintf("[DNS 服务器]  规则  全局选项 · final:%s", orEmpty(d.cfg.Final))
		return d.listView(&d.list, head, "暂无服务器，a 添加。", d.statusLine(),
			Hints(d))
	}
}

func (d *DNSPage) statusLine() string {
	return d.feedback(d.status, d.err)
}

func orEmpty(s string) string {
	if s == "" {
		return "（未设置）"
	}
	return s
}

func (d *DNSPage) configureForm() {
	if d.formKind == "add-server" || d.formKind == "edit-server" {
		d.form.SetChoices("type", []components.Option{
			{Value: "udp", Label: "UDP · 普通 DNS"}, {Value: "tcp", Label: "TCP · 普通 DNS"},
			{Value: "tls", Label: "TLS · 加密 DNS"}, {Value: "https", Label: "HTTPS · DoH"},
			{Value: "quic", Label: "QUIC · DoQ"}, {Value: "h3", Label: "HTTP/3 · DoH"},
			{Value: "local", Label: "local · 系统解析器"},
		})
		d.form.Field("tag").Validate = required("DNS 名称")
		d.form.Field("address").When = func(f *components.Form) bool { return f.ValueByKey("type") != "local" }
		d.form.Field("address").Validate = required("DNS 地址")
		section(&d.form, "服务器", false, "tag", "type", "address")
		section(&d.form, "解析与出站", true, "resolver", "detour")
		choices := proxyChoices(d.app, false)
		choices[0] = components.Option{Value: "", Label: "DIRECT · 直连"}
		if d.form.ValueByKey("detour") == "direct" || d.form.ValueByKey("detour") == "DIRECT" {
			d.form.SetValueByKey("detour", "")
		}
		d.form.SetChoices("detour", choices)
	} else {
		d.form.SetOptions("type", "domain", "domain_suffix", "domain_keyword", "rule_set")
		configureRuleValue(d.app, &d.form)
	}
	servers, err := d.app.DNS.ListServers()
	if err == nil {
		var choices []components.Option
		for _, server := range servers {
			if server.Enabled {
				choices = append(choices, components.Option{Value: server.Tag, Label: server.Tag + " · " + server.Type})
			}
		}
		d.form.SetChoices("server", choices)
		d.form.SetChoices("final", append([]components.Option{{Value: "", Label: "自动 · 首个可用服务器"}}, choices...))
		resolvers := []components.Option{{Value: "", Label: "自动选择"}}
		for _, server := range servers {
			if server.Enabled && (d.formKind != "edit-server" || server.ID != d.editID) {
				resolvers = append(resolvers, components.Option{Value: server.Tag, Label: server.Tag + " · " + server.Type})
			}
		}
		d.form.SetChoices("resolver", resolvers)
	}
	if d.formKind == "options" {
		d.form.Title = "DNS / [全局选项]"
		d.form.Field("range").When = fieldIs("fakeip", "true")
		d.form.Field("range").Help = "仅启用 FakeIP 时使用；默认 198.18.0.0/15，保存后 Ctrl+A 应用。"
		d.form.Field("fakeip").Help = "使用合成 IP 回答 A/AAAA 查询；默认关闭，Ctrl+A 后生效。"
		d.form.Field("final").Help = "未匹配 DNS 规则时使用的服务器；从已启用服务器中选择。"
	}
	d.form.Begin()
}

func (d *DNSPage) serverByID(id int64) ([]*config.DNSServer, int, bool) {
	for i, s := range d.servers {
		if s.ID == id {
			return d.servers, i, true
		}
	}
	return nil, 0, false
}
func (d *DNSPage) ruleByID(id int64) ([]*config.DNSRule, int, bool) {
	for i, r := range d.rules {
		if r.ID == id {
			return d.rules, i, true
		}
	}
	return nil, 0, false
}
