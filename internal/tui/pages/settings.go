package pages

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/headless"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
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
	confirm    components.Confirm
	restoring  bool
	busy       bool
	status     string
	err        error
}

// NewSettings 创建 Settings 页。
func NewSettings(app *application.App) *SettingsPage {
	p := &SettingsPage{base: base{app: app}}
	p.form = newSettingsForm()
	return p
}

func (s *SettingsPage) Title() string { return "Settings" }

// Editing 处于表单编辑状态时拦截全局键位。
func (s *SettingsPage) Editing() bool { return s.editing }

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
	if msg, ok := msg.(tea.WindowSizeMsg); ok {
		s.SetSize(msg.Width, msg.Height)
		return s, nil
	}
	switch msg := msg.(type) {
	case components.ConfirmMsg:
		return s.onConfirm(msg)
	case actionDoneMsg:
		s.busy = false
		switch {
		case msg.Err != nil:
			s.err, s.status = msg.Err, ""
		case msg.Action == "backup":
			s.err = nil
			s.status = "备份已导出: " + msg.Data
		}
		return s, nil
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
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
		switch key.String() {
		case "esc":
			s.importMode = false
			s.err = nil
			return s, nil
		case "enter":
			s.submitImportPath()
			return s, nil
		}
		s.importForm.Update(msg)
		return s, nil
	}
	if s.editing {
		switch key.String() {
		case "esc":
			s.editing = false
			s.err = nil
			return s, nil
		case "enter":
			s.submit()
			return s, nil
		}
		s.form.Update(msg)
		return s, nil
	}

	switch key.String() {
	case "e":
		// Reset 清空并把焦点移回第一个字段，再填入当前值（避免沿用上次编辑的焦点位置）
		s.form.Reset()
		s.loadForm()
		s.editing = true
		s.status, s.err = "", nil
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
		s.status, s.err = "", nil
	}
	return s, nil
}

// submitImportPath 校验路径并弹出确认。
func (s *SettingsPage) submitImportPath() {
	path := strings.TrimSpace(s.importForm.ValueByKey("path"))
	if path == "" {
		s.err = fmt.Errorf("请填写备份文件路径")
		return
	}
	if _, err := os.Stat(path); err != nil {
		s.err = fmt.Errorf("备份文件不存在: %s", path)
		return
	}
	s.importPath = path
	s.importMode = false
	s.err = nil
	s.confirm = components.NewConfirm("restore-backup",
		fmt.Sprintf("用 %s 恢复将覆盖当前全部数据，恢复后应用会退出（请重新启动）。确认？", path))
}

// onConfirm 处理恢复确认。
func (s *SettingsPage) onConfirm(msg components.ConfirmMsg) (Page, tea.Cmd) {
	s.confirm.Active = false
	if !msg.Confirmed || msg.ID != "restore-backup" {
		return s, nil
	}
	serviceLock, err := headless.Acquire(s.app.Paths)
	if err != nil {
		s.err = fmt.Errorf("无法恢复: %w（请先停止 headless 服务）", err)
		return s, nil
	}
	defer serviceLock.Close()
	// 停核心 → 关闭数据库 → 恢复文件 → 退出（重启后生效）
	_ = s.app.StopCore()
	_ = s.app.DB.Close()
	if err := application.RestoreArchive(s.app.Paths, s.importPath); err != nil {
		restoreErr := err
		if reopenErr := s.app.ReopenDB(); reopenErr != nil {
			err = fmt.Errorf("%v；重新打开数据库失败: %w", restoreErr, reopenErr)
		}
		s.err = err
		s.restoring = false
		s.app.AppLog.AppendLine("备份恢复失败: " + err.Error())
		return s, nil
	}
	s.restoring = true
	s.status = "备份已恢复，应用即将退出，请重新启动 karing"
	s.app.AppLog.AppendLine("备份已恢复: " + s.importPath + "，应用退出")
	return s, tea.Quit
}

// exportBackup 异步导出备份。
func (s *SettingsPage) exportBackup() tea.Cmd {
	return func() tea.Msg {
		dest, err := s.app.Backup("")
		return actionDoneMsg{Action: "backup", Data: dest, Err: err}
	}
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
	s.status = "设置已保存；端口/日志级别等需重新生成配置（g）并重启（r）生效，自动更新间隔需重启应用生效"
}

func validLogLevel(l string) bool {
	switch l {
	case "debug", "info", "warn", "error":
		return true
	}
	return false
}

func (s *SettingsPage) View() string {
	if s.confirm.Active {
		return s.confirm.View()
	}
	if s.importMode {
		view := s.importForm.View()
		if s.err != nil {
			view += "\n" + styles.Err.Render(s.err.Error())
		}
		return view
	}
	if s.editing {
		view := s.form.View()
		if s.err != nil {
			view += "\n" + styles.Err.Render(s.err.Error())
		}
		return view
	}

	set := s.app.GetSettings()
	allowLAN := "关闭"
	if set.AllowLAN {
		allowLAN = "开启"
	}
	dlProxy := set.DownloadProxy
	if dlProxy == "" {
		dlProxy = "直连"
	}
	clashAPI := "关闭"
	if set.ClashAPIPort > 0 {
		clashAPI = fmt.Sprintf("127.0.0.1:%d", set.ClashAPIPort)
		if set.ClashAPISecret != "" {
			clashAPI += "（已设密钥）"
		}
	}
	autoUpdate := "关闭"
	if set.AutoUpdateMinutes > 0 {
		autoUpdate = fmt.Sprintf("%d 分钟", set.AutoUpdateMinutes)
	}
	privateDirect := "关闭"
	if set.PrivateDirect {
		privateDirect = "开启"
	}
	resolveIPRules := "关闭（IP 规则仅对 IP 目标生效）"
	if set.ResolveIPRules {
		resolveIPRules = "开启（代理侧收到 IP 而非域名）"
	}

	body := fmt.Sprintf(
		"mixed 监听端口:  %d\n允许局域网:      %s\nClash API:       %s\n下载代理:        %s\n核心日志级别:    %s\n订阅自动更新:    %s\n"+
			"内网直连:        %s\nIP 规则解析域名: %s\n\n"+
			"数据库:          %s\n运行时目录:      %s\n缓存目录:        %s\n日志目录:        %s\nsing-box 路径:   %s\n",
		set.MixedPort, allowLAN, clashAPI, dlProxy, set.LogLevel, autoUpdate,
		privateDirect, resolveIPRules,
		s.app.Paths.DB, s.app.Paths.Runtime, s.app.Paths.Cache, s.app.Paths.Logs, s.app.Paths.CoreBin,
	)

	foot := "\ne 编辑 · b 导出备份 · i 导入恢复 · r 清除提示"
	if s.restoring {
		foot = "\n" + styles.Ok.Render(s.status)
	} else if s.err != nil {
		foot = "\n" + styles.Err.Render(s.err.Error()) + foot
	} else if s.status != "" {
		foot = "\n" + styles.Ok.Render(s.status) + foot
	}
	return styles.Title.Render("设置") + "\n" + body + styles.Dim.Render(foot)
}
