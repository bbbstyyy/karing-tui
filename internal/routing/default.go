package routing

import (
	"fmt"

	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// EnsureDefaultRouting 首次使用（无任何分流组）时初始化默认分流方案：
// 直接采用主用地区（中国大陆）的预置方案 —— 27 个分流组（其中 6 个启用）
// 全部归 custom 层，外加一个 kind='final'、目标为 Manual 的兜底组。
//
// 已装用户的既有方案**不会被改写**：本函数只在库中没有任何分流组时生效
// （application/app.go 是唯一调用点）。想换用地区方案必须显式对齐：
// `karing route preset cn [--merge|--replace]` 或 TUI Rules 页的预置入口。
//
// 依赖顺序：启动时先 storage.EnsureDefaultGroups 建出 Auto / Manual 代理组，
// 再调用本函数，故 final 的目标必然存在；ApplyPreset 仍会自行复核并在缺失时补建。
func EnsureDefaultRouting(db *storage.DB) error {
	n, err := db.TableCount("routing_groups")
	if err != nil {
		return fmt.Errorf("统计分流组数量失败: %w", err)
	}
	if n > 0 {
		return nil
	}
	m := NewManager(db, nil)
	report, err := m.ApplyPreset(PresetCN, PresetMerge)
	if err != nil {
		return fmt.Errorf("初始化默认分流方案失败: %w", err)
	}
	if report.Failed > 0 {
		return fmt.Errorf("初始化默认分流方案失败: %v", report.FailedItems())
	}
	return nil
}
