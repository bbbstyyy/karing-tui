package pages

import "github.com/bbbstyyy/karing-tui/internal/tui/keys"

// AtTop distinguishes ordinary browsing from nested detail/navigation states.
func AtTop(page Page) bool {
	if p, ok := page.(interface{ PreviewFocused() bool }); ok && p.PreviewFocused() {
		return false
	}
	switch p := page.(type) {
	case *Profiles:
		return p.mode == profilesSubs
	case *Groups:
		return p.mode == groupsList
	case *Rules:
		return p.mode == rulesGroups
	case *DNSPage:
		return p.mode == dnsServers
	default:
		return !page.Editing()
	}
}

func HasUnsaved(page Page) bool {
	switch p := page.(type) {
	case *Profiles:
		return p.mode == profilesForm && p.form.Dirty()
	case *Groups:
		return (p.mode == groupsForm && p.form.Dirty()) || p.mode == groupsPick
	case *Rules:
		return p.mode == rulesForm && (p.form.Dirty() || (p.logical != nil && p.logical.dirty()))
	case *DNSPage:
		return (p.mode == dnsForm || p.mode == dnsOptions) && p.form.Dirty()
	case *SettingsPage:
		return (p.editing && p.form.Dirty()) || (p.importMode && p.importForm.Dirty())
	}
	return false
}

// Help is scoped to the current page and mode. Short hints remain in each page.
func Help(page Page) []string {
	lines := []string{"当前页面 · " + page.Title()}
	for _, action := range Actions(page) {
		line := action.Binding().Hint()
		if action.Disabled != "" {
			line += "（" + action.Disabled + "）"
		}
		lines = append(lines, line)
	}
	for _, group := range []struct {
		title    string
		bindings []keys.Binding
	}{{"全局", keys.Global}, {"列表与预览", keys.List}, {"表单与选择器", keys.Form}} {
		lines = append(lines, "", group.title)
		for _, binding := range group.bindings {
			lines = append(lines, binding.Hint())
		}
	}
	return append(lines, "", "搜索：/ 输入；Enter 保留筛选；Esc 撤销本次搜索；c 清除。", "确认默认取消；Tab 选择按钮，Enter 执行。输入中的数字、?、q 保持文本语义。", "配置修改后 Ctrl+A 应用；代理组同时展示保存选择与运行中的节点。")
}
