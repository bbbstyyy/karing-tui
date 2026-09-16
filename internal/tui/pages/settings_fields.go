package pages

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/redact"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
)

type settingField struct{ key, label, group, defaultValue, help string }

var settingGroups = []string{"监听与访问", "下载与订阅", "分流行为", "备份恢复"}
var settingFields = []settingField{
	{"mixed_port", "mixed 监听端口", "监听与访问", "2080", "HTTP/SOCKS 代理入口；Ctrl+A 后生效。"},
	{"allow_lan", "允许局域网访问", "监听与访问", "关闭", "开启后监听全部网卡；仅允许可信网络访问，Ctrl+A 后生效。"},
	{"clash_api_port", "Clash API 端口", "监听与访问", "9090", "运行状态与测速接口；0 关闭，Ctrl+A 后生效。"},
	{"clash_api_secret", "Clash API 密钥", "监听与访问", "空", "保护 API 访问；默认遮罩，Ctrl+R 显隐，Ctrl+A 后生效。"},
	{"log_level", "核心日志级别", "监听与访问", "info", "控制核心日志详细程度；Ctrl+A 后生效。"},
	{"download_proxy", "下载代理", "下载与订阅", "空（直连）", "订阅、规则集与核心下载使用此代理；不读取环境代理，保存后用于新下载。"},
	{"auto_update_minutes", "自动更新间隔（分钟）", "下载与订阅", "0（关闭）", "周期更新启用订阅；间隔修改需重启应用，更新结果仍需 Ctrl+A 应用。"},
	{"private_direct", "内网直连", "分流行为", "开启", "内网 IP 直接连接，不需要额外 DNS 解析；Ctrl+A 后生效。"},
	{"resolve_ip_rules", "IP 规则解析域名", "分流行为", "关闭", "开启后先解析域名匹配 IP 规则；代理侧收到 IP，解析失败会中断连接。Ctrl+A 后生效。"},
	{"routing_layers", "分流层序", "分流行为", "只读", "优先级 = (层序, 层内序号, ID)：自定义分流组 < GeoSite < GeoIP < ACL < final。预置方案的全部组都在「自定义分流组」层。"},
	{"routing_preset", "默认分流方案", "分流行为", "预置 cn", "中国大陆地区预置：27 组（6 个默认启用）+ final 兜底组。Rules 页按 P 可重新导入/恢复；已装方案不会被自动改写。"},
	{"routing_final", "final 兜底出口", "分流行为", "Manual", "未匹配流量走 Manual（select）。在 Manual 中没有选择节点时走成员列表第一个节点（订阅中排序第一个），不是不走代理。"},
}

func configureSettingsForm(f *components.Form) {
	for _, spec := range settingFields {
		if !settingEditable(spec.key) {
			continue // 只读说明项，不进编辑表单
		}
		field := f.Field(spec.key)
		field.Label, field.Section = spec.label, spec.group
		field.Help = "默认: " + spec.defaultValue + "。" + spec.help
	}
	f.Field("mixed_port").Validate = integer("mixed 端口", 1, 65535, false)
	f.Field("clash_api_port").Validate = integer("Clash API 端口", 0, 65535, false)
	f.Field("auto_update_minutes").Validate = integer("自动更新间隔", 0, config.MaxAutoUpdateMinutes, false)
	f.Field("download_proxy").Validate = func(value string) error {
		if strings.TrimSpace(value) == "" {
			return nil
		}
		u, err := url.Parse(value)
		if err != nil || u.Hostname() == "" {
			return fmt.Errorf("下载代理格式错误，请填写 HTTP/HTTPS/SOCKS5 URL 或留空直连")
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return fmt.Errorf("下载代理仅支持 HTTP/HTTPS/SOCKS5")
		}
		if u.Port() != "" {
			return integer("代理端口", 1, 65535, false)(u.Port())
		}
		return nil
	}
}

func (s *SettingsPage) settingValues() map[string]string {
	set := s.app.GetSettings()
	final := s.finalRoutingInfo()
	return map[string]string{
		"mixed_port": strconv.Itoa(set.MixedPort), "allow_lan": strconv.FormatBool(set.AllowLAN),
		"clash_api_port": strconv.Itoa(set.ClashAPIPort), "clash_api_secret": set.ClashAPISecret,
		"log_level": set.LogLevel, "download_proxy": set.DownloadProxy, "auto_update_minutes": strconv.Itoa(set.AutoUpdateMinutes),
		"private_direct": strconv.FormatBool(set.PrivateDirect), "resolve_ip_rules": strconv.FormatBool(set.ResolveIPRules),
		// 只读说明项：反映当前分流状态，不进编辑表单
		"routing_layers": "custom < geosite < geoip < acl < final",
		"routing_preset": final.preset,
		"routing_final":  final.target,
	}
}

