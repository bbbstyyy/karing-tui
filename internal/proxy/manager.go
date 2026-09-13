// Package proxy 实现节点管理：手动节点增删改、启停、搜索过滤与延迟测试。
package proxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/core"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/storage"
	"github.com/bbbstyyy/karing-tui/internal/subscription"
)

// Manager 节点管理器。
type Manager struct {
	DB    *storage.DB
	Paths *platform.Paths
	Bin   *core.BinaryManager
	Logf  func(format string, args ...any)
}

// NewManager 创建节点管理器。logf 可为 nil。
func NewManager(db *storage.DB, paths *platform.Paths, bin *core.BinaryManager, logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{DB: db, Paths: paths, Bin: bin, Logf: logf}
}

// AddFromLinks 导入分享链接为手动节点（支持多行/空白分隔的多条链接）。
// 返回导入数量；全部失败时返回错误。
func (m *Manager) AddFromLinks(content string) (int, error) {
	nodes, err := subscription.ParseContent(content)
	if err != nil {
		return 0, err
	}
	added := 0
	for _, n := range nodes {
		n.SubscriptionID = config.ManualSubscriptionID
		n.Enabled = true
		if err := m.DB.CreateNode(n); err != nil {
			return added, fmt.Errorf("写入节点 %q 失败: %w", n.Name, err)
		}
		added++
	}
	if added > 0 {
		m.Logf("手动导入 %d 个节点", added)
	}
	return added, nil
}

// SaveManual 保存手动节点（表单录入/编辑）。ID 为 0 新建，否则更新。
func (m *Manager) SaveManual(n *config.Node) error {
	if n == nil {
		return fmt.Errorf("节点不能为空")
	}
	if strings.TrimSpace(n.Server) == "" || n.Port <= 0 || n.Port > 65535 {
		return fmt.Errorf("服务器或端口非法")
	}
	n.Protocol = strings.ToLower(strings.TrimSpace(n.Protocol))
	if err := validateManualProtocol(n); err != nil {
		return err
	}
	n.Name = strings.TrimSpace(n.Name)
	if n.Name == "" {
		n.Name = fmt.Sprintf("%s:%d", n.Server, n.Port)
	}
	n.SubscriptionID = config.ManualSubscriptionID
	if n.Metadata == nil {
		n.Metadata = map[string]any{}
	}
	if n.ID == 0 {
		n.Enabled = true
		if err := m.DB.CreateNode(n); err != nil {
			return err
		}
		m.Logf("添加手动节点 %q (%s)", n.Name, n.Protocol)
		return nil
	}
	if err := m.DB.UpdateNode(n); err != nil {
		return err
	}
	m.Logf("修改手动节点 %q", n.Name)
	return nil
}

func validateManualProtocol(n *config.Node) error {
	str := func(key string) string {
		if v, ok := n.Metadata[key].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	switch n.Protocol {
	case "shadowsocks":
		if str("method") == "" || str("password") == "" {
			return fmt.Errorf("shadowsocks 节点缺少加密方法或密码")
		}
	case "vmess", "vless":
		if str("uuid") == "" {
			return fmt.Errorf("%s 节点缺少 UUID", n.Protocol)
		}
	case "trojan", "hysteria2", "anytls", "shadowtls":
		if str("password") == "" {
			return fmt.Errorf("%s 节点缺少密码", n.Protocol)
		}
	case "tuic":
		if str("uuid") == "" {
			return fmt.Errorf("tuic 节点缺少 UUID")
		}
	case "ssh":
		if str("user") == "" && str("username") == "" {
			return fmt.Errorf("ssh 节点缺少用户名")
		}
	case "socks", "socks5", "http", "hysteria", "naive":
	case "tor":
		return fmt.Errorf("tor 节点不支持手动录入")
	default:
		return fmt.Errorf("协议 %q 不支持手动录入", n.Protocol)
	}
	return nil
}

// Delete 删除节点。
func (m *Manager) Delete(id int64) error {
	n, err := m.DB.GetNode(id)
	if err != nil {
		return err
	}
	if err := m.DB.DeleteNode(id); err != nil {
		return err
	}
	m.Logf("删除节点 %q", n.Name)
	return nil
}

// SetEnabled 启用/禁用节点。
func (m *Manager) SetEnabled(id int64, enabled bool) error {
	n, err := m.DB.GetNode(id)
	if err != nil {
		return err
	}
	n.Enabled = enabled
	if err := m.DB.UpdateNode(n); err != nil {
		return err
	}
	state := "启用"
	if !enabled {
		state = "禁用"
	}
	m.Logf("节点 %q 已%s", n.Name, state)
	return nil
}

// TestLatency 批量测速并写回数据库；返回 节点ID→延迟毫秒（失败为 -1）。
// url 为空用默认测速地址；timeoutMS 为单节点超时（为 0 用 3000）。
func (m *Manager) TestLatency(ctx context.Context, ids []int64, url string, timeoutMS int) (map[int64]int64, error) {
	nodes := make([]*config.Node, 0, len(ids))
	for _, id := range ids {
		n, err := m.DB.GetNode(id)
		if err != nil {
			continue
		}
		nodes = append(nodes, n)
	}
	results, err := m.testNodes(ctx, nodes, url, timeoutMS)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for id, ms := range results {
		if err := m.DB.UpdateNodeLatency(id, ms, now); err != nil {
			return results, err
		}
	}
	m.Logf("节点测速完成: %d 个", len(results))
	return results, nil
}
