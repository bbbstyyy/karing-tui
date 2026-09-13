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
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
)

// dnsMode DNS 页的视图模式。
type dnsMode int

const (
	dnsServers dnsMode = iota // DNS 服务器列表
	dnsRules                  // DNS 规则列表
	dnsOptions                // 全局选项表单（策略/FakeIP/final）
	dnsForm                   // 服务器/规则表单
	dnsConfirm                // 删除确认
)

// DNSPage DNS 配置页：服务器管理、DNS 规则、FakeIP 与策略。
type DNSPage struct {
	base
	mode   dnsMode
	cfg    config.DNSConfig
	err    error
	status string

	list components.SimpleList

	form     components.Form
	formKind string // add-server / edit-server / add-rule / edit-rule
	editID   int64

	confirm   components.Confirm
	confirmID int64
}

// NewDNS 创建 DNS 页。
func NewDNS(app *application.App) *DNSPage {
	return &DNSPage{base: base{app: app}}
}

func (d *DNSPage) Title() string { return "DNS" }

// Editing 表单输入状态时拦截全局键位。
func (d *DNSPage) Editing() bool { return d.mode == dnsForm || d.mode == dnsOptions }

func (d *DNSPage) Init() tea.Cmd { return nil }

func (d *DNSPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		d.SetSize(msg.Width, msg.Height)
		return d, nil
	case ActivateMsg:
		d.reload()
		return d, nil
	case components.ConfirmMsg:
		return d.onConfirm(msg)
	case tea.KeyMsg:
		return d.handleKey(msg)
	}
	return d, nil
}

func (d *DNSPage) handleKey(msg tea.KeyMsg) (Page, tea.Cmd) {
	key := msg.String()
	switch d.mode {
	case dnsForm:
		switch key {
		case "esc":
			d.mode = d.formBackMode()
			return d, nil
		case "enter":
			d.submitForm()
			return d, nil
		}
		d.form.Update(msg)
		return d, nil

	case dnsOptions:
		switch key {
		case "esc":
			d.mode = dnsServers
			return d, nil
		case "enter":
			d.submitOptions()
			return d, nil
		}
		d.form.Update(msg)
		return d, nil

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
				d.mode = dnsForm
			}
		case "D":
			if rules, i, ok := d.selectedRule(); ok {
				if err := d.app.DNS.DeleteRule(rules[i].ID); err != nil {
					d.err = err
				} else {
					d.err = nil
					d.status = "DNS 规则已删除"
				}
				d.reload()
			}
		case " ":
			if rules, i, ok := d.selectedRule(); ok {
				if err := d.app.DNS.SetRuleEnabled(rules[i].ID, !rules[i].Enabled); err != nil {
					d.err = err
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
				}
				d.reload()
			}
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
					[]string{"Tag", "类型", "地址", "AddressResolver", "Detour"},
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
				}
				d.reload()
			}
		case "R":
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

