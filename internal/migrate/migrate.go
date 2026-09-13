// Package migrate 实现配置迁移：从 Clash 配置或 sing-box 配置（含 Karing
// 导出的 sing-box 完整配置）导入节点、代理组与分流规则到内部模型。
// 导入是合并式的：与现有数据重名的组自动改名（追加序号），节点直接追加。
package migrate

import (
	"fmt"

	"github.com/bbbstyyy/karing-tui/internal/storage"
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

func rollbackImport(db *storage.DB, ruleSetIDs, routingIDs, groupIDs, nodeIDs []int64) {
	for i := len(routingIDs) - 1; i >= 0; i-- {
		_ = db.DeleteRoutingGroup(routingIDs[i])
	}
	for i := len(groupIDs) - 1; i >= 0; i-- {
		_ = db.DeleteProxyGroup(groupIDs[i])
	}
	for i := len(nodeIDs) - 1; i >= 0; i-- {
		_ = db.DeleteNode(nodeIDs[i])
	}
	for i := len(ruleSetIDs) - 1; i >= 0; i-- {
		_ = db.DeleteRuleSet(ruleSetIDs[i])
	}
}
