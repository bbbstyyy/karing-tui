package pages

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/headless"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
)

// SettingsPage 展示并编辑应用设置，并提供备份导出/导入。
type SettingsPage struct {
	base
	form    components.Form
	editing bool

	// 导入备份：单字段表单填路径 → 确认弹窗 → 恢复后退出应用
	importForm components.Form
	importMode bool
	importPath string
	prepared   *application.PreparedRestore
	confirm    components.Confirm
	restoring  bool
	busy       bool
	status     string
	err        error
	section    int
	list       components.SimpleList
}

// NewSettings 创建 Settings 页。
func NewSettings(app *application.App) *SettingsPage {
	p := &SettingsPage{base: base{app: app}}
	p.form = newSettingsForm()
	configureSettingsForm(&p.form)
	return p
}

func (s *SettingsPage) Title() string { return "设置" }

// Editing 处于表单编辑状态时拦截全局键位。
func (s *SettingsPage) Editing() bool {
	return s.detailActive || s.editing || s.importMode || s.confirm.Active || s.restoring
}

func (s *SettingsPage) Init() tea.Cmd { return nil }

// newSettingsForm 构建设置编辑表单。
func newSettingsForm() components.Form {
	return components.NewForm("编辑设置",
		[]string{
			"mixed 监听端口",
			"允许局域网 (true/false)",
			"下载代理（留空直连）",
			"核心日志级别 (debug/info/warn/error)",
			"Clash API 端口 (0 关闭)",
			"Clash API 密钥",
			"订阅自动更新间隔（分钟，0 关闭）",
			"内网直连 (true/false)",
			"IP 规则解析域名 (true/false)",
		},
		[]string{"mixed_port", "allow_lan", "download_proxy", "log_level", "clash_api_port", "clash_api_secret", "auto_update_minutes", "private_direct", "resolve_ip_rules"},
		[]string{"2080", "false", "http://127.0.0.1:7890", "info", "9090", "留空不鉴权", "0", "true", "false（开启后代理侧收到 IP 而非域名）"},
	)
}

// loadForm 把当前设置填入表单。
func (s *SettingsPage) loadForm() {
	set := s.app.GetSettings()
	s.form.SetValueByKey("mixed_port", strconv.Itoa(set.MixedPort))
	s.form.SetValueByKey("allow_lan", strconv.FormatBool(set.AllowLAN))
	s.form.SetValueByKey("download_proxy", set.DownloadProxy)
	s.form.SetValueByKey("log_level", set.LogLevel)
	s.form.SetValueByKey("clash_api_port", strconv.Itoa(set.ClashAPIPort))
	s.form.SetValueByKey("clash_api_secret", set.ClashAPISecret)
	s.form.SetValueByKey("auto_update_minutes", strconv.Itoa(set.AutoUpdateMinutes))
	s.form.SetValueByKey("private_direct", strconv.FormatBool(set.PrivateDirect))
	s.form.SetValueByKey("resolve_ip_rules", strconv.FormatBool(set.ResolveIPRules))
}

