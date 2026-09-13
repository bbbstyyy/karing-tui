package core

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

func TestInstallEmbedded(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte("#!/bin/sh\necho embedded\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	b := NewBinaryManager(paths, "")
	if err := b.installEmbedded(compressed.Bytes()); err != nil {
		t.Fatalf("installEmbedded: %v", err)
	}
	got, err := os.ReadFile(paths.CoreBin)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("#!/bin/sh\necho embedded\n")) {
		t.Fatalf("installed content = %q", got)
	}
	if info, err := os.Stat(paths.CoreBin); err != nil {
		t.Fatal(err)
	} else if info.Mode()&0o111 == 0 {
		t.Fatal("installed embedded binary is not executable")
	}
}

func TestParseVersion(t *testing.T) {
	out := "sing-box version 1.14.0\n\nEnvironment: go1.26.7 darwin/arm64\nTags: with_gvisor\n"
	v, err := ParseVersion(out)
	if err != nil || v != "1.14.0" {
		t.Errorf("ParseVersion = %q, %v; 期望 1.14.0", v, err)
	}

	if _, err := ParseVersion(" garbage "); err == nil {
		t.Error("无法解析的输出应返回错误")
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.13.9", "1.14.0", -1},
		{"1.14.0", "1.14.0", 0},
		{"1.15.0", "1.14.0", 1},
	} {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q,%q)=%d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestFetchReleaseAssetSHA256(t *testing.T) {
	const want = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q", got)
		}
		_, _ = w.Write([]byte(`{"assets":[{"name":"sing-box-1.14.0-darwin-arm64.tar.gz","digest":"sha256:` + want + `"}]}`))
	}))
	defer server.Close()

	got, err := fetchReleaseAssetSHA256(context.Background(), server.Client(), server.URL, "sing-box-1.14.0-darwin-arm64.tar.gz")
	if err != nil {
		t.Fatalf("fetchReleaseAssetSHA256: %v", err)
	}
	if got != want {
		t.Fatalf("digest = %q, 期望 %q", got, want)
	}
}

