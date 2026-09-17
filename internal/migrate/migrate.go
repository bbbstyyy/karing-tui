// Package migrate 实现配置迁移：从 Clash 配置或 sing-box 配置（含 Karing
// 导出的 sing-box 完整配置）导入节点、代理组与分流规则到内部模型。
// 导入是合并式的：与现有数据重名的组自动改名（追加序号），节点直接追加。
//
// 原子性（C14-AUDIT）：两个 Import 函数的全部写入在**单个事务**中完成，
// 任一步失败整体回滚、不留半套导入数据。此前的手工补偿回滚
// （记录已建 ID、失败后逐个删除）已删除——补偿删除自身可能失败且错误
// 无法可靠上报，事务由 SQLite 保证。解析（YAML/JSON）是内存计算，
// 允许留在事务内。
package migrate

import (
	"fmt"
)

// Report 汇总一次导入的结果。
type Report struct {
	Nodes    int      // 导入节点数
	Groups   int      // 导入代理组数
	Routings int      // 导入分流组数
	RuleSets int      // 导入规则集数
	Skipped  []string // 跳过项说明（不支持的类型/引用缺失等）
}

func (r *Report) skipf(format string, args ...any) {
	r.Skipped = append(r.Skipped, fmt.Sprintf(format, args...))
}

// Summary 单行结果摘要。
func (r Report) Summary() string {
	s := fmt.Sprintf("节点 %d · 代理组 %d · 分流组 %d · 规则集 %d · 跳过 %d",
		r.Nodes, r.Groups, r.Routings, r.RuleSets, len(r.Skipped))
	return s
}

// txProbe 仅供测试使用（C14-AUDIT 失败回滚验证）：导入事务内每次 DB 写之前
// 以操作名调用，返回错误即中止整个导入事务。生产路径恒为 nil。
var txProbe func(op string) error

func probe(op string) error {
	if txProbe != nil {
		return txProbe(op)
	}
	return nil
}
