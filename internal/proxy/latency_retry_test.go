package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/core"
)

// V7-6：测速 API 端口从分配到内核真正 bind 之间有 TOCTOU 窗口
// （core.FreePort 只保证「这一刻没人占」），窗口内被抢占会造成一次偶发的整批
// 失败。重试必须只针对 bind 冲突，且每次都要换端口。

func stubCoreBinary(t *testing.T, m *Manager) {
	t.Helper()
	// m.Bin.Ensure 走 Resolve → Installed（文件存在且可执行），不校验版本，
	// 因此一个壳脚本就够——真正的内核由注入的 StartAdhoc 决定。
	if err := os.WriteFile(m.Paths.CoreBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.Bin = core.NewBinaryManager(m.Paths, "")
}

// fakeCore 写一个假的「sing-box」脚本。
func fakeCore(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-sing-box")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func urlPort(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("解析端口 %q: %v", raw, err)
	}
	return p
}

func retryTestManager(t *testing.T) *Manager {
	t.Helper()
	m := newTestManager(t)
	m.LoadDNS = func() (config.DNSConfig, error) { return config.DNSConfig{}, nil }
	stubCoreBinary(t, m)
	return m
}

func retryTestNode() *config.Node {
	return &config.Node{ID: 7, Name: "retry-node", Protocol: "trojan", Server: "127.0.0.1", Port: 443,
		Enabled: true, Metadata: map[string]any{"password": "p"}}
}

// 第一次尝试的端口被抢占：内核以 bind 冲突退出，必须自动换端口并成功。
func TestLatencyProbeRetriesAfterAPIPortCollision(t *testing.T) {
	m := retryTestManager(t)

	// 第二次尝试的 API 端口由测试自己的 HTTP 服务占着：
	// waitAPI 探测到端口可连接即返回成功，随后 delay 查询也由它应答。
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"delay":42}`))
	}))
	defer api.Close()
	apiPort := urlPort(t, api.URL)

	collide := fakeCore(t, `echo "FATAL start service: start inbound/http: listen tcp 127.0.0.1:1: bind: address already in use" >&2
exit 1`)
	live := fakeCore(t, "trap 'exit 0' TERM\nwhile :; do sleep 1; done")

	var allocs, starts int
	m.AllocatePort = func() (int, error) {
		allocs++
		if allocs == 1 {
			// 一个没人监听的候选端口（core.FreePort 已把它关掉）。
			return core.FreePort()
		}
		return apiPort, nil
	}
	m.StartAdhoc = func(ctx context.Context, bin, configPath, cacheDir string) (*core.Adhoc, error) {
		starts++
		if starts == 1 {
			return core.StartAdhoc(ctx, collide, configPath, cacheDir)
		}
		return core.StartAdhoc(ctx, live, configPath, cacheDir)
	}

	node := retryTestNode()
	results, err := m.testNodes(context.Background(), []*config.Node{node}, "", 0, nil)
	if err != nil {
		t.Fatalf("端口冲突后应自动换端口并成功，实际报错: %v", err)
	}
	if allocs != 2 {
		t.Errorf("应重新分配端口（AllocatePort 调 2 次），实际 %d 次", allocs)
	}
	if starts != 2 {
		t.Errorf("应启动两次临时核心，实际 %d 次", starts)
	}
	if got := results[node.ID]; got != 42 {
		t.Errorf("最终延迟应来自第二次尝试的端口，实际 %d", got)
	}
}

// 非 bind 错误绝不重试，且原错误原样透出——否则真故障会被伪装成偶发。
func TestLatencyProbeDoesNotRetryInvalidConfig(t *testing.T) {
	m := retryTestManager(t)

	var allocs, starts int
	m.AllocatePort = func() (int, error) {
		allocs++
		return core.FreePort()
	}
	m.StartAdhoc = func(context.Context, string, string, string) (*core.Adhoc, error) {
		starts++
		return nil, errors.New("启动临时 sing-box 实例失败: permission denied")
	}

	_, err := m.testNodes(context.Background(), []*config.Node{retryTestNode()}, "", 0, nil)
	if err == nil {
		t.Fatal("启动失败必须返回错误")
	}
	if starts != 1 {
		t.Errorf("非 bind 错误不得重试，StartAdhoc 实际被调用 %d 次", starts)
	}
	if allocs != 1 {
		t.Errorf("非 bind 错误不得重新分配端口，实际 %d 次", allocs)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("原错误必须保留，实际 %v", err)
	}
}

// 重试耗尽的场景：三次都是 bind 冲突时必须如实失败（不静默成功、不死循环）。
func TestLatencyProbeGivesUpAfterRepeatedCollisions(t *testing.T) {
	m := retryTestManager(t)
	collide := fakeCore(t, `echo "listen tcp: bind: address already in use" >&2
exit 1`)

	var starts int
	m.AllocatePort = func() (int, error) { return core.FreePort() }
	m.StartAdhoc = func(ctx context.Context, bin, configPath, cacheDir string) (*core.Adhoc, error) {
		starts++
		return core.StartAdhoc(ctx, collide, configPath, cacheDir)
	}

	if _, err := m.testNodes(context.Background(), []*config.Node{retryTestNode()}, "", 0, nil); err == nil {
		t.Fatal("端口连续被抢占时必须如实报错")
	}
	if starts != defaultProbeAttempts {
		t.Errorf("应尝试 %d 次，实际 %d 次", defaultProbeAttempts, starts)
	}
}
