package core

import (
	"context"
	"net"
	"os"
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
