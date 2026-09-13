package subscription

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// Manager 编排订阅的增删改查与更新（下载 → 解析 → 入池）。
type Manager struct {
	DB       *storage.DB
	Proxy    func() string // 返回用户配置的下载代理地址（如 Settings.DownloadProxy）
	Logf     func(format string, args ...any)
	AutoTest func(context.Context, int64) error
}

// NewManager 创建订阅管理器。proxy/logf 可为 nil。
func NewManager(db *storage.DB, proxy func() string, logf func(string, ...any)) *Manager {
	if proxy == nil {
		proxy = func() string { return "" }
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{DB: db, Proxy: proxy, Logf: logf}
}

// Add 新建订阅。
func (m *Manager) Add(ctx context.Context, name, rawURL, userAgent string) (*config.Subscription, error) {
	return m.AddWithStrategy(ctx, name, rawURL, userAgent, config.DefaultDownloadStrategy)
}

// AddWithStrategy 新建订阅并设置其下载通道策略。
func (m *Manager) AddWithStrategy(ctx context.Context, name, rawURL, userAgent, strategy string) (*config.Subscription, error) {
	name = strings.TrimSpace(name)
	rawURL = strings.TrimSpace(rawURL)
	if name == "" {
		return nil, fmt.Errorf("订阅名称不能为空")
	}
	if rawURL == "" {
		return nil, fmt.Errorf("订阅 URL 不能为空")
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	strategy, err := config.NormalizeDownloadStrategy(strategy)
	if err != nil {
		return nil, err
	}
	s := &config.Subscription{Name: name, URL: rawURL, UserAgent: strings.TrimSpace(userAgent), DownloadStrategy: strategy}
	if err := m.DB.CreateSubscription(s); err != nil {
		return nil, err
	}
	m.Logf("添加订阅 %q: %s", name, rawURL)
	return s, nil
}

// Edit 更新订阅的可编辑字段（名称/URL/UA/启用状态）。
func (m *Manager) Edit(s *config.Subscription) error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("订阅名称不能为空")
	}
	strategy, err := config.NormalizeDownloadStrategy(s.DownloadStrategy)
	if err != nil {
		return err
	}
	s.DownloadStrategy = strategy
	if err := m.DB.UpdateSubscription(s); err != nil {
		return err
	}
	m.Logf("修改订阅 %q", s.Name)
	return nil
}

// SetEnabled 启用/停用订阅。
func (m *Manager) SetEnabled(id int64, enabled bool) error {
	s, err := m.DB.GetSubscription(id)
	if err != nil {
		return err
	}
	s.Enabled = enabled
	if err := m.DB.UpdateSubscription(s); err != nil {
		return err
	}
	state := "启用"
	if !enabled {
		state = "停用"
	}
	m.Logf("订阅 %q 已%s", s.Name, state)
	return nil
}

// Delete 删除订阅；其节点随外键级联删除。
func (m *Manager) Delete(id int64) error {
	s, err := m.DB.GetSubscription(id)
	if err != nil {
		return err
	}
	if err := m.DB.DeleteSubscription(id); err != nil {
		return err
	}
	m.Logf("删除订阅 %q（节点已级联删除）", s.Name)
	return nil
}

