package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/rules"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// setupHome 准备隔离的 KARING_HOME，并预建全部内置规则集（指向空本地缓存文件），
// 使 app 初始化与配置生成不触发规则集下载（联网）。
func setupHome(t *testing.T) *platform.Paths {
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
	return paths
}

// seedGroup 预置状态：一个手动节点、默认组（Auto/Manual，动态全部节点）
// 与含显式成员的 select 组 "Test"，模拟真实使用中的数据库。
func seedGroup(t *testing.T, paths *platform.Paths) (*config.Node, *config.ProxyGroup) {
	t.Helper()
	db, err := storage.Open(paths)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer db.Close()

	n := &config.Node{
		Name: "JP-01", Protocol: "shadowsocks", Server: "1.2.3.4", Port: 8388, Enabled: true,
		Metadata: map[string]any{"method": "aes-128-gcm", "password": "test"},
	}
	if err := db.CreateNode(n); err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	var testGroup *config.ProxyGroup
	for _, g := range []*config.ProxyGroup{
		{Name: "Auto", Type: "urltest", Members: []config.ProxyGroupMember{{Type: "all"}}},
		{Name: "Manual", Type: "select", Members: []config.ProxyGroupMember{{Type: "all"}}},
		{Name: "Test", Type: "select", Members: []config.ProxyGroupMember{{Type: "node", ID: n.ID}}},
	} {
		if err := db.CreateProxyGroup(g); err != nil {
			t.Fatalf("创建代理组 %s: %v", g.Name, err)
		}
		if g.Name == "Test" {
			testGroup = g
		}
	}
	return n, testGroup
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestUsageErrors(t *testing.T) {
	home := setupHome(t)
	_ = home

	if code, _, _ := run(t, "nonsense"); code != 2 {
		t.Errorf("未知命令退出码 = %d, 期望 2", code)
	}
	if code, _, _ := run(t, "profile"); code != 2 {
		t.Errorf("缺子命令退出码 = %d, 期望 2", code)
	}
	if code, out, _ := run(t, "help"); code != 0 || !strings.Contains(out, "status") {
		t.Errorf("help 退出码 = %d, 输出缺 status", code)
	}
	if code, out, _ := run(t, "version"); code != 0 || !strings.Contains(out, Version) {
		t.Errorf("version 退出码 = %d, 输出缺版本号", code)
	}
	if code, _, _ := run(t, "route"); code != 2 {
		t.Errorf("route 缺子命令退出码 = %d, 期望 2", code)
	}
	if code, _, _ := run(t, "route", "test"); code != 2 {
		t.Errorf("route test 缺输入退出码 = %d, 期望 2", code)
	}
}

func TestRouteTest(t *testing.T) {
	paths := setupHome(t)
	db, err := storage.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRoutingGroup(&config.RoutingGroup{
		Name: "测试直连", Target: "DIRECT", Position: -1, Enabled: true,
		Rules: []config.Rule{{Type: "domain_suffix", Value: "example.com", Enabled: true}},
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.CreateRoutingGroup(&config.RoutingGroup{
		Name: "测试兜底", Target: "BLOCK", Position: 99, Enabled: true,
		Rules: []config.Rule{{Type: "final", Enabled: true}},
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	code, out, errOut := run(t, "route", "test", "www.example.com.")
	if code != 0 {
		t.Fatalf("route test 退出码 = %d, stderr: %s", code, errOut)
	}
	for _, want := range []string{"命中分流组 测试直连", "domain_suffix=example.com", "出站: DIRECT"} {
		if !strings.Contains(out, want) {
			t.Errorf("route test 输出缺 %q:\n%s", want, out)
		}
	}

	code, out, errOut = run(t, "route", "test", "203.0.113.8")
	if code != 0 {
		t.Fatalf("route test IP 退出码 = %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "使用兜底分流组 测试兜底") || !strings.Contains(out, "出站: BLOCK") {
		t.Errorf("route test IP 兜底输出不符: %s", out)
	}

	code, out, errOut = run(t, "route", "test", "127.0.0.1")
	if code != 0 {
		t.Fatalf("route test 内网 IP 退出码 = %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "命中内网直连规则 ip_is_private") || strings.Count(out, "出站: DIRECT") != 1 {
		t.Errorf("route test 内网直连输出不符: %s", out)
	}
}

func TestStatusEmpty(t *testing.T) {
	setupHome(t)
	code, out, errOut := run(t, "status")
	if code != 0 {
		t.Fatalf("status 退出码 = %d, stderr: %s", code, errOut)
	}
	for _, want := range []string{"数据目录", "订阅: 0 个", "代理组: Auto", "未检测到运行实例"} {
		if !strings.Contains(out, want) {
			t.Errorf("status 输出缺 %q:\n%s", want, out)
		}
	}
}

func TestStatusJSON(t *testing.T) {
	setupHome(t)
	code, out, errOut := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("status --json 退出码 = %d, stderr: %s", code, errOut)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("status --json 非法 JSON: %v\n%s", err, out)
	}
	for _, key := range []string{"version", "data_dir", "running", "runtime_state", "mixed_port", "config_exists", "subscriptions", "proxy_groups"} {
		if _, ok := got[key]; !ok {
			t.Errorf("status --json 缺少字段 %q: %s", key, out)
		}
	}
}

func TestStatusJSONDetectsTUICoreByMixedPort(t *testing.T) {
	setupHome(t)
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	app, err := application.New(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	app.Settings.MixedPort = listener.Addr().(*net.TCPAddr).Port
	app.Settings.ClashAPIPort = 0

	var out, stderr bytes.Buffer
	if code := cmdStatusJSON(app, &out, &stderr); code != 0 {
		t.Fatalf("cmdStatusJSON 退出码 = %d, stderr: %s", code, stderr.String())
	}
	var got statusJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Running || got.RuntimeState != "running" {
		t.Fatalf("TUI 核心端口可达时状态不符: running=%v state=%q", got.Running, got.RuntimeState)
	}
}

func TestProfileListAndUpdate(t *testing.T) {
	paths := setupHome(t)
	seedGroup(t, paths)

	if code, out, _ := run(t, "profile", "list"); code != 0 || !strings.Contains(out, "暂无订阅") {
		t.Errorf("空订阅列表输出不符: %s", out)
	}
	if code, _, _ := run(t, "profile", "update", "不存在"); code != 1 {
		t.Errorf("更新不存在订阅退出码 = %d, 期望 1", code)
	}
	if code, out, _ := run(t, "profile", "update"); code != 0 || !strings.Contains(out, "没有启用的订阅") {
		t.Errorf("无启用订阅更新输出不符: %s", out)
	}
}

func TestProxyListAndSelect(t *testing.T) {
	paths := setupHome(t)
	n, g := seedGroup(t, paths)

	code, out, errOut := run(t, "proxy", "list")
	if code != 0 || !strings.Contains(out, "Manual") || !strings.Contains(out, "Test") {
		t.Fatalf("proxy list 输出缺组: %s / %s", out, errOut)
	}

	code, out, _ = run(t, "proxy", "list", "Test")
	if code != 0 || !strings.Contains(out, "JP-01") {
		t.Errorf("proxy list Test 输出缺 JP-01: %s", out)
	}

	// 显式成员组：选中节点 → 持久化 + 重新生成配置
	code, out, errOut = run(t, "proxy", "select", "Test", "JP-01")
	if code != 0 {
		t.Fatalf("proxy select 退出码 = %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "已持久化") {
		t.Errorf("proxy select 输出缺确认: %s", out)
	}

	// 动态"全部节点"组：按节点名选中
	code, _, errOut = run(t, "proxy", "select", "Manual", "JP-01")
	if code != 0 {
		t.Fatalf("proxy select Manual 退出码 = %d, stderr: %s", code, errOut)
	}

	db, err := storage.Open(paths)
	if err != nil {
		t.Fatalf("重新打开数据库: %v", err)
	}
	defer db.Close()
	got, err := db.GetProxyGroup(g.ID)
	if err != nil {
		t.Fatalf("读取代理组: %v", err)
	}
	want := "node:" + strconv.FormatInt(n.ID, 10)
	if got.Selected != want {
		t.Errorf("Selected = %q, 期望 %q", got.Selected, want)
	}
	if _, err := os.Stat(paths.Config); err != nil {
		t.Errorf("配置文件未生成: %v", err)
	}

	// 非组成员与未知组
	if code, _, _ := run(t, "proxy", "select", "Test", "US-01"); code != 1 {
		t.Errorf("选非组成员退出码 = %d, 期望 1", code)
	}
	if code, _, _ := run(t, "proxy", "select", "Nope", "JP-01"); code != 1 {
		t.Errorf("选未知组退出码 = %d, 期望 1", code)
	}
}
