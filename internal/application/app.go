// Package application 是表现层（TUI）与领域模块之间的桥梁：
// 持有 DB、设置、运行核心，并提供配置生成与核心启停的编排。
package application

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/clashapi"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/core"
	"github.com/bbbstyyy/karing-tui/internal/dns"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/proxy"
	"github.com/bbbstyyy/karing-tui/internal/redact"
	"github.com/bbbstyyy/karing-tui/internal/routing"
	"github.com/bbbstyyy/karing-tui/internal/rules"
	"github.com/bbbstyyy/karing-tui/internal/storage"
	"github.com/bbbstyyy/karing-tui/internal/subscription"
)

// App 是应用门面，聚合所有子系统。
type App struct {
	Paths *platform.Paths
	DB    *storage.DB
	Bin   *core.BinaryManager
	Core  *core.Manager
	Subs  *subscription.Manager
	Proxy *proxy.Manager
	Rules *rules.Manager
	Rout  *routing.Manager
	DNS   *dns.Manager

	Settings config.Settings

	// AppLog 是程序自身日志的环形缓冲，供 TUI Logs 页查看。
	AppLog *core.LogBuf

	mu                             sync.Mutex
	configOpMu                     sync.Mutex
	operationMu                    sync.RWMutex
	restored                       bool
	backgroundCtx                  context.Context
	cancelBackground               context.CancelFunc
	configStateMu                  sync.RWMutex
	revision                       uint64
	generatedRevision              uint64
	appliedRevision                uint64
	generatedSettings              config.Settings
	runningSettings                config.Settings
	runningSettingsKnown           bool
	generatedHash, appliedHash     [32]byte
	generatedConfig, runningConfig []byte
	settingsMu                     sync.RWMutex
	configGenerated                bool
	lastCheckAt                    time.Time
	lastCheckErr                   error

	logFile io.WriteCloser
	logger  *log.Logger
	// instanceLock is held by the interactive TUI and headless supervisor so
	// backup restore cannot replace the database underneath a live App.
	instanceLock *storage.InstanceLock
}

// New 初始化应用：打开数据库、加载设置、创建运行核心管理器，并补齐默认配置。
func New(paths *platform.Paths) (*App, error) {
	return newApp(paths, true)
}

// NewReadOnly 打开应用供只读 CLI 使用：不执行默认配置初始化，也不迁移 schema。
// 数据库不存在时不会创建它（见 storage.OpenQueryOnly）。
// 这样 status/list/route test 等命令不会在读取数据库时产生写入。
func NewReadOnly(paths *platform.Paths) (*App, error) {
	return newApp(paths, false)
}

