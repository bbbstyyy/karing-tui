package storage

import (
	"github.com/bbbstyyy/karing-tui/internal/config"
)

// DefaultGroupAuto / DefaultGroupManual 默认代理组名。
const (
	DefaultGroupAuto   = "Auto"
	DefaultGroupManual = "Manual"
)

// EnsureDefaultGroups 首次使用（无任何代理组）时创建默认组：
// Auto（urltest 自动选择）与 Manual（select 手动选择），成员均为动态"全部节点"。
func (d *DB) EnsureDefaultGroups() error {
	n, err := d.TableCount("proxy_groups")
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	auto := &config.ProxyGroup{
		Name:      DefaultGroupAuto,
		Type:      "urltest",
		TestURL:   config.DefaultTestURL,
		IntervalS: 300,
		Members:   []config.ProxyGroupMember{{Type: "all"}},
	}
	if err := d.CreateProxyGroup(auto); err != nil {
		return err
	}
	manual := &config.ProxyGroup{
		Name:    DefaultGroupManual,
		Type:    "select",
		Members: []config.ProxyGroupMember{{Type: "all"}},
	}
	if err := d.CreateProxyGroup(manual); err != nil {
		return err
	}
	return nil
}