func (s *SettingsPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	if !s.Editing() {
		if handled, cmd := s.handleRetry(msg); handled {
			return s, cmd
		}
	}
	if s.detailActive || !s.Editing() {
		if s.handleDetails(msg, s.err) {
			return s, nil
		}
	}
	if msg, ok := msg.(tea.WindowSizeMsg); ok {
		s.SetSize(msg.Width, msg.Height)
		return s, nil
	}
	switch msg := msg.(type) {
	case ActivateMsg:
		s.reloadSettings()
		return s, nil
	case components.ConfirmMsg:
		return s.onConfirm(msg)
	case actionDoneMsg:
		if !s.accept(msg) {
			if msg.Restore != nil {
				_ = msg.Restore.Close()
			}
			return s, nil
		}
		s.busy = false
		s.restoring = false
		s.err = msg.Err
		if msg.Err != nil {
			s.status = ""
			return s, nil
		}
		switch msg.Action {
		case "restore":
			s.status = "备份已恢复，请重新启动 karing"
			return s, tea.Quit
		case "backup":
			s.status = "备份已导出: " + msg.Data
		case "restore-preflight":
			s.prepared = msg.Restore
			s.confirm = components.NewConfirm("restore-backup", fmt.Sprintf("预检通过：%d 个订阅，%d 个节点。用 %s 覆盖当前全部数据？确认后停止核心并恢复，应用会退出，请重新启动。", s.prepared.Subscriptions, s.prepared.Nodes, s.importPath))
		}
		return s, nil
	}

	key, ok := msg.(tea.KeyMsg)
	if !ok {
		if s.importMode {
			return s, s.importForm.Update(msg)
		}
		if s.editing {
			return s, s.form.Update(msg)
		}
		return s, nil
	}

	// 恢复完成后锁定输入（应用即将退出）
	if s.restoring {
		return s, nil
	}
	if s.confirm.Active {
		if consumed, cmd := s.confirm.Update(msg); consumed {
			return s, cmd
		}
		return s, nil
	}
	if s.importMode {
		action, cmd := s.importForm.Handle(msg)
		switch action {
		case "cancel":
			s.importMode = false
			s.err = nil
		case "save":
			return s, s.submitImportPath()
		}
		return s, cmd
	}
	if s.editing {
		action, cmd := s.form.Handle(msg)
		switch action {
		case "cancel":
			s.editing = false
			s.err = nil
		case "save":
			s.submit()
		}
		return s, cmd
	}
	if consumed, cmd := s.list.Update(msg); consumed {
		return s, cmd
	}

	switch key.String() {
	case "e", "enter":
		selected := s.list.SelectedKey()
		if selected == "backup" {
			return s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
		}
		if selected == "restore" {
			return s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
		}
		if selected == "paths" {
			s.openDetails("数据目录", s.settingsPreview())
			return s, nil
		}
		if !settingEditable(selected) {
			// 分流说明项只读：Enter 查看完整说明，避免「空表单保存」覆盖真实设置
			s.openDetails("设置详情", s.settingsPreview())
			return s, nil
		}
		s.editSetting(selected)
	case "E":
		s.editSetting("")
	case "alt+enter":
		s.openDetails("设置详情", s.settingsPreview())
	case "[", "]":
		delta := 1
		if key.String() == "[" {
			delta = -1
		}
		s.section = (s.section + delta + len(settingGroups)) % len(settingGroups)
		s.reloadSettings()
	case "b":
		if !s.busy {
			s.busy = true
			s.status, s.err = "正在导出备份…", nil
			return s, s.exportBackup()
		}
	case "i":
		if s.importForm.Fields == nil {
			s.importForm = components.NewForm("从备份恢复",
				[]string{"备份文件路径"},
				[]string{"path"},
				[]string{s.app.Paths.Root + "/backups/karing-backup-….zip"},
			)
		}
		s.importForm.Reset()
		s.importMode = true
		s.status, s.err = "", nil
	case "r":
		s.reloadSettings()
	}
	return s, nil
}

// submitImportPath 校验路径并弹出确认。
func (s *SettingsPage) submitImportPath() tea.Cmd {
	path := strings.TrimSpace(s.importForm.ValueByKey("path"))
	if path == "" {
		s.err = fmt.Errorf("请填写备份文件路径")
		return nil
	}
	if s.busy {
		s.err = fmt.Errorf("请等待当前备份任务完成")
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		s.err = fmt.Errorf("备份文件不可读: %w", err)
		return nil
	}
	s.importPath = path
	s.importMode = false
	s.busy = true
	s.status, s.err = "正在预检归档、数据库与迁移…", nil
	s.retryTask = func() tea.Cmd { s.importForm.SetValueByKey("path", path); return s.submitImportPath() }
	return s.task("备份预检", func() tea.Msg {
		prepared, err := application.PrepareRestore(s.app.Paths, path)
		return actionDoneMsg{Action: "restore-preflight", Restore: prepared, Err: err}
	})
}