func newApp(paths *platform.Paths, initializeDefaults bool) (*App, error) {
	// 只读入口（CLI 的 status / profile list / route list / diagnose 等 14 处）
	// 走 query-only 打开策略：不迁移、不 chmod、库不存在时不创建文件（C13）。
	var (
		db  *storage.DB
		err error
	)
	if initializeDefaults {
		db, err = storage.Open(paths)
	} else {
		db, err = storage.OpenQueryOnly(paths)
	}
	if err != nil {
		return nil, err
	}

	settings, err := db.LoadSettings()
	if err != nil {
		db.Close()
		return nil, err
	}

	a := &App{
		Paths:    paths,
		DB:       db,
		Settings: settings,
		AppLog:   core.NewLogBuf(1000),
	}
	if initializeDefaults {
		if err := db.EnsureDefaultGroups(); err != nil {
			db.Close()
			return nil, fmt.Errorf("初始化默认代理组失败: %w", err)
		}
	}
	a.Rules = rules.NewManager(db, paths, func() string { return a.GetSettings().DownloadProxy }, a.logf)
	a.Rout = routing.NewManager(db, a.logf)
	if initializeDefaults {
		if err := a.Rules.EnsureBuiltinRuleSets(); err != nil {
			db.Close()
			return nil, fmt.Errorf("初始化内置规则集失败: %w", err)
		}
		if err := routing.EnsureDefaultRouting(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("初始化默认分流方案失败: %w", err)
		}
	}
	a.DNS = dns.NewManager(db, a.logf)
	if initializeDefaults {
		if err := a.DNS.EnsureDefaultDNS(); err != nil {
			db.Close()
			return nil, fmt.Errorf("初始化默认 DNS 失败: %w", err)
		}
	}
	a.Bin = core.NewBinaryManager(paths, settings.DownloadProxy)
	a.Core = core.NewManager(paths, a.Bin)
	a.Subs = subscription.NewManager(db, func() string { return a.GetSettings().DownloadProxy }, a.logf)
	a.Proxy = proxy.NewManager(db, paths, a.Bin, a.logf)
	a.Subs.AutoTest = func(ctx context.Context, subID int64) error {
		nodes, err := a.DB.ListNodes(subID)
		if err != nil {
			return err
		}
		ids := make([]int64, 0, len(nodes))
		for _, n := range nodes {
			ids = append(ids, n.ID)
		}
		_, err = a.Proxy.TestLatency(ctx, ids, "", 0)
		return err
	}
	a.Core.OnExit = func(err error) {
		a.logf("sing-box 进程退出: %v", err)
	}

	if initializeDefaults {
		logPath := filepath.Join(paths.Logs, "karing.log")
		logFile, err := core.NewRotateWriter(logPath, core.DefaultLogMaxBytes, core.DefaultLogKeep)
		if err != nil {
			db.Close()
			return nil, err
		}
		a.logFile = logFile
		a.logger = log.New(io.MultiWriter(logFile, a.AppLog), "", log.LstdFlags)
	} else {
		a.logger = log.New(io.Discard, "", 0)
	}

	a.logf("Karing TUI 启动，数据目录: %s", paths.Root)
	a.backgroundCtx, a.cancelBackground = context.WithCancel(context.Background())
	return a, nil
}

// NewExclusive initializes an application while holding the data-directory
// instance lock for its lifetime. The TUI and headless supervisor use this
// entry point; ordinary CLI readers use NewReadOnly so status commands can
// inspect a running headless service without mutating default configuration.
func NewExclusive(paths *platform.Paths) (*App, error) {
	lock, err := storage.AcquireInstance(paths)
	if err != nil {
		return nil, err
	}
	a, err := New(paths)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	a.instanceLock = lock
	return a, nil
}

// Close 停止核心、关闭日志与数据库。
func (a *App) Close() error {
	if a.cancelBackground != nil {
		a.cancelBackground()
	}
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	a.restored = true
	if err := a.Core.Stop(); err != nil {
		a.logf("停止核心失败: %v", err)
	}
	if a.logFile != nil {
		a.logFile.Close()
		a.logFile = nil
	}
	dbErr := a.DB.Close()
	lockErr := a.instanceLock.Close()
	if dbErr != nil {
		return dbErr
	}
	return lockErr
}

func (a *App) BackgroundContext() context.Context {
	if a.backgroundCtx != nil {
		return a.backgroundCtx
	}
	return context.Background()
}

// ReopenDB 重新打开数据库，并让所有领域 manager 使用新连接。
// 主要用于备份恢复失败后的 TUI 恢复，调用前旧连接应已关闭。
func (a *App) ReopenDB() error {
	db, err := storage.Open(a.Paths)
	if err != nil {
		return err
	}
	a.DB = db
	a.Rules.DB = db
	a.Rout.DB = db
	a.DNS.DB = db
	a.Subs.DB = db
	a.Proxy.DB = db
	return nil
}

func (a *App) logf(format string, args ...any) {
	if a.logger != nil {
		a.logger.Print(redact.Text(fmt.Sprintf(format, args...)))
	}
}

// GetSettings returns a consistent snapshot of the mutable application settings.
func (a *App) GetSettings() config.Settings {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.Settings
}

// SetSettings publishes a new application settings snapshot atomically.
func (a *App) SetSettings(s config.Settings) {
	a.settingsMu.Lock()
	a.Settings = s
	a.settingsMu.Unlock()
}