// Update 下载并解析订阅，替换其节点池；返回更新后的订阅。
// 旧节点中 name+protocol+server+port 匹配的禁用状态与测速结果会被保留。
func (m *Manager) Update(ctx context.Context, id int64) (*config.Subscription, error) {
	s, err := m.DB.GetSubscription(id)
	if err != nil {
		return nil, err
	}
	if s.URL == "" {
		return nil, fmt.Errorf("订阅 %q 缺少 URL", s.Name)
	}

	dl := &Downloader{ProxyURL: m.Proxy(), Strategy: s.DownloadStrategy}
	body, headers, err := dl.FetchMeta(ctx, s.URL, s.UserAgent)
	if err != nil {
		return nil, err
	}
	nodes, err := ParseContent(string(body))
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		n.SubscriptionID = s.ID
		n.Enabled = true
	}
	// 保留匹配旧节点的禁用状态与测速结果
	old, err := m.DB.ListNodes(s.ID)
	if err != nil {
		return nil, err
	}
	state := map[string]*config.Node{}
	for _, o := range old {
		state[nodeKey(o)] = o
	}

	matched := map[int64]bool{}
	for _, n := range nodes {
		if o, ok := state[nodeKey(n)]; ok && !matched[o.ID] {
			matched[o.ID] = true
			// 保留节点 ID，使显式代理组成员和 select 选中项在刷新后仍然有效。
			n.ID = o.ID
			n.Enabled = o.Enabled
			if !o.LastTested.IsZero() {
				n.LatencyMS = o.LatencyMS
				n.LastTested = o.LastTested
			}
		}
	}
	if nodes, err = applyNodePolicy(nodes, s.NodeFilter, s.SortBy); err != nil {
		return nil, err
	}
	created := len(nodes)
	if err := m.DB.ReplaceSubscriptionNodes(s.ID, nodes, time.Now()); err != nil {
		return nil, err
	}
	if info, ok := parseUserInfo(headers); ok {
		u, d, total, exp := s.TrafficUpload, s.TrafficDownload, s.TrafficTotal, s.ExpireAt
		if info.hasUpload {
			u = info.upload
		}
		if info.hasDownload {
			d = info.download
		}
		if info.hasTotal {
			total = info.total
		}
		if info.hasExpire {
			exp = info.expire
		}
		_ = m.DB.UpdateSubscriptionTraffic(s.ID, u, d, total, exp)
	}
	if s.AutoTest && m.AutoTest != nil {
		if err := m.AutoTest(ctx, s.ID); err != nil {
			m.Logf("订阅 %q 自动测速失败: %v", s.Name, err)
		}
	}
	if s.AutoClean {
		if n, err := m.DB.DeleteFailedNodesBySubscription(s.ID); err == nil && n > 0 {
			m.Logf("订阅 %q 清理失效节点: %d", s.Name, n)
		}
	}
	m.Logf("订阅 %q 更新完成: %d 节点", s.Name, created)
	return m.DB.GetSubscription(s.ID)
}

func applyNodePolicy(nodes []*config.Node, expr, sortBy string) ([]*config.Node, error) {
	terms := []string{}
	for _, t := range strings.Split(expr, ",") {
		if t = strings.TrimSpace(t); t != "" {
			terms = append(terms, t)
		}
	}
	if len(terms) > 0 {
		compiled := make([]struct {
			re  *regexp.Regexp
			neg bool
		}, 0, len(terms))
		for _, term := range terms {
			neg := strings.HasPrefix(term, "!")
			if neg {
				term = strings.TrimSpace(term[1:])
			}
			re, err := regexp.Compile("(?i)" + term)
			if err != nil {
				return nil, fmt.Errorf("订阅节点过滤器 %q 非法: %w", term, err)
			}
			compiled = append(compiled, struct {
				re  *regexp.Regexp
				neg bool
			}{re, neg})
		}
		filtered := nodes[:0]
		for _, n := range nodes {
			text := n.Name + " " + n.Server + " " + n.Protocol
			matchedPositive, hasPositive, excluded := false, false, false
			for _, c := range compiled {
				if c.re.MatchString(text) {
					if c.neg {
						excluded = true
					} else {
						hasPositive = true
						matchedPositive = true
					}
				} else if !c.neg {
					hasPositive = true
				}
			}
			include := !excluded && (!hasPositive || matchedPositive)
			if include {
				filtered = append(filtered, n)
			}
		}
		nodes = filtered
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		if sortBy == "latency" && nodes[i].LatencyMS != nodes[j].LatencyMS {
			ai, aj := nodes[i].LatencyMS, nodes[j].LatencyMS
			if ai < 0 {
				return false
			}
			if aj < 0 {
				return true
			}
			return ai < aj
		}
		return nodes[i].Name < nodes[j].Name
	})
	return nodes, nil
}

type userInfo struct {
	upload, download, total int64
	expire                  time.Time
	hasUpload               bool
	hasDownload             bool
	hasTotal                bool
	hasExpire               bool
}

func parseUserInfo(h http.Header) (userInfo, bool) {
	v := h.Get("subscription-userinfo")
	if v == "" {
		return userInfo{}, false
	}
	var info userInfo
	for _, p := range strings.Split(v, ";") {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) != 2 {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(kv[1]), 10, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(kv[0]) {
		case "upload":
			info.upload, info.hasUpload = n, true
		case "download":
			info.download, info.hasDownload = n, true
		case "total":
			info.total, info.hasTotal = n, true
		case "expire":
			info.expire, info.hasExpire = time.Unix(n, 0), true
		}
	}
	return info, info.hasUpload || info.hasDownload || info.hasTotal || info.hasExpire
}

// nodeKey 节点匹配键：协议|服务器|端口。
// 展示名称可能在订阅刷新时变化，不能参与身份匹配；这与 Karing 的
// type;server;serverport 禁用状态键保持一致。
func nodeKey(n *config.Node) string {
	return strings.ToLower(strings.TrimSpace(n.Protocol)) + "|" + strings.ToLower(strings.TrimSpace(n.Server)) + "|" + strconv.Itoa(n.Port)
}