func (s *SettingsPage) onConfirm(msg components.ConfirmMsg) (Page, tea.Cmd) {
	s.confirm.Active = false
	if msg.ID != "restore-backup" {
		return s, nil
	}
	prepared := s.prepared
	s.prepared = nil
	if prepared == nil {
		s.err = fmt.Errorf("备份预检结果已失效，请重新选择备份")
		return s, nil
	}
	if !msg.Confirmed {
		_ = prepared.Close()
		s.status = "已取消恢复，当前数据和核心保持不变"
		return s, nil
	}
	s.restoring, s.busy = true, true
	return s, s.startTask("备份恢复", 0, false, func(func(itemResult)) tea.Msg {
		defer prepared.Close()
		serviceLock, err := headless.Acquire(s.app.Paths)
		if err != nil {
			return actionDoneMsg{Action: "restore", Err: fmt.Errorf("无法恢复: %w（请先停止 headless 服务）", err)}
		}
		defer serviceLock.Close()
		ctx, cancel := context.WithTimeout(s.app.BackgroundContext(), 30*time.Second)
		defer cancel()
		err = s.app.RestorePrepared(ctx, prepared)
		if err != nil {
			s.app.AppLog.AppendLine("备份恢复失败: " + err.Error())
		}
		return actionDoneMsg{Action: "restore", Err: err}
	})
}

// exportBackup 异步导出备份。
func (s *SettingsPage) exportBackup() tea.Cmd {
	s.busy = true
	s.retryTask = s.exportBackup
	return s.taskN("备份导出", 1, func(report func(itemResult)) tea.Msg {
		dest, err := s.app.Backup("")
		result := itemResult{Name: "当前配置与数据库", State: "成功", Detail: dest}
		if err != nil {
			result.State, result.Detail = "失败", err.Error()
		}
		report(result)
		return actionDoneMsg{Action: "backup", Data: dest, Err: err, Results: []itemResult{result}}
	})
}

// submit 解析表单并保存设置。
func (s *SettingsPage) submit() {
	set := s.app.GetSettings()

	v := func(key string) string { return strings.TrimSpace(s.form.ValueByKey(key)) }
	mixedPort, err := strconv.Atoi(v("mixed_port"))
	if err != nil || mixedPort <= 0 || mixedPort > 65535 {
		s.err = fmt.Errorf("mixed 监听端口须为 1-65535")
		return
	}
	allowLAN, err := strconv.ParseBool(v("allow_lan"))
	if err != nil {
		s.err = fmt.Errorf("允许局域网须为 true 或 false")
		return
	}
	logLevel := v("log_level")
	if !validLogLevel(logLevel) {
		s.err = fmt.Errorf("日志级别须为 debug/info/warn/error")
		return
	}
	clashPort, err := strconv.Atoi(v("clash_api_port"))
	if err != nil || clashPort < 0 || clashPort > 65535 {
		s.err = fmt.Errorf("端口须为 0-65535（Clash API）")
		return
	}
	autoUpdate, err := strconv.Atoi(v("auto_update_minutes"))
	if err != nil || autoUpdate < 0 || autoUpdate > config.MaxAutoUpdateMinutes {
		s.err = fmt.Errorf("自动更新间隔须为 0-%d 分钟", config.MaxAutoUpdateMinutes)
		return
	}
	privateDirect, err := strconv.ParseBool(v("private_direct"))
	if err != nil {
		s.err = fmt.Errorf("内网直连须为 true 或 false")
		return
	}
	resolveIPRules, err := strconv.ParseBool(v("resolve_ip_rules"))
	if err != nil {
		s.err = fmt.Errorf("IP 规则解析域名须为 true 或 false")
		return
	}

	set.MixedPort = mixedPort
	set.AllowLAN = allowLAN
	set.DownloadProxy = v("download_proxy")
	set.LogLevel = logLevel
	set.ClashAPIPort = clashPort
	set.ClashAPISecret = v("clash_api_secret")
	set.AutoUpdateMinutes = autoUpdate
	set.PrivateDirect = privateDirect
	set.ResolveIPRules = resolveIPRules

	if err := s.app.DB.SaveSettings(set); err != nil {
		s.err = err
		return
	}
	s.app.SetSettings(set)
	s.app.Bin.SetProxy(set.DownloadProxy)
	s.editing = false
	s.err = nil
	s.app.MarkConfigDirty()
	s.status = "设置已保存；Ctrl+A 应用核心配置。自动更新间隔需重启应用生效。"
}

func validLogLevel(l string) bool {
	switch l {
	case "debug", "info", "warn", "error":
		return true
	}
	return false
}

func (s *SettingsPage) View() string {
	if s.detailActive {
		return s.detailsView()
	}
	if s.confirm.Active {
		return s.confirmView(&s.confirm)
	}
	if s.importMode {
		return s.formView(&s.importForm, s.err)
	}
	if s.editing {
		return s.formView(&s.form, s.err)
	}

	return s.settingsView()
}
