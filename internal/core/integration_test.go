package core

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// TestIntegrationRealSingBox 用真实 sing-box 二进制验证完整链路：
// 生成配置 → check → start → 运行确认 → stop。
// 设置环境变量 SINGBOX_BIN 指向真实二进制时才会运行。
func TestIntegrationRealSingBox(t *testing.T) {
	src := os.Getenv("SINGBOX_BIN")
	if src == "" {
		t.Skip("未设置 SINGBOX_BIN，跳过真实 sing-box 集成测试")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("SINGBOX_BIN 指向的文件不存在: %v", err)
	}

	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	// 复制真实二进制到受管路径
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.CoreBin, data, 0o755); err != nil {
		t.Fatal(err)
	}

	bin := NewBinaryManager(paths, "")
	m := NewManager(paths, bin)
	t.Cleanup(func() { _ = m.Stop() })

	// 生成最小配置；用随机空闲端口避免与本机已有服务冲突
	settings := config.DefaultSettings()
	settings.MixedPort = freePort(t)
	settings.ClashAPIPort = 0 // 此测试只验证 mixed 入站，避免占用本机默认 API 端口。
	cfgPath := paths.Config
	out, err := config.Generate(config.Snapshot{Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// 版本解析
	v, err := bin.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	t.Logf("sing-box 版本: %s", v)

	// 校验
	if err := m.Check(ctx, cfgPath); err != nil {
		t.Fatalf("Check: %v", err)
	}

	// 启动
	if err := m.Start(ctx, cfgPath); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !m.IsRunning() {
		t.Fatal("启动后应处于运行状态")
	}
	st := m.Status()
	if st.State != StateRunning || st.Version == "" {
		t.Errorf("状态不符: %+v", st)
	}

	// mixed 端口应可连接（TCP 探测）
	time.Sleep(300 * time.Millisecond)
	if !PortOpen("127.0.0.1", settings.MixedPort) {
		t.Errorf("mixed 端口 %d 未监听", settings.MixedPort)
	}

	// 停止
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if m.IsRunning() {
		t.Fatal("停止后不应处于运行状态")
	}
}

// freePort 申请一个随机空闲 TCP 端口。
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestRealSingBoxDNSResolverCycle 用真实内核给出「DNS resolver 环怎么处置」的判决（V7-8）。
//
// 实测结论（本机 revision cf69a007）：
//
//	sing-box check  <cfg>  → rc=0
//	sing-box run    <cfg>  → FATAL start service: circular server dependency: a -> b -> a
//
// 与 V6 轮的三条教训同源：环只在 run 阶段暴露，静态检查看不出。因此
//  1. 正式配置必须在**生成阶段**拦环（config.ValidateDNSResolverGraph）；
//  2. CI 门禁必须**真启动**，不能用 check。
//
// 本用例刻意手写原始 JSON 而不走 config.Generate：Generate 现在会先拒绝环，
// 走它根本到不了内核。那正是修复生效的证据，但证明不了内核行为——两者要分开取证。
func TestRealSingBoxDNSResolverCycle(t *testing.T) {
	src := os.Getenv("SINGBOX_BIN")
	if src == "" {
		t.Skip("未设置 SINGBOX_BIN，跳过真实 sing-box 集成测试")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读取 SINGBOX_BIN: %v", err)
	}

	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.CoreBin, data, 0o755); err != nil {
		t.Fatal(err)
	}

	// 两种 DNS 拓扑，其余部分逐字相同。
	dnsConfig := func(servers string) string {
		return `{
  "log": {"level": "error"},
  "dns": {"servers": [` + servers + `], "final": "a"},
  "outbounds": [{"type": "direct", "tag": "direct"}],
  "route": {"final": "direct", "default_domain_resolver": "a"}
}
`
	}
	cyclic := dnsConfig(`{"type":"udp","tag":"a","server":"1.1.1.1","domain_resolver":"b"},` +
		`{"type":"udp","tag":"b","server":"8.8.8.8","domain_resolver":"a"}`)
	chain := dnsConfig(`{"type":"udp","tag":"a","server":"1.1.1.1","domain_resolver":"b"},` +
		`{"type":"udp","tag":"b","server":"8.8.8.8"}`)

	writeCfg := func(t *testing.T, name, body string) string {
		t.Helper()
		path := filepath.Join(paths.Runtime, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// 等内核退出，返回是否在期限内退出。
	waitExit := func(inst *Adhoc, d time.Duration) bool {
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if inst.Exited() {
				return true
			}
			time.Sleep(50 * time.Millisecond)
		}
		return inst.Exited()
	}

	ctx := context.Background()

	t.Run("成环的配置必须让内核启动失败", func(t *testing.T) {
		cfgPath := writeCfg(t, "dns-cycle.json", cyclic)
		inst, err := StartAdhoc(ctx, paths.CoreBin, cfgPath, paths.Cache)
		if err != nil {
			t.Fatalf("StartAdhoc: %v", err)
		}
		defer inst.Stop()
		if !waitExit(inst, 5*time.Second) {
			t.Fatal("成环的配置应让内核退出，实际仍在运行")
		}
		if out := strings.Join(inst.Output.Tail(20), "\n"); !strings.Contains(out, "circular server dependency") {
			t.Errorf("内核日志应指出环，实得:\n%s", out)
		}
	})

	// 对照组：同 fixture 去掉环之后必须能正常启动——否则上面那条可能只是
	// 「配置本身非法」而不是「环被拒」。
	t.Run("正向链的同一 fixture 必须能启动", func(t *testing.T) {
		cfgPath := writeCfg(t, "dns-chain.json", chain)
		inst, err := StartAdhoc(ctx, paths.CoreBin, cfgPath, paths.Cache)
		if err != nil {
			t.Fatalf("StartAdhoc: %v", err)
		}
		defer inst.Stop()
		time.Sleep(500 * time.Millisecond)
		if inst.Exited() {
			t.Fatalf("正向链不应启动失败，内核日志:\n%s", strings.Join(inst.Output.Tail(20), "\n"))
		}
	})

	// 生成侧必须拦在更早的地方：同一台机器上 Generate 不得产出成环的配置。
	t.Run("生成侧必须在写出配置前拒绝环", func(t *testing.T) {
		snap := config.Snapshot{
			Settings: config.DefaultSettings(),
			DNS: &config.DNSConfig{
				Strategy: "prefer_ipv4",
				Servers: []config.DNSServer{
					{Tag: "a", Type: "udp", Address: "1.1.1.1", AddressResolver: "b", Enabled: true},
					{Tag: "b", Type: "udp", Address: "8.8.8.8", AddressResolver: "a", Enabled: true},
				},
				Final: "a",
			},
		}
		if _, err := config.Generate(snap); err == nil {
			t.Fatal("成环的 DNS 配置不得被生成为可启动配置")
		}
	})
}
