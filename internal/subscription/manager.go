package subscription

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/storage"
	"github.com/bbbstyyy/karing-tui/internal/validation"
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
		return nil, validation.New("name", "订阅名称不能为空")
	}
	var err error
	if rawURL, err = normalizeSubscriptionURL(rawURL); err != nil {
		return nil, err
	}
	strategy, err = config.NormalizeDownloadStrategy(strategy)
	if err != nil {
		return nil, validation.At("download_strategy", err)
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
		return validation.New("name", "订阅名称不能为空")
	}
	strategy, err := config.NormalizeDownloadStrategy(s.DownloadStrategy)
	if err != nil {
		return validation.At("download_strategy", err)
	}
	rawURL, err := normalizeSubscriptionURL(s.URL)
	if err != nil {
		return err
	}
	if err := ValidateNodeFilter(s.NodeFilter); err != nil {
		return err
	}
	s.Name, s.URL = strings.TrimSpace(s.Name), rawURL
	s.DownloadStrategy = strategy
	if err := m.DB.UpdateSubscription(s); err != nil {
		return err
	}
	m.Logf("修改订阅 %q", s.Name)
	return nil
}

func normalizeSubscriptionURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", validation.New("url", "订阅 URL 不能为空")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", validation.New("url", "请填写有效的 HTTP/HTTPS 订阅 URL")
	}
	return raw, nil
}

// ValidateNodeFilter uses the same grammar as subscription updates, so an
// invalid filter is rejected before any subscription fields are saved.
func ValidateNodeFilter(expr string) error {
	_, err := applyNodePolicy(nil, expr, "name")
	return validation.At("filter", err)
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
// 旧节点中「连接语义指纹」相同的节点会被继承 ID、禁用状态与测速结果
// （见 nodeFingerprint）；展示名称与端点都不参与身份匹配。
//
// 节点替换、订阅状态与（若有）流量元数据在同一事务里提交：任一步失败整次刷新回滚，
// 不会出现「节点已换、流量未更新却报成功」的半状态。
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
	// 保留匹配旧节点的禁用状态与测速结果。
	//
	// 身份按「连接语义指纹」分桶：同 endpoint 不同凭据的节点落在不同桶里，
	// 不会互相错绑。只有指纹完全相同的重复节点才需要桶内再分配，
	// 分配规则是「先同展示名、再取最小未使用 ID」，保证确定性且每个旧 ID 只消费一次。
	old, err := m.DB.ListNodes(s.ID)
	if err != nil {
		return nil, err
	}
	buckets := map[string][]*config.Node{}
	for _, o := range old {
		fp, err := nodeFingerprint(o)
		if err != nil {
			// 不降级成旧的 endpoint 键：算不出指纹说明节点本身不合法，
			// 此时沿用低精度身份正是本函数要修的故障。宁可让刷新失败。
			return nil, err
		}
		buckets[fp] = append(buckets[fp], o)
	}
	for fp, bucket := range buckets {
		// 按 ID 升序稳定排序，使「取最小未使用 ID」是确定性的。
		sort.SliceStable(bucket, func(i, j int) bool { return bucket[i].ID < bucket[j].ID })
		buckets[fp] = bucket
	}

	for _, n := range nodes {
		fp, err := nodeFingerprint(n)
		if err != nil {
			return nil, err
		}
		o, rest := takeBucketNode(buckets[fp], n.Name)
		if o == nil {
			continue
		}
		buckets[fp] = rest
		// 保留节点 ID，使显式代理组成员和 select 选中项在刷新后仍然有效。
		n.ID = o.ID
		n.Enabled = o.Enabled
		if !o.LastTested.IsZero() {
			n.LatencyMS = o.LatencyMS
			n.LastTested = o.LastTested
		}
	}
	if nodes, err = applyNodePolicy(nodes, s.NodeFilter, s.SortBy); err != nil {
		return nil, err
	}
	// 流量元数据与节点替换放进同一个事务（V7-10）：写失败必须让整次刷新回滚，
	// 而不是「节点已换成新池、流量却还是旧值」还报成功。缺字段时保留旧值。
	meta := storage.SubscriptionRefreshMeta{}
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
		meta = storage.SubscriptionRefreshMeta{HasTraffic: true, Upload: u, Download: d, Total: total, ExpireAt: exp}
	}
	created := len(nodes)
	if err := m.DB.ReplaceSubscriptionNodesWithMeta(s.ID, nodes, time.Now(), meta); err != nil {
		return nil, err
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

// 节点身份匹配已迁到 identity.go 的 nodeFingerprint()：旧的
// protocol|server|port 键无法区分同端点的不同凭据，已于 v7 轮删除。