func (d *DNSPage) reload() {
	cfg, err := d.app.DNS.LoadConfig()
	d.cfg, d.err = cfg, err
	if d.mode == dnsRules {
		rules, err := d.app.DNS.ListRules()
		if err != nil {
			d.err = err
		}
		d.list.Items = nil
		for _, r := range rules {
			state := "启用"
			if !r.Enabled {
				state = "停用"
			}
			d.list.Items = append(d.list.Items,
				fmt.Sprintf("%-15s %-36s → %-10s %s", r.Type, r.Value, r.Server, state))
		}
		d.clamp(len(rules))
		return
	}
	servers, err := d.app.DNS.ListServers()
	if err != nil {
		d.err = err
	}
	d.list.Items = nil
	for _, s := range servers {
		state := "启用"
		if !s.Enabled {
			state = "停用"
		}
		final := ""
		if s.Tag == cfg.Final {
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
	}
	d.clamp(len(servers))
}

func (d *DNSPage) selectedServer() ([]*config.DNSServer, int, bool) {
	servers, err := d.app.DNS.ListServers()
	if err != nil || d.list.Cursor < 0 || d.list.Cursor >= len(servers) {
		return nil, 0, false
	}
	return servers, d.list.Cursor, true
}

func (d *DNSPage) selectedRule() ([]*config.DNSRule, int, bool) {
	rules, err := d.app.DNS.ListRules()
	if err != nil || d.list.Cursor < 0 || d.list.Cursor >= len(rules) {
		return nil, 0, false
	}
	return rules, d.list.Cursor, true
}

func (d *DNSPage) clamp(n int) {
	if d.list.Cursor >= n {
		d.list.Cursor = n - 1
	}
	if d.list.Cursor < 0 {
		d.list.Cursor = 0
	}
}

// --- 表单 ---

func (d *DNSPage) openForm(kind string) {
	d.formKind = kind
	switch kind {
	case "add-server":
		d.form = components.NewForm("添加 DNS 服务器",
			[]string{"Tag", "类型", "地址", "AddressResolver", "Detour"},
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
	d.mode = dnsForm
}

func (d *DNSPage) openOptions() {
	d.form = components.NewForm("DNS 全局选项",
		[]string{"策略", "FakeIP", "FakeIP 网段", "默认(final)"},
		[]string{"strategy", "fakeip", "range", "final"},
		[]string{"prefer_ipv4/prefer_ipv6/ipv4_only/ipv6_only", "true/false", dns.DefaultFakeIPRange, "默认 DNS 服务器 Tag"})
	d.form.SetValueByKey("strategy", d.cfg.Strategy)
	d.form.SetValueByKey("fakeip", fmt.Sprintf("%v", d.cfg.FakeIPEnabled))
	d.form.SetValueByKey("range", d.cfg.FakeIPRange)
	d.form.SetValueByKey("final", d.cfg.Final)
	d.err = nil
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
		servers, i, ok := d.selectedServer()
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
		rules, i, ok := d.selectedRule()
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
	d.mode = dnsServers
	d.reload()
}

func (d *DNSPage) onConfirm(msg components.ConfirmMsg) (Page, tea.Cmd) {
	if d.mode != dnsConfirm {
		return d, nil
	}
	d.mode = dnsServers
	if msg.Confirmed && msg.ID == "delete-server" {
		if err := d.app.DNS.DeleteServer(d.confirmID); err != nil {
			d.err = err
		} else {
			d.err = nil
			d.status = "DNS 服务器已删除"
		}
		d.reload()
	}
	return d, nil
}

// --- 渲染 ---

func (d *DNSPage) View() string {
	switch d.mode {
	case dnsForm:
		return d.form.View()
	case dnsOptions:
		return d.form.View()
	case dnsConfirm:
		return d.confirm.View()
	case dnsRules:
		head := styles.Title.Render("DNS 规则") + "\n"
		body := d.list.View("暂无 DNS 规则。")
		return head + body + d.statusLine() +
			styles.Dim.Render("\na 添加 · e 编辑 · D 删除 · space 启停 · J 下移 · K 上移 · esc 返回")
	default:
		head := styles.Title.Render("DNS 服务器") +
			styles.Dim.Render(fmt.Sprintf("  [策略:%s · FakeIP:%s · final:%s]",
				d.cfg.Strategy, fmt.Sprintf("%v", d.cfg.FakeIPEnabled), orEmpty(d.cfg.Final)))
		body := d.list.View("暂无 DNS 服务器。")
		return head + "\n" + body + d.statusLine() +
			styles.Dim.Render("\na 添加 · e 编辑 · d 删除 · space 启停 · R DNS 规则 · F 全局选项 · r 刷新")
	}
}

func (d *DNSPage) statusLine() string {
	var b string
	if d.status != "" {
		b += "\n" + styles.Ok.Render(d.status)
	}
	if d.err != nil {
		b += "\n" + styles.Err.Render("错误: "+d.err.Error())
	}
	return b
}

func orEmpty(s string) string {
	if s == "" {
		return "（未设置）"
	}
	return s
}
