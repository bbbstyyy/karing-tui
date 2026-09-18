// Package proxy 实现节点管理：手动节点增删改、启停、搜索过滤与延迟测试。
package proxy

import (
	"context"
	"errors"
	"fmt"
	"github.com/bbbstyyy/karing-tui/internal/validation"
	"strings"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/core"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/redact"
	"github.com/bbbstyyy/karing-tui/internal/storage"
	"github.com/bbbstyyy/karing-tui/internal/subscription"
)

// Manager 节点管理器。
type Manager struct {
	DB    *storage.DB
	Paths *platform.Paths
	Bin   *core.BinaryManager
	Logf  func(format string, args ...any)

	// LoadDNS 提供「当前已保存的 DNS 配置」，供独立节点测速的临时核心使用。
	//
	// 用函数注入而不是持有 *dns.Manager：proxy 只能依赖 config，直接依赖 dns 会形成
	// proxy ↔ dns 的包循环。应用初始化时注入 a.DNS.LoadConfig。
	//
	// 为 nil 时测速会直接报错（见 loadProbeDNS）：漏注入必须立刻暴露，而不是让独立
	// 测速静默退回「没有 DNS」的行为。返回空 DNSConfig 表示用户确实没配 DNS，
	// 那种情况与主核心一致，允许放行。
	LoadDNS func() (config.DNSConfig, error)

	// AllocatePort / StartAdhoc 是「测速 API 端口 + 临时核心」的可注入缝（V7-6）。
	// 零值取包级实现（core.FreePort / core.StartAdhoc），生产路径不设置它们。
	// 存在的理由：端口从分配到被内核真正 bind 之间有 TOCTOU 窗口，
	// 需要能构造「第一次被抢占、第二次成功」来验证重试确实生效。
	AllocatePort func() (int, error)
	StartAdhoc   func(ctx context.Context, bin, configPath, cacheDir string) (*core.Adhoc, error)
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

type LinkResult struct {
	Index int
	Name  string
	Err   error
}

// AddFromLinksDetailed accounts for every supplied link, including parse and
// persistence failures. Raw links (which contain credentials) stay out of results.
func (m *Manager) AddFromLinksDetailed(content string) []LinkResult {
	links := strings.Fields(content)
	results := make([]LinkResult, 0, len(links))
	for i, link := range links {
		res := LinkResult{Index: i + 1}
		nodes, err := subscription.ParseContent(link)
		if err == nil && len(nodes) != 1 {
			err = fmt.Errorf("每项应包含一条有效分享链接")
		}
		if err == nil {
			n := nodes[0]
			n.SubscriptionID = config.ManualSubscriptionID
			n.Enabled = true
			res.Name = n.Name
			err = m.DB.CreateNode(n)
		}
		res.Err = redact.Error(err)
		results = append(results, res)
	}
	return results
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
		if str("method") == "" {
			return validation.New("method", "shadowsocks 节点缺少加密方法")
		}
		if str("password") == "" {
			return validation.New("cred", "shadowsocks 节点缺少密码")
		}
	case "vmess", "vless":
		if str("uuid") == "" {
			return validation.New("cred", "%s 节点缺少 UUID", n.Protocol)
		}
	case "trojan", "hysteria2", "anytls", "shadowtls":
		if str("password") == "" {
			return validation.New("cred", "%s 节点缺少密码", n.Protocol)
		}
	case "tuic":
		if str("uuid") == "" {
			return validation.New("uuid", "tuic 节点缺少 UUID")
		}
	case "ssh":
		if str("user") == "" && str("username") == "" {
			return validation.New("user", "ssh 节点缺少用户名")
		}
	case "socks", "socks5", "http", "hysteria", "naive":
	case "tor":
		return validation.New("protocol", "tor 节点不支持手动录入")
	default:
		return validation.New("protocol", "协议 %q 不支持手动录入", n.Protocol)
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
// url 为空用默认测速地址；timeoutMS 为单节点超时（为 0 用 config.DefaultLatencyTimeoutMS）。
func (m *Manager) TestLatency(ctx context.Context, ids []int64, url string, timeoutMS int) (map[int64]int64, error) {
	return m.TestLatencyProgress(ctx, ids, url, timeoutMS, nil)
}

type NodeTestResult struct {
	ID        int64
	Name      string
	LatencyMS int64
	Err       error
}

// TestLatencyProgress reports each attempted node while the batch is running.
// The callback can run concurrently and must not mutate a TUI model.
//
// 顺序是硬约束（V7-7）：**先写库，再返回错误**。反过来会让「错误非 nil 就早退」的
// 调用方（以及本函数的未来维护者）跳过持久化——全部节点被 skip 时 last_tested
// 永远为空，AutoClean 的判据 `last_tested IS NOT NULL AND latency_ms < 0` 永不成立，
// 用户看到「每个节点都失败、清理却 0 个」。批量错误与持久化错误同时存在时两者都保留。
func (m *Manager) TestLatencyProgress(ctx context.Context, ids []int64, url string, timeoutMS int, report func(NodeTestResult)) (map[int64]int64, error) {
	nodes := make([]*config.Node, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		n, err := m.DB.GetNode(id)
		if err != nil {
			if report != nil {
				report(NodeTestResult{ID: id, Name: fmt.Sprintf("节点 %d", id), LatencyMS: -1, Err: err})
			}
			continue
		}
		nodes = append(nodes, n)
	}
	results, batchErr := m.testNodes(ctx, nodes, url, timeoutMS, report)

	now := time.Now()
	var persistErr error
	for id, ms := range results {
		if err := m.DB.UpdateNodeLatency(id, ms, now); err != nil {
			persistErr = err
			break
		}
	}
	switch {
	case batchErr != nil && persistErr != nil:
		// 两个都要保留：只报一个会让另一类问题彻底隐形。
		return results, errors.Join(batchErr, persistErr)
	case batchErr != nil:
		return results, batchErr
	case persistErr != nil:
		return results, persistErr
	}
	m.Logf("节点测速完成: %d 个", len(results))
	return results, nil
}