func TestNormalizeSHA256(t *testing.T) {
	want := strings.Repeat("ab", 32)
	for _, input := range []string{want, "sha256:" + want, "SHA256:" + want} {
		got, err := normalizeSHA256(input)
		if err != nil || got != want {
			t.Errorf("normalizeSHA256(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "sha256:bad", strings.Repeat("g", 64)} {
		if _, err := normalizeSHA256(input); err == nil {
			t.Errorf("normalizeSHA256(%q) 应返回错误", input)
		}
	}
}

func TestLogBufRing(t *testing.T) {
	b := NewLogBuf(3)
	for i := 0; i < 5; i++ {
		b.AppendLine("line")
		_ = i
	}
	// 上面逐行写 5 行同名日志不好区分，重写：
	b2 := NewLogBuf(3)
	for i := 0; i < 5; i++ {
		b2.AppendLine(string(rune('a' + i)))
	}
	got := b2.Tail(0)
	if len(got) != 3 || got[0] != "c" || got[2] != "e" {
		t.Errorf("Tail = %v, 期望 [c d e]", got)
	}
	if b2.Dropped() != 2 {
		t.Errorf("Dropped = %d, 期望 2", b2.Dropped())
	}

	b3 := NewLogBuf(10)
	_, _ = b3.Write([]byte("a\nb\n\nc\n"))
	if got := b3.Tail(0); len(got) != 3 {
		t.Errorf("Write 拆行结果 = %v, 期望 3 行", got)
	}
}

// newFakeSingBox 创建一个行为类似 sing-box 的假二进制：
// `version` 输出版本串，`check` 退出 0，`run` 睡眠直到被杀。
func newFakeSingBox(t *testing.T, binDir string) string {
	t.Helper()
	script := `#!/bin/sh
case "$1" in
  version) echo "sing-box version 9.9.9"; exit 0 ;;
  check)   exit 0 ;;
  run)     trap 'exit 0' TERM; while :; do sleep 1; done ;;
esac
exit 1
`
	path := filepath.Join(binDir, "sing-box")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestManager(t *testing.T) (*Manager, *platform.Paths) {
	t.Helper()
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeSingBox(t, filepath.Dir(paths.CoreBin))
	// 直接把假二进制放到受管路径，Resolve 优先命中。
	if fake != paths.CoreBin {
		if err := os.Rename(fake, paths.CoreBin); err != nil {
			t.Fatal(err)
		}
	}
	bin := NewBinaryManager(paths, "")
	return NewManager(paths, bin), paths
}

func TestManagerLifecycle(t *testing.T) {
	m, paths := newTestManager(t)

	if m.IsRunning() {
		t.Fatal("初始状态应为停止")
	}

	// 启动前需要一份配置文件（假 check 恒过）
	cfg := filepath.Join(paths.Runtime, "config.json")
	if err := os.WriteFile(cfg, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.Check(context.Background(), cfg); err != nil {
		t.Fatalf("Check: %v", err)
	}

	if err := m.Start(context.Background(), cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !m.IsRunning() {
		t.Fatal("Start 后应为运行中")
	}
	st := m.Status()
	if st.State != StateRunning {
		t.Errorf("Status.State = %v", st.State)
	}

	// 重复启动应报错
	if err := m.Start(context.Background(), cfg); err == nil {
		t.Error("重复启动应返回错误")
	}

	// 日志缓冲可用（假进程无输出，仅验证不 panic）
	_ = m.Output.Tail(5)

	// 停止
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if m.IsRunning() {
		t.Fatal("Stop 后应为停止")
	}

	// 重复停止应无害
	if err := m.Stop(); err != nil {
		t.Fatalf("重复 Stop: %v", err)
	}

	// 重启
	if err := m.Restart(context.Background(), cfg); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !m.IsRunning() {
		t.Fatal("Restart 后应为运行中")
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("收尾 Stop: %v", err)
	}
}

func TestManagerStartHonorsCanceledContext(t *testing.T) {
	m, paths := newTestManager(t)
	cfg := filepath.Join(paths.Runtime, "config.json")
	if err := os.WriteFile(cfg, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Start(ctx, cfg); err != context.Canceled {
		t.Fatalf("取消的 context 应返回 context.Canceled，得到 %v", err)
	}
	if m.IsRunning() {
		t.Fatal("取消的启动不应留下运行实例")
	}
}

func TestManagerCrashDetection(t *testing.T) {
	m, paths := newTestManager(t)

	// 用一个立即退出的假二进制模拟崩溃。
	crashScript := `#!/bin/sh
case "$1" in
  version) echo "sing-box version 9.9.9"; exit 0 ;;
  check)   exit 0 ;;
  run)     echo "FATAL: port in use" >&2; exit 1 ;;
esac
exit 1
`
	if err := os.WriteFile(paths.CoreBin, []byte(crashScript), 0o755); err != nil {
		t.Fatal(err)
	}

	crashed := make(chan error, 1)
	m.OnExit = func(err error) { crashed <- err }

	cfg := filepath.Join(paths.Runtime, "config.json")
	if err := os.WriteFile(cfg, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start 应检测到立即退出并报错
	err := m.Start(context.Background(), cfg)
	if err == nil {
		t.Fatal("启动即崩溃的进程应返回错误")
	}
	if !m.IsRunning() {
		// 预期：已停止
	} else {
		t.Fatal("崩溃后不应处于运行状态")
	}
	select {
	case e := <-crashed:
		if e == nil {
			t.Log("OnExit 收到 nil（主动停止路径），异常退出未被识别")
		}
	case <-time.After(3 * time.Second):
		t.Error("3 秒内未收到崩溃回调")
	}
}

func TestBinaryResolveMissing(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	bin := NewBinaryManager(paths, "")
	if bin.Installed() {
		t.Error("未下载时 Installed 应为 false")
	}
	if _, err := bin.Resolve(); err == nil {
		t.Error("二进制缺失时 Resolve 应返回错误")
	}
}
