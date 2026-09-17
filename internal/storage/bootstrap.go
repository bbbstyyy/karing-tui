package storage

import (
	"context"
	"database/sql"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// DefaultGroupAuto / DefaultGroupManual 默认代理组名。
const (
	DefaultGroupAuto   = "Auto"
	DefaultGroupManual = "Manual"
)

// EnsureDefaultGroups 首次使用（无任何代理组）时创建默认组：
// Auto（urltest 自动选择）与 Manual（select 手动选择），成员均为动态"全部节点"。
// 两组在同一个事务中创建（C14-AUDIT）：否则中途失败会留下「只有 Auto」的
// 状态，且下次调用因 n>0 短路，Manual 将永久缺失。
func (d *DB) EnsureDefaultGroups() error {
	n, err := d.TableCount("proxy_groups")
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return d.WithTx(context.Background(), func(tx *sql.Tx) error {
		auto := &config.ProxyGroup{
			Name:      DefaultGroupAuto,
			Type:      "urltest",
			TestURL:   config.DefaultTestURL,
			IntervalS: 300,
			Members:   []config.ProxyGroupMember{{Type: "all"}},
		}
		if err := d.CreateProxyGroupTx(tx, auto); err != nil {
			return err
		}
		manual := &config.ProxyGroup{
			Name:    DefaultGroupManual,
			Type:    "select",
			Members: []config.ProxyGroupMember{{Type: "all"}},
		}
		return d.CreateProxyGroupTx(tx, manual)
	})
}
