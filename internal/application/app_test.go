package application

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/rules"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// setupApp 准备隔离的 KARING_HOME 与应用；预建全部内置规则集（指向空本地
// 缓存文件），使配置生成不触发规则集下载（联网）。
func setupApp(t *testing.T) (*App, *platform.Paths) {
	t.Helper()
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	db, err := storage.Open(paths)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer db.Close()
	for _, b := range rules.BuiltinRuleSets {
		cache := filepath.Join(paths.Cache, b.Tag+".srs")
		if err := os.WriteFile(cache, nil, 0o644); err != nil {
			t.Fatalf("写缓存文件: %v", err)
		}
		if err := db.CreateRuleSet(&config.RuleSet{
			Name: b.Tag, Tag: b.Tag, SourceType: "remote", Format: "srs",
			URL: b.URL, Enabled: true, CachedPath: cache,
		}); err != nil {
			t.Fatalf("预插规则集: %v", err)
		}
	}

	app, err := New(paths)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { app.Close() })
	return app, paths
}

func TestAutoUpdateOnce(t *testing.T) {
	app, paths := setupApp(t)

	// 本地订阅服务：一条 SIP002 ss 链接的 base64 列表
	sub := base64.StdEncoding.EncodeToString([]byte(
		"ss://YWVzLTI1Ni1nY206cGFzc3dvcmQxMjM@1.2.3.4:8388#JP-01\n"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sub))
	}))
	defer srv.Close()

	if _, err := app.Subs.Add(context.Background(), "测试订阅", srv.URL, ""); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if n := app.AutoUpdateOnce(context.Background()); n != 1 {
		t.Fatalf("AutoUpdateOnce = %d, 期望 1", n)
	}

	subs, err := app.DB.ListSubscriptions()
	if err != nil || len(subs) != 1 {
		t.Fatalf("ListSubscriptions: %v / %d", err, len(subs))
	}
	if subs[0].NodeCount != 1 {
		t.Errorf("NodeCount = %d, 期望 1", subs[0].NodeCount)
	}
	nodes, err := app.DB.ListNodes(0)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("ListNodes: %v / %d", err, len(nodes))
	}
	if nodes[0].Name != "JP-01" {
		t.Errorf("节点名 = %q, 期望 JP-01", nodes[0].Name)
	}
	if _, err := os.Stat(paths.Config); err != nil {
		t.Errorf("配置文件未生成: %v", err)
	}

	// 无订阅更新成功（全部失败）时不重新生成、不报错
	app.Subs.SetEnabled(subs[0].ID, false)
	if n := app.AutoUpdateOnce(context.Background()); n != 0 {
		t.Errorf("停用后 AutoUpdateOnce = %d, 期望 0", n)
	}
}

func TestStartAutoUpdateDisabled(t *testing.T) {
	app, _ := setupApp(t)
	if app.Settings.AutoUpdateMinutes != 0 {
		t.Fatalf("默认 AutoUpdateMinutes = %d, 期望 0", app.Settings.AutoUpdateMinutes)
	}
	// 关闭状态立即返回（不阻塞）
	done := make(chan struct{})
	go func() {
		app.StartAutoUpdate(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartAutoUpdate 在关闭状态下未立即返回")
	}
}

func TestStartAutoUpdateRejectsDurationOverflow(t *testing.T) {
	app, _ := setupApp(t)
	app.Settings.AutoUpdateMinutes = config.MaxAutoUpdateMinutes + 1
	done := make(chan struct{})
	go func() {
		app.StartAutoUpdate(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("超出 time.Duration 的自动更新间隔未立即退出")
	}
}

func TestGenerateConfigInstallsEmbeddedCatalogOffline(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	app, err := New(paths)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { app.Close() })

	var fetches int
	app.Rules.Fetch = func(context.Context, string, string, string, int64) ([]byte, error) {
		fetches++
		return nil, os.ErrNotExist
	}
	if err := app.GenerateConfig(context.Background()); err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}
	if fetches != 0 {
		t.Fatalf("首次生成默认配置不应联网，实际调用 Fetch %d 次", fetches)
	}
	for _, ref := range []string{"geosite-cn", "geoip-cn", "geosite-telegram", "geoip-telegram", "geosite-category-ai-!cn"} {
		path := filepath.Join(app.Rules.CacheDir(), ref+".srs")
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			t.Errorf("嵌入规则集 %s 未安装到缓存（err=%v）", ref, err)
		}
	}
	out, err := os.ReadFile(paths.Config)
	if err != nil {
		t.Fatalf("读取生成配置: %v", err)
	}
	if strings.Contains(string(out), `"type":"remote"`) {
		t.Error("默认分类规则集首次生成时不应回退为 remote")
	}
}

