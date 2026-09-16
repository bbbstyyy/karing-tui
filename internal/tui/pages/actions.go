package pages

import (
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/tui/keys"
	tea "github.com/charmbracelet/bubbletea"
)

// Action metadata is shared by the keyboard-accessible menu and context help.
type Action struct {
	Key, Label, Disabled string
	Aliases              []string
}

func (a Action) Message() tea.KeyMsg {
	return keys.Message(a.Key)
}

func (a Action) Binding() keys.Binding {
	return keys.Binding{Key: a.Message().String(), Label: a.Label, Aliases: a.Aliases}
}

func commandKey(page Page, msg tea.KeyMsg) string {
	if page.Editing() {
		return msg.String()
	}
	for _, action := range Actions(page) {
		binding := action.Binding()
		if binding.Matches(msg.String()) {
			return binding.Key
		}
	}
	return msg.String()
}

func Hints(page Page) string {
	var hints []string
	for _, action := range Actions(page) {
		if action.Disabled == "" {
			hints = append(hints, action.Binding().Hint())
		}
		if len(hints) == 4 {
			break
		}
	}
	return strings.Join(hints, " · ") + " · Ctrl+O 更多 · ? 帮助"
}

func Actions(page Page) []Action {
	var actions []Action
	add := func(key, label string, available bool, reason string) {
		a := Action{Key: key, Label: label}
		if !available {
			a.Disabled = reason
		}
		actions = append(actions, a)
	}
	const empty = "请先选择一个对象"
	switch p := page.(type) {
	case *Dashboard:
		running := p.app.Core.IsRunning()
		add("s", "启动核心", !running && !p.taskActive, "核心已运行或有任务进行中")
		add("x", "停止核心", running && !p.taskActive, "核心未运行或有任务进行中")
		add(keys.Restart, "重新生成并重启", !p.taskActive, "核心任务进行中")
		add("g", "生成并校验", !p.taskActive, "核心任务进行中")
		add("t", "运行中的代理组测速", running && !p.testBusy, "核心未运行或正在测速")
		add("a", "前往订阅与节点", true, "")
	case *Profiles:
		if p.mode == profilesResults {
			add("Enter", "查看完整结果", len(p.results) > 0, "暂无结果")
			break
		}
		_, selected := p.list.Selected()
		if p.mode == profilesSubs {
			add("a", "添加订阅", true, "")
			add("e", "编辑订阅", selected, empty)
			add("Enter", "查看订阅节点", selected, empty)
			add("Alt+Enter", "查看订阅详情", selected, empty)
			add("u", "更新选中订阅", selected && !p.busy, "请选择订阅并等待当前任务完成")
			add("U", "更新所有启用订阅", !p.busy && len(p.subs) > 0, "没有订阅或任务进行中")
			add("v", "查看逐项更新结果", len(p.results) > 0, "暂无结果")
			add(keys.Retry, "仅重试失败项", len(failedIDs(p.results)) > 0 && !p.busy, "没有失败项或任务进行中")
			add("n", "查看全部节点", true, "")
		} else {
			add("Enter", "查看节点详情", selected, empty)
			add("/", "搜索节点", true, "")
			add("c", "清除搜索和过滤", true, "")
			add("i", "导入分享链接", true, "")
			add("a", "添加手动节点", true, "")
			n, ok := p.selectedNode()
			manual := ok && n.SubscriptionID == 0
			add("e", "编辑手动节点", manual, "订阅节点由订阅管理")
			add(keys.Test, "测速选中节点", selected && !p.busy, "请选择节点并等待当前任务完成")
			actions[len(actions)-1].Aliases = []string{"s"}
			add(keys.TestAll, "测速当前结果列表", selected && !p.busy, "没有节点或任务进行中")
			actions[len(actions)-1].Aliases = []string{"A"}
			add("p", "切换协议过滤", true, "")
			add("S", "切换订阅过滤", true, "")
			add("o", "切换排序", true, "")
			add(keys.Previous, "返回订阅", true, "")
		}
		add("Space", "启用/停用", selected, empty)
		canDelete := selected
		reason := empty
		if p.mode == profilesNodes {
			n, ok := p.selectedNode()
			canDelete = ok && n.SubscriptionID == 0
			reason = "仅手动节点可删除"
		}
		add("d", "删除选中对象", canDelete, reason)
	case *Groups:
		switch p.mode {
		case groupsList:
			_, ok := p.selectedGroup()
			add("a", "新建代理组", true, "")
			add("Enter", "查看成员", ok, empty)
			add("Alt+Enter", "查看代理组详情", ok, empty)
			add("e", "编辑代理组", ok, empty)
			add("d", "删除代理组", ok, empty)
		case groupsDetail:
			_, ok := p.curMember()
			add("Enter", "查看成员详情", ok, empty)
			add("m", "勾选组成员", p.cur != nil, empty)
			add("Space", "设为保存选择", ok && p.cur.Type == "select", "URLTest 自动选择，或没有可用成员")
			add(keys.Test, "测速组内节点", !p.busy, "正在测速")
			actions[len(actions)-1].Aliases = []string{"s"}
		}
	case *Rules:
		_, ok := p.list.Selected()
		switch p.mode {
		case rulesGroups:
			add("Enter", "查看组内规则", ok, empty)
			add("Alt+Enter", "查看分流组详情", ok, empty)
			add("a", "新建分流组", true, "")
			add("e", "编辑目标、名称与层", ok, empty)
			add("d", "删除分流组", ok, empty)
			add("J", "层内下移（降低层内优先级）", ok, empty)
			add("K", "层内上移（提高层内优先级）", ok, empty)
			add("m", "移动到其他层（改变优先级归属）", ok, empty)
			add("f", "仅显示启用 / 显示全部", true, "")
			add("P", "导入 / 恢复地区预置", !p.busy, "任务进行中")
			add(keys.Next, "管理规则集", true, "")
			actions[len(actions)-1].Aliases = []string{"R"}
		case rulesMove:
			add("Enter", "移动到选中的层", true, "")
			add("Esc", "取消移动", true, "")
		case rulesGroupRl:
			add("Enter", "查看规则全文", ok, empty)
			add("a", "添加规则", true, "")
			add("e", "编辑规则", ok, empty)
			add("d", "删除规则", ok, empty)
			add("J", "下移规则", ok, empty)
			add("K", "上移规则", ok, empty)
			add("c", "从分类库添加", true, "")
		case rulesSets:
			_, custom := p.selectedSet()
			add("Enter", "查看规则集详情", ok, empty)
			add("a", "添加规则集", true, "")
			add("d", "删除自定义规则集", custom, "内置分类只读")
			add("c", "浏览分类库", true, "")
			add("u", "下载选中规则集", ok && !p.busy, "没有选择或正在下载")
			add("U", "更新全部规则集", !p.busy, "正在下载")
			add(keys.Previous, "返回分流组", true, "")
		case rulesCatalog:
			add("/", "搜索分类", true, "")
			if p.catBack == rulesGroupRl {
				add("Enter", "加入当前分流组", ok, empty)
			} else {
				add("Enter", "查看分类详情", ok, empty)
			}
			add("u", "下载缓存", ok && !p.busy, "没有选择或正在下载")
			add("Alt+Enter", "查看分类详情", ok, empty)
			add("c", "清除分类搜索", true, "")
			add(keys.Next, "切换分类种类", true, "")
		}
	case *DNSPage:
		if p.mode == dnsGlobal {
			add("e", "编辑 DNS 全局选项", true, "")
			add("Enter", "编辑 DNS 全局选项", true, "")
			add(keys.Next, "切换 DNS 子页签", true, "")
			break
		}
		_, ok := p.list.Selected()
		add("Enter", "查看完整详情", ok, empty)
		add("a", "添加", true, "")
		add("e", "编辑", ok, empty)
		add("d", "删除", ok, empty)
		add("Space", "启用/停用", ok, empty)
		if p.mode == dnsRules {
			add("J", "下移规则", ok, empty)
			add("K", "上移规则", ok, empty)
		}
		add("F", "全局 DNS 选项", true, "")
		add(keys.Next, "切换 DNS 子页签", true, "")
		if p.mode == dnsServers {
			actions[len(actions)-1].Aliases = []string{"R"}
		}
	case *LogsPage:
		add("/", "搜索日志", true, "")
		add("f", "切换级别过滤", true, "")
		add("c", "清除过滤", true, "")
		add("G", "跟随最新日志", true, "")
		add("]", "切换日志来源", true, "")
	case *SettingsPage:
		selected := p.list.SelectedKey()
		add("e", "编辑当前设置", p.section != 3 && settingEditable(selected), "请选择需要编辑的设置")
		add("E", "编辑全部设置", true, "")
		add("Enter", "打开当前项", true, "")
		add("Alt+Enter", "查看完整说明", true, "")
		add(keys.Next, "切换设置分组", true, "")
		add("b", "导出备份", !p.busy, "备份任务进行中")
		add("i", "预检并恢复备份", !p.busy, "备份任务进行中")
	}
	if task, ok := page.(interface{ TaskActions() (bool, bool) }); ok {
		results, retry := task.TaskActions()
		addOnce := func(key, label string, available bool) {
			for _, a := range actions {
				if a.Key == key {
					return
				}
			}
			if available {
				add(key, label, true, "")
			}
		}
		addOnce(keys.Results, "查看逐项任务结果", results)
		addOnce(keys.Retry, "仅重试失败项", retry)
	}
	for i := range actions {
		if actions[i].Key == keys.Delete {
			actions[i].Aliases = append(actions[i].Aliases, "D")
		}
	}
	actions = append(actions, Action{Key: keys.Refresh, Label: "刷新当前页面"})
	actions = append(actions, Action{Key: "Ctrl+A", Label: "应用最新保存的配置"})
	if !AtTop(page) {
		actions = append(actions, Action{Key: "Esc", Label: "返回上一层"})
	}
	return actions
}