// routingInfo 汇总只读说明项要显示的分流状态。
type routingInfo struct{ preset, target string }

// finalRoutingInfo 读取 final 兜底组的目标与预置来源标记。
// 数据库不可达或没有 final 组时给出缺省说明，不影响设置页其余部分。
func (s *SettingsPage) finalRoutingInfo() routingInfo {
	info := routingInfo{preset: "预置 cn", target: "Manual"}
	groups, err := s.app.DB.ListRoutingGroups()
	if err != nil {
		return info
	}
	for _, g := range groups {
		if config.KindNormalize(g.Kind) == config.KindFinal {
			info.target = g.Target
		}
	}
	return info
}

// settingReadOnlyKeys 分流行为分组里的只读说明项：它们描述当前分流状态，
// 不进编辑表单——否则「编辑全部」会把这些说明当成输入，覆盖真实设置。
var settingReadOnlyKeys = map[string]bool{"routing_layers": true, "routing_preset": true, "routing_final": true}

// settingEditable 报告设置项是否可编辑。
func settingEditable(key string) bool { return !settingReadOnlyKeys[key] }

func settingValue(key, value string) string {
	switch key {
	case "clash_api_secret":
		if value != "" {
			return "******（已设置）"
		}
		return "未设置"
	case "allow_lan", "private_direct", "resolve_ip_rules":
		if value == "true" {
			return "[x] 开启"
		}
		return "[ ] 关闭"
	case "download_proxy":
		if value == "" {
			return "直连"
		}
		return redact.Text(value)
	}
	return value
}

func (s *SettingsPage) reloadSettings() {
	values := s.settingValues()
	var rows [][]string
	var ids []string
	if s.section == 3 {
		rows = [][]string{{"导出备份", "b · 新建 ZIP 快照"}, {"恢复备份", "i · 先预检，再确认覆盖"}, {"数据目录", "Enter 查看完整路径"}}
		ids = []string{"backup", "restore", "paths"}
	} else {
		for _, spec := range settingFields {
			if spec.group == settingGroups[s.section] {
				rows = append(rows, []string{spec.label, settingValue(spec.key, values[spec.key])})
				ids = append(ids, spec.key)
			}
		}
	}
	s.list.SetTable([]components.Column{col("设置", 22, 0, false), col("当前值", 30, 0, false)}, rows, ids)
}

func (s *SettingsPage) settingsPreview() string {
	key := s.list.SelectedKey()
	for _, spec := range settingFields {
		if spec.key == key {
			return spec.label + "\n当前: " + settingValue(key, s.settingValues()[key]) + "\n默认: " + spec.defaultValue + "\n\n" + spec.help + "\n\ne / Enter 编辑当前项 · E 编辑全部"
		}
	}
	switch key {
	case "backup":
		return "导出数据库与生成配置的 ZIP 快照。\n包含订阅和节点凭据，请妥善保存。\n按 b 导出，v 查看结果。"
	case "restore":
		return "选择备份后先校验归档、数据库和迁移。\n预检通过才确认停机恢复；成功后退出应用。\n按 i 选择备份文件。"
	case "paths":
		return fmt.Sprintf("数据库: %s\n运行目录: %s\n缓存目录: %s\n日志目录: %s\n核心路径: %s", s.app.Paths.DB, s.app.Paths.Runtime, s.app.Paths.Cache, s.app.Paths.Logs, s.app.Paths.CoreBin)
	}
	return ""
}

func (s *SettingsPage) editSetting(key string) {
	if key != "" && !settingEditable(key) {
		return
	}
	s.form = newSettingsForm()
	configureSettingsForm(&s.form)
	s.loadForm()
	if key != "" {
		for i := range s.form.Fields {
			visible := s.form.Fields[i].Key == key
			s.form.Fields[i].When = func(*components.Form) bool { return visible }
		}
		s.form.FocusKey(key)
	}
	s.form.Begin()
	s.editing = true
	s.status, s.err = "", nil
}

func (s *SettingsPage) settingsView() string {
	s.reloadSettings()
	s.preview = s.settingsPreview()
	tabs := append([]string(nil), settingGroups...)
	tabs[s.section] = "[" + tabs[s.section] + "]"
	head := strings.Join(tabs, "  ") + " · [/] 切换"
	return s.listView(&s.list, head, "暂无设置", s.feedback(s.status, s.err), Hints(s))
}