func TestBuildSnapshotExcludesDisabledSubscriptionNodes(t *testing.T) {
	app, _ := setupApp(t)

	sub, err := app.Subs.Add(context.Background(), "停用订阅", "https://example.com/sub", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := app.DB.CreateNode(&config.Node{Name: "订阅节点", Protocol: "shadowsocks", Server: "1.2.3.4", Port: 8388, Enabled: true, SubscriptionID: sub.ID, Metadata: map[string]any{}}); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if err := app.DB.CreateNode(&config.Node{Name: "手动节点", Protocol: "shadowsocks", Server: "1.2.3.5", Port: 8388, Enabled: true, Metadata: map[string]any{}}); err != nil {
		t.Fatalf("CreateNode manual: %v", err)
	}
	if err := app.Subs.SetEnabled(sub.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	snap, err := app.buildSnapshot()
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	if len(snap.Nodes) != 1 || snap.Nodes[0].Name != "手动节点" {
		t.Fatalf("停用订阅节点仍在快照中: %+v", snap.Nodes)
	}
}

func TestReopenDBRebindsManagers(t *testing.T) {
	app, _ := setupApp(t)
	if err := app.DB.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := app.ReopenDB(); err != nil {
		t.Fatalf("ReopenDB: %v", err)
	}
	for name, got := range map[string]*storage.DB{
		"Rules": app.Rules.DB,
		"Rout":  app.Rout.DB,
		"DNS":   app.DNS.DB,
		"Subs":  app.Subs.DB,
		"Proxy": app.Proxy.DB,
	} {
		if got != app.DB {
			t.Errorf("%s manager 未绑定新数据库", name)
		}
	}
	if _, err := app.DB.ListSubscriptions(); err != nil {
		t.Fatalf("重开数据库不可读: %v", err)
	}
}

func TestRestartCoreRegeneratesConfig(t *testing.T) {
	app, paths := setupApp(t)

	script := `#!/bin/sh
case "$1" in
  version) echo "sing-box version 1.14.0"; exit 0 ;;
  check) exit 0 ;;
  run) trap 'exit 0' TERM; while :; do sleep 1; done ;;
esac
exit 1
`
	if err := os.WriteFile(paths.CoreBin, []byte(script), 0o755); err != nil {
		t.Fatalf("写入假 sing-box: %v", err)
	}

	if err := app.StartCore(context.Background()); err != nil {
		t.Fatalf("StartCore: %v", err)
	}
	defer app.StopCore()

	if err := app.DB.CreateNode(&config.Node{
		Name:     "重启后节点",
		Protocol: "shadowsocks",
		Server:   "1.2.3.4",
		Port:     8388,
		Enabled:  true,
		Metadata: map[string]any{"method": "aes-128-gcm", "password": "pw"},
	}); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	if err := app.RestartCore(context.Background()); err != nil {
		t.Fatalf("RestartCore: %v", err)
	}
	out, err := os.ReadFile(paths.Config)
	if err != nil {
		t.Fatalf("读取配置: %v", err)
	}
	if !strings.Contains(string(out), `"tag": "重启后节点"`) {
		t.Fatalf("RestartCore 未将最新数据库节点写入配置: %s", out)
	}
}