// --- 配置生成 ---

// GenerateConfig 由内部模型生成 sing-box 配置并写入 runtime/config.json，先备份上一份。
func (a *App) GenerateConfig(ctx context.Context) error {
	a.configOpMu.Lock()
	defer a.configOpMu.Unlock()
	return a.generateConfig(ctx)
}

func (a *App) generateConfig(ctx context.Context) error {
	// 补全规则集本地缓存（尽力而为；失败时生成远程引用，由 sing-box 启动时下载）
	if err := a.Rules.EnsureCached(ctx); err != nil {
		a.logf("规则集缓存补全失败: %v", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.configStateMu.RLock()
	revision := a.revision
	a.configStateMu.RUnlock()

	snap, err := a.buildSnapshot()
	if err != nil {
		return fmt.Errorf("收集配置数据失败: %w", err)
	}
	out, err := config.Generate(snap)
	if err != nil {
		return fmt.Errorf("生成配置失败: %w", err)
	}
	if old, err := os.ReadFile(a.Paths.Config); err == nil {
		if bak := a.Paths.Config + ".bak"; string(old) != string(out) {
			if err := os.WriteFile(bak, old, 0o600); err != nil {
				return fmt.Errorf("备份旧配置失败: %w", err)
			}
			if err := os.Chmod(bak, 0o600); err != nil {
				return fmt.Errorf("设置旧配置备份权限失败: %w", err)
			}
		}
	}
	// 先写同目录临时文件，再替换目标，避免进程中断留下半个配置文件。
	tmp, err := os.CreateTemp(filepath.Dir(a.Paths.Config), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时配置文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("设置临时配置文件权限失败: %w", err)
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return fmt.Errorf("写入配置文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("同步配置文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时配置文件失败: %w", err)
	}
	if err := replaceFile(tmpPath, a.Paths.Config); err != nil {
		return fmt.Errorf("替换配置文件失败: %w", err)
	}
	a.configStateMu.Lock()
	a.configGenerated = true
	a.generatedRevision = revision
	a.generatedSettings = snap.Settings
	a.generatedHash = sha256.Sum256(out)
	a.generatedConfig = append([]byte(nil), out...)
	a.lastCheckAt = time.Time{}
	a.lastCheckErr = nil
	a.configStateMu.Unlock()
	a.logf("配置已生成: %s", a.Paths.Config)
	return nil
}

// buildSnapshot 从数据库收集配置生成所需的全部内部模型。
func (a *App) buildSnapshot() (config.Snapshot, error) {
	snap := config.Snapshot{
		Settings:        a.GetSettings(),
		RuleSetCacheDir: a.Rules.CacheDir(),
	}

	nodes, err := a.DB.ListNodes(0)
	if err != nil {
		return snap, err
	}
	subs, err := a.DB.ListSubscriptions()
	if err != nil {
		return snap, err
	}
	enabledSubs := make(map[int64]bool, len(subs))
	for _, sub := range subs {
		enabledSubs[sub.ID] = sub.Enabled
	}
	for _, node := range nodes {
		if node.SubscriptionID == config.ManualSubscriptionID || enabledSubs[node.SubscriptionID] {
			snap.Nodes = append(snap.Nodes, node)
		}
	}

	groups, err := a.DB.ListProxyGroups()
	if err != nil {
		return snap, err
	}
	snap.ProxyGroups = groups

	routings, err := a.DB.ListRoutingGroups()
	if err != nil {
		return snap, err
	}
	snap.RoutingGroups = routings

	ruleSets, err := a.DB.ListRuleSets()
	if err != nil {
		return snap, err
	}
	snap.RuleSets = ruleSets

	dnsCfg, err := a.DNS.LoadConfig()
	if err != nil {
		return snap, err
	}
	snap.DNS = &dnsCfg
	return snap, nil
}

// CheckConfig 校验生成的配置；结果缓存在 App 供 Dashboard 展示。
func (a *App) CheckConfig(ctx context.Context) error {
	a.configOpMu.Lock()
	defer a.configOpMu.Unlock()
	return a.checkConfig(ctx)
}

func (a *App) checkConfig(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	err := a.Core.Check(ctx, a.Paths.Config)
	a.configStateMu.Lock()
	a.lastCheckAt = time.Now()
	a.lastCheckErr = err
	a.configStateMu.Unlock()
	if err != nil {
		a.logf("配置校验失败: %v", err)
		return err
	}
	a.logf("配置校验通过")
	return nil
}

// ConfigStatus 返回配置状态描述。
func (a *App) ConfigStatus() (generated bool, lastCheckAt time.Time, lastCheckErr error) {
	a.configStateMu.RLock()
	defer a.configStateMu.RUnlock()
	return a.configGenerated, a.lastCheckAt, a.lastCheckErr
}

// MarkConfigDirty records a model change without waiting for network or core
// operations. UI feedback must remain responsive while a config is checked.
func (a *App) MarkConfigDirty() {
	a.configStateMu.Lock()
	a.revision++
	a.configStateMu.Unlock()
}

// ConfigRevision 返回配置变更代数：MarkConfigDirty 每成功写入一次模型就自增。
//
// 供界面把「按模型推导出来的数据」按代数缓存：代数变了就重算，没变就直接复用。
// 它只保证「写入过就一定变」，不保证「没变就一定没写入」——因此只适合做
// 失效式缓存（多算几次无害），不能拿它证明某次写入没有发生。
func (a *App) ConfigRevision() uint64 {
	a.configStateMu.RLock()
	defer a.configStateMu.RUnlock()
	return a.revision
}

func (a *App) ConfigStage() string {
	running := a.Core.IsRunning()
	a.configStateMu.RLock()
	defer a.configStateMu.RUnlock()
	if !a.configGenerated || a.revision != a.generatedRevision {
		return "已保存 · 待生成/校验"
	}
	if a.lastCheckAt.IsZero() {
		return "已生成 · 待校验"
	}
	if a.lastCheckErr != nil {
		return "校验失败 · 待修正"
	}
	if running && a.appliedRevision == a.generatedRevision && a.runningSettingsKnown && a.generatedHash == a.appliedHash {
		return "运行中已生效"
	}
	return "校验通过 · 待应用"
}

func (a *App) recordApplied() {
	a.configStateMu.Lock()
	defer a.configStateMu.Unlock()
	a.appliedRevision = a.generatedRevision
	a.appliedHash = a.generatedHash
	a.runningConfig = append([]byte(nil), a.generatedConfig...)
	a.runningSettings = a.generatedSettings
	a.runningSettingsKnown = true
}

// CoreSettings returns the settings loaded by the running core, or the saved
// settings while stopped. Saving pending settings must not change runtime views.
func (a *App) CoreSettings() config.Settings {
	set := a.GetSettings()
	if a.Core.IsRunning() {
		a.configStateMu.RLock()
		if a.runningSettingsKnown {
			set = a.runningSettings
		}
		a.configStateMu.RUnlock()
	}
	return set
}

// ClashClient 返回 clash API 客户端；未开启（端口为 0）时返回 nil。
func (a *App) ClashClient() *clashapi.Client {
	set := a.CoreSettings()
	if set.ClashAPIPort <= 0 || set.ClashAPIPort > 65535 {
		return nil
	}
	return clashapi.New("127.0.0.1", set.ClashAPIPort, set.ClashAPISecret)
}

// --- 核心生命周期 ---

// StartCore 确保二进制存在 → 从最新数据库快照生成并校验配置 → 启动。
func (a *App) StartCore(ctx context.Context) error {
	a.configOpMu.Lock()
	defer a.configOpMu.Unlock()
	a.logf("正在启动 sing-box…")
	wasRunning := a.Core.IsRunning()
	if !wasRunning {
		if _, err := a.Bin.Ensure(ctx); err != nil {
			a.logf("sing-box 二进制不可用: %v", err)
			return err
		}
		if err := a.generateConfig(ctx); err != nil {
			return err
		}
		if err := a.checkConfig(ctx); err != nil {
			return err
		}
	}
	if err := a.Core.Start(ctx, a.Paths.Config); err != nil {
		a.logf("启动失败: %v", err)
		return err
	}
	if !wasRunning {
		a.recordApplied()
	}
	a.logf("sing-box 已启动，mixed 端口 %d", a.GetSettings().MixedPort)
	return nil
}

// StopCore 停止 sing-box。
func (a *App) StopCore() error {
	a.configOpMu.Lock()
	defer a.configOpMu.Unlock()
	a.logf("正在停止 sing-box…")
	if err := a.Core.Stop(); err != nil {
		a.logf("停止失败: %v", err)
		return err
	}
	a.logf("sing-box 已停止")
	return nil
}

// RestartCore 从最新应用状态生成并校验配置后重启 sing-box。
func (a *App) RestartCore(ctx context.Context) error {
	a.configOpMu.Lock()
	defer a.configOpMu.Unlock()
	if _, err := a.Bin.Ensure(ctx); err != nil {
		a.logf("sing-box 二进制不可用: %v", err)
		return err
	}
	if err := a.generateConfig(ctx); err != nil {
		return err
	}
	if err := a.checkConfig(ctx); err != nil {
		return err
	}
	if err := a.Core.Restart(ctx, a.Paths.Config); err != nil {
		a.logf("重启失败: %v", err)
		return err
	}
	a.recordApplied()
	a.logf("sing-box 已重启")
	return nil
}

// --- 订阅自动更新 ---

// StartAutoUpdate 运行订阅自动更新循环（阻塞，应在 goroutine 中调用）。
// 间隔取启动时的 Settings.AutoUpdateMinutes（分钟），0 表示关闭、立即返回；
// 修改间隔需重启应用生效。每轮更新全部启用订阅，有成功则重新生成配置
// （不自动重启核心，运行中的实例需手动重启后新节点才生效）。
func (a *App) StartAutoUpdate(ctx context.Context) {
	set := a.GetSettings()
	if set.AutoUpdateMinutes <= 0 {
		return
	}
	if set.AutoUpdateMinutes > config.MaxAutoUpdateMinutes {
		a.logf("订阅自动更新间隔超出范围（最大 %d 分钟），已禁用", config.MaxAutoUpdateMinutes)
		return
	}
	interval := time.Duration(set.AutoUpdateMinutes) * time.Minute
	a.logf("订阅自动更新已启动，间隔 %d 分钟", set.AutoUpdateMinutes)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.AutoUpdateOnce(ctx)
		}
	}
}

// AutoUpdateOnce 执行一轮自动更新：更新全部启用订阅并按需重新生成配置。
// 返回成功更新的订阅数。
func (a *App) AutoUpdateOnce(ctx context.Context) int {
	release, err := a.BeginOperation()
	if err != nil {
		return 0
	}
	defer release()
	subs, err := a.DB.ListSubscriptions()
	if err != nil {
		a.logf("自动更新读取订阅失败: %v", err)
		return 0
	}
	ok, failed := 0, 0
	for _, s := range subs {
		if !s.Enabled {
			continue
		}
		if _, err := a.Subs.Update(ctx, s.ID); err != nil {
			a.logf("自动更新订阅 %q 失败: %v", s.Name, err)
			failed++
			continue
		}
		ok++
		a.MarkConfigDirty()
	}
	if ok == 0 {
		if failed > 0 {
			a.logf("自动更新完成: %d 成功, %d 失败", ok, failed)
		}
		return 0
	}
	if err := a.GenerateConfig(ctx); err != nil {
		a.logf("自动更新后重新生成配置失败: %v", err)
	} else {
		a.logf("自动更新完成: %d 成功, %d 失败，配置已重新生成（运行中的实例重启后生效）", ok, failed)
	}
	return ok
}
