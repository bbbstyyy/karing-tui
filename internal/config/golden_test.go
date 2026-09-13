package config

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// -update 重新生成 golden 文件（go test ./internal/config/ -update）
var update = flag.Bool("update", false, "重新生成 golden 快照文件")

// TestGenerateGolden 全量快照的快照测试：输出与 testdata/golden-config.json
// 逐字节一致。生成器改动（新增字段/调整结构）后执行 go test -update 更新基线，
// 并在 diff 中确认改动符合预期。
func TestGenerateGolden(t *testing.T) {
	out, err := Generate(testFullSnapshot(t))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	golden := filepath.Join("testdata", "golden-config.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, out, 0o644); err != nil {
			t.Fatalf("写入 golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("读取 golden 失败（先运行 go test ./internal/config/ -update 生成基线）: %v", err)
	}
	if !bytes.Equal(want, out) {
		// 找出第一处差异便于定位
		i := 0
		for i < len(want) && i < len(out) && want[i] == out[i] {
			i++
		}
		lo := i - 60
		if lo < 0 {
			lo = 0
		}
		hiW, hiO := i+60, i+60
		if hiW > len(want) {
			hiW = len(want)
		}
		if hiO > len(out) {
			hiO = len(out)
		}
		t.Fatalf("输出与 golden 不一致（位置 %d）:\n  golden: ...%s...\n  输出:   ...%s...",
			i, want[lo:hiW], out[lo:hiO])
	}
}
