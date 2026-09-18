package core

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// newCountingSingBox 创建一个会记录 `version` 调用次数的假 sing-box：
// 每次执行 version 都把计数文件加一，版本号本身固定。
func newCountingSingBox(t *testing.T, binPath, counterPath string) {
	t.Helper()
	script := `#!/bin/sh
if [ "$1" = "version" ]; then
  n=0
  [ -f "` + counterPath + `" ] && n=$(cat "` + counterPath + `")
  echo $((n + 1)) > "` + counterPath + `"
  echo "sing-box version 9.9.9"
  exit 0
fi
exit 1
`
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readCounter(t *testing.T, counterPath string) int {
	t.Helper()
	data, err := os.ReadFile(counterPath)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("计数文件内容非法: %q", data)
	}
	return n
}

// Version 对同一二进制必须走缓存：TUI 每帧渲染都会经 Manager.Status()
// 调用 Version，不起缓存时每次都要执行 `sing-box version` 子进程，
// 滚动列表会明显卡顿。
func TestVersionCachesSubprocessCalls(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(t.TempDir(), "count")
	newCountingSingBox(t, paths.CoreBin, counter)

	bin := NewBinaryManager(paths, "")
	for i := 0; i < 5; i++ {
		v, err := bin.Version(context.Background())
		if err != nil {
			t.Fatalf("Version: %v", err)
		}
		if v != "9.9.9" {
			t.Fatalf("Version = %q, 期望 9.9.9", v)
		}
	}
	if n := readCounter(t, counter); n != 1 {
		t.Fatalf("version 子进程执行了 %d 次，期望缓存后仅 1 次", n)
	}
}

// 二进制被替换（下载更新/释放内置）后缓存必须失效，重新执行 version。
func TestVersionCacheInvalidatesOnBinaryChange(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(t.TempDir(), "count")
	newCountingSingBox(t, paths.CoreBin, counter)

	bin := NewBinaryManager(paths, "")
	if _, err := bin.Version(context.Background()); err != nil {
		t.Fatalf("Version: %v", err)
	}
	if n := readCounter(t, counter); n != 1 {
		t.Fatalf("首次 Version 后计数 = %d，期望 1", n)
	}

	// 模拟二进制更新：重写文件并改变 mtime（缓存键含 mtime 与大小）。
	newCountingSingBox(t, paths.CoreBin, counter)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(paths.CoreBin, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := bin.Version(context.Background()); err != nil {
		t.Fatalf("更新后 Version: %v", err)
	}
	if n := readCounter(t, counter); n != 2 {
		t.Fatalf("二进制更新后 version 子进程执行了 %d 次，期望 2", n)
	}
}

// V7-9：设置页保存下载代理（SetProxy）与后台下载读代理（proxySnapshot）并发时
// 不得有 data race。
//
// 判决性：旧实现里 SetProxy 裸写、Download 裸读同一个 string 字段。用并发探针
// 在旧代码上实测过一次 DATA RACE（Write at binary.go:62 SetProxy vs 读点），
// 这里以常驻形式钉住修复后不再复现。
func TestBinaryManagerProxyConcurrentAccess(t *testing.T) {
	bin := NewBinaryManager(&platform.Paths{}, "")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			bin.SetProxy("http://127.0.0.1:7890")
		}
	}()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = bin.proxySnapshot()
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	// 反向核对：快照读到的确实是写入值（不是恒空）。
	bin.SetProxy("  http://127.0.0.1:1080  ")
	if got := bin.proxySnapshot(); got != "http://127.0.0.1:1080" {
		t.Fatalf("SetProxy 应去掉首尾空白，实得 %q", got)
	}
}
