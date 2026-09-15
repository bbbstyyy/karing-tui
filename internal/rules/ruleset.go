// Package rules 实现规则集（RuleSet）管理：内置规则集初始化、
// 自定义远程 .srs/.json 规则集、下载与本地缓存。
// 生成的 sing-box 配置使用 local 类型引用缓存文件，运行时不依赖网络。
package rules

import (
	"context"
	"fmt"
	"github.com/bbbstyyy/karing-tui/internal/validation"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/catalog"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/core"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// BuiltinRuleSet 内置规则集定义（MetaCubeX meta-rules-dat，sing/geo 分支）。
type BuiltinRuleSet struct {
	Tag  string
	Name string
	URL  string
}

const rulesetBaseURL = "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo"

// BuiltinRuleSets 项目早期硬编码的 7 个内置规则集。
//
// 自 4.3 起改由 internal/catalog 提供全量分类库（geosite/geoip/acl），规则以
// "geosite:cn" 形式按需引用、按需下载，不再把固定几条写进 rulesets 表——故新库
// 不再初始化这些条目。此处保留定义仅为老库兼容：老库里已存在的同名条目继续有效，
// 且其 tag 与分类库派生的 tag 一致（geosite-cn 等），两条路径生成同一份配置。
var BuiltinRuleSets = []BuiltinRuleSet{
	{"geosite-cn", "中国大陆域名", rulesetBaseURL + "/geosite/cn.srs"},
	{"geoip-cn", "中国大陆 IP", rulesetBaseURL + "/geoip/cn.srs"},
	{"geosite-category-ads-all", "广告域名", rulesetBaseURL + "/geosite/category-ads-all.srs"},
	{"geosite-google", "Google", rulesetBaseURL + "/geosite/google.srs"},
	{"geosite-telegram", "Telegram 域名", rulesetBaseURL + "/geosite/telegram.srs"},
	{"geoip-telegram", "Telegram IP", rulesetBaseURL + "/geoip/telegram.srs"},
	{"geosite-category-ai-!cn", "AI 服务", rulesetBaseURL + "/geosite/category-ai-!cn.srs"},
}

// Manager 规则集管理器。
type Manager struct {
	DB    *storage.DB
	Paths *platform.Paths
	Proxy func() string // 返回当前下载代理地址
	Logf  func(format string, args ...any)

	// Fetch 下载实现，默认 core.FetchHTTP；测试可替换以避免真实联网。
	Fetch func(ctx context.Context, rawURL, proxyURL, userAgent string, limit int64) ([]byte, error)
}

// NewManager 创建规则集管理器。proxy/logf 可为 nil。
func NewManager(db *storage.DB, paths *platform.Paths, proxy func() string, logf func(string, ...any)) *Manager {
	if proxy == nil {
		proxy = func() string { return "" }
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{DB: db, Paths: paths, Proxy: proxy, Logf: logf, Fetch: core.FetchHTTP}
}

// fetch 执行下载，未注入时回落到 core.FetchHTTP。
func (m *Manager) fetch(ctx context.Context, rawURL string) ([]byte, error) {
	f := m.Fetch
	if f == nil {
		f = core.FetchHTTP
	}
	return f(ctx, rawURL, m.Proxy(), "", 32<<20)
}

// EnsureBuiltinRuleSets 老库兼容占位：4.3 起内置分类由 internal/catalog 按需提供，
// 新库不再把固定 7 条写入 rulesets 表（rulesets 表此后只存用户自定义规则集）。
// 老库里已有的条目不动，继续按自定义规则集处理。
func (m *Manager) EnsureBuiltinRuleSets() error { return nil }

// AddRuleSet 新增自定义远程规则集。
func (m *Manager) AddRuleSet(name, tag, rawURL, format string) (*config.RuleSet, error) {
	rs := &config.RuleSet{
		Name:       strings.TrimSpace(name),
		Tag:        strings.TrimSpace(tag),
		SourceType: "remote",
		Format:     strings.TrimSpace(format),
		URL:        strings.TrimSpace(rawURL),
		Enabled:    true,
	}
	if rs.Name == "" || rs.Tag == "" {
		return nil, validation.New("name", "名称与 Tag 为必填项")
	}
	if rs.Format == "" {
		rs.Format = "srs"
	}
	if rs.Format != "srs" && rs.Format != "json" {
		return nil, validation.New("format", "格式仅支持 srs/json")
	}
	if rs.URL == "" {
		return nil, validation.New("url", "远程规则集必须提供 URL")
	}
	existing, err := m.DB.ListRuleSets()
	if err != nil {
		return nil, err
	}
	for _, e := range existing {
		if e.Tag == rs.Tag {
			return nil, validation.New("tag", "规则集 Tag %q 已存在", rs.Tag)
		}
	}
	// tag 与内置分类的派生 tag 冲突时，规则里写这个 tag 会优先解析为自定义规则集，
	// 分类引用被静默遮蔽——写入前就拒绝，避免用户困惑。
	if ref, ok := catalog.Parse(rs.Tag); ok && ref.Tag() == rs.Tag {
		return nil, validation.New("tag", "规则集 Tag %q 与内置分类 %s 冲突，请换一个（内置分类无需添加，规则里直接写 %s 即可）", rs.Tag, ref, ref)
	}
	if err := m.DB.CreateRuleSet(rs); err != nil {
		return nil, err
	}
	m.Logf("添加规则集 %q (%s)", rs.Name, rs.Tag)
	return rs, nil
}

// DeleteRuleSet 删除规则集；被分流规则引用时拒绝。
func (m *Manager) DeleteRuleSet(id int64) error {
	rs, err := m.getRuleSet(id)
	if err != nil {
		return err
	}
	routings, err := m.DB.ListRoutingGroups()
	if err != nil {
		return err
	}
	refs := func(value string) bool {
		for _, v := range strings.Split(value, ",") {
			if strings.TrimSpace(v) == rs.Tag {
				return true
			}
		}
		return false
	}
	for _, rg := range routings {
		for _, r := range rg.Rules {
			if r.Type == "rule_set" && refs(r.Value) {
				return fmt.Errorf("分流组 %q 的规则正在引用规则集 %q", rg.Name, rs.Tag)
			}
			for _, c := range r.Conditions {
				if c.Type == "rule_set" && refs(c.Value) {
					return fmt.Errorf("分流组 %q 的逻辑规则正在引用规则集 %q", rg.Name, rs.Tag)
				}
			}
		}
	}
	dnsRules, err := m.DB.ListDNSRules()
	if err != nil {
		return err
	}
	for _, r := range dnsRules {
		if r.Type == "rule_set" && refs(r.Value) {
			return fmt.Errorf("DNS 规则正在引用规则集 %q", rs.Tag)
		}
	}
	if err := m.DB.DeleteRuleSet(id); err != nil {
		return err
	}
	m.Logf("删除规则集 %q", rs.Name)
	return nil
}

// SetEnabled 启用/停用规则集。
func (m *Manager) SetEnabled(id int64, enabled bool) error {
	rs, err := m.getRuleSet(id)
	if err != nil {
		return err
	}
	rs.Enabled = enabled
	return m.DB.UpdateRuleSet(rs)
}

// Download 下载（或更新）规则集到本地缓存 cache/rulesets/<tag>.<fmt>。
func (m *Manager) Download(ctx context.Context, id int64) error {
	rs, err := m.getRuleSet(id)
	if err != nil {
		return err
	}
	if rs.SourceType != "remote" {
		return fmt.Errorf("本地规则集无需下载")
	}
	if strings.TrimSpace(rs.Tag) != rs.Tag || strings.ContainsAny(rs.Tag, "/\\:\x00") || rs.Tag == "." || rs.Tag == ".." || rs.Tag == "" {
		return fmt.Errorf("规则集 Tag 非法，拒绝写入缓存路径")
	}
	data, err := m.fetch(ctx, rs.URL)
	if err != nil {
		return fmt.Errorf("下载规则集 %q 失败: %w", rs.Name, err)
	}
	dir := filepath.Join(m.Paths.Cache, "rulesets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建规则集缓存目录失败: %w", err)
	}
	path := filepath.Join(dir, rs.Tag+"."+rs.Format)
	if err := writeCacheFile(path, data); err != nil {
		return fmt.Errorf("写入规则集缓存失败: %w", err)
	}
	rs.CachedPath = path
	rs.UpdatedAt = time.Now()
	if err := m.DB.UpdateRuleSet(rs); err != nil {
		return err
	}
	m.Logf("规则集 %q 已缓存: %s（%d 字节）", rs.Name, path, len(data))
	return nil
}

// DownloadAll 下载全部启用的远程规则集；单个失败不影响其余，返回首个错误。
func (m *Manager) DownloadAll(ctx context.Context) error {
	list, err := m.DB.ListRuleSets()
	if err != nil {
		return err
	}
	var firstErr error
	for _, rs := range list {
		if !rs.Enabled || rs.SourceType != "remote" {
			continue
		}
		if err := m.Download(ctx, rs.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// EnsureCached 确保启用的远程规则集都有本地缓存（配置生成前调用，2.6 用）。
// 缺缓存的现场下载。同时补全被规则引用到的内置分类（4.3）。
func (m *Manager) EnsureCached(ctx context.Context) error {
	list, err := m.DB.ListRuleSets()
	if err != nil {
		return err
	}
	refs, err := m.ReferencedCustomTags()
	if err != nil {
		return err
	}
	for _, rs := range list {
		if !rs.Enabled || rs.SourceType != "remote" || !refs[rs.Tag] {
			continue
		}
		if rs.CachedPath != "" {
			if _, err := os.Stat(rs.CachedPath); err == nil {
				continue
			}
		}
		if err := m.Download(ctx, rs.ID); err != nil {
			return err
		}
	}
	return m.EnsureCatalogCached(ctx)
}

// ReferencedCustomTags 返回当前启用路由/DNS规则实际引用的自定义规则集。
// 未被引用的规则集不应阻塞配置生成或触发无意义的网络下载。
func (m *Manager) ReferencedCustomTags() (map[string]bool, error) {
	custom := map[string]bool{}
	sets, err := m.DB.ListRuleSets()
	if err != nil {
		return nil, err
	}
	for _, rs := range sets {
		if rs.Enabled {
			custom[rs.Tag] = true
		}
	}
	refs := map[string]bool{}
	collect := func(value string) {
		for _, v := range strings.Split(value, ",") {
			v = strings.TrimSpace(v)
			if custom[v] {
				refs[v] = true
			}
		}
	}
	groups, err := m.DB.ListRoutingGroups()
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if !g.Enabled {
			continue
		}
		for _, r := range g.Rules {
			if !r.Enabled {
				continue
			}
			if r.Type == "rule_set" {
				collect(r.Value)
			}
			for _, c := range r.Conditions {
				if c.Type == "rule_set" {
					collect(c.Value)
				}
			}
		}
	}
	dnsRules, err := m.DB.ListDNSRules()
	if err != nil {
		return nil, err
	}
	for _, r := range dnsRules {
		if r.Enabled && r.Type == "rule_set" {
			collect(r.Value)
		}
	}
	return refs, nil
}

// --- 内置分类库（4.3）---

// CacheDir 返回规则集缓存目录 cache/rulesets。
func (m *Manager) CacheDir() string {
	return filepath.Join(m.Paths.Cache, "rulesets")
}

// CatalogCachePath 返回某内置分类的本地缓存路径。
func (m *Manager) CatalogCachePath(ref catalog.Ref) string {
	return filepath.Join(m.CacheDir(), ref.Tag()+".srs")
}

// CatalogCached 报告某内置分类已有本地缓存。
func (m *Manager) CatalogCached(ref catalog.Ref) bool {
	info, err := os.Stat(m.CatalogCachePath(ref))
	return err == nil && !info.IsDir()
}

// DownloadCatalog 下载（或更新）一个内置分类的规则集到本地缓存。
func (m *Manager) DownloadCatalog(ctx context.Context, ref catalog.Ref) error {
	data, err := m.fetch(ctx, ref.URL())
	if err != nil {
		return fmt.Errorf("下载分类规则集 %s 失败: %w", ref, err)
	}
	dir := m.CacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建规则集缓存目录失败: %w", err)
	}
	path := m.CatalogCachePath(ref)
	if err := writeCacheFile(path, data); err != nil {
		return fmt.Errorf("写入规则集缓存失败: %w", err)
	}
	m.Logf("分类规则集 %s 已缓存: %s（%d 字节）", ref, path, len(data))
	return nil
}

func writeCacheFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ruleset-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// ReferencedCatalog 扫描全部分流规则与 DNS 规则，返回被引用到的内置分类
// （去重、按 tag 排序）。未被任何规则引用的分类不下载、不进配置——这正是
// 「按需」的含义：分类库有两千余条，只有用到的才落地。
// 已在 rulesets 表中存在同名 tag 的（老库遗留的内置条目）跳过，由自定义规则集路径处理。
func (m *Manager) ReferencedCatalog() ([]catalog.Ref, error) {
	custom := map[string]bool{}
	sets, err := m.DB.ListRuleSets()
	if err != nil {
		return nil, err
	}
	for _, rs := range sets {
		custom[rs.Tag] = true
	}

	found := map[string]catalog.Ref{}
	collect := func(value string) {
		for _, v := range strings.Split(value, ",") {
			v = strings.TrimSpace(v)
			if v == "" || custom[v] {
				continue
			}
			if ref, ok := catalog.Parse(v); ok {
				found[ref.Tag()] = ref
			}
		}
	}

	groups, err := m.DB.ListRoutingGroups()
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if !g.Enabled {
			continue
		}
		for _, r := range g.Rules {
			if !r.Enabled {
				continue
			}
			if r.Type == "rule_set" {
				collect(r.Value)
			}
			for _, c := range r.Conditions {
				if c.Type == "rule_set" {
					collect(c.Value)
				}
			}
		}
	}

	dnsRules, err := m.DB.ListDNSRules()
	if err != nil {
		return nil, err
	}
	for _, r := range dnsRules {
		if r.Enabled && r.Type == "rule_set" {
			collect(r.Value)
		}
	}

	tags := make([]string, 0, len(found))
	for tag := range found {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	out := make([]catalog.Ref, 0, len(tags))
	for _, tag := range tags {
		out = append(out, found[tag])
	}
	return out, nil
}

// EnsureCatalogCached 为被规则引用到的内置分类补全本地缓存；已有缓存的跳过。
// 优先从编译期嵌入的规则集安装到用户缓存，因此首次启动不依赖网络；
// 对于当前内置快照未包含的较新分类，再回退到远程下载。单个失败不中断其余
// （生成器会为缺缓存的分类回退 remote 引用，由 sing-box 自行下载），返回首个错误。
func (m *Manager) EnsureCatalogCached(ctx context.Context) error {
	refs, err := m.ReferencedCatalog()
	if err != nil {
		return err
	}
	var firstErr error
	for _, ref := range refs {
		if m.CatalogCached(ref) {
			continue
		}
		if data, ok := ref.EmbeddedRuleSet(); ok {
			path := m.CatalogCachePath(ref)
			if err := writeCacheFile(path, data); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("安装内置分类规则集 %s 失败: %w", ref, err)
				}
				continue
			}
			m.Logf("内置分类规则集 %s 已从程序资源安装: %s（%d 字节）", ref, path, len(data))
			continue
		}
		if err := m.DownloadCatalog(ctx, ref); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// UpdateCatalogCached 重新下载已缓存的内置分类（规则集更新用），忽略未缓存的。
func (m *Manager) UpdateCatalogCached(ctx context.Context) error {
	refs, err := m.ReferencedCatalog()
	if err != nil {
		return err
	}
	var firstErr error
	for _, ref := range refs {
		if err := m.DownloadCatalog(ctx, ref); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *Manager) getRuleSet(id int64) (*config.RuleSet, error) {
	list, err := m.DB.ListRuleSets()
	if err != nil {
		return nil, err
	}
	for _, rs := range list {
		if rs.ID == id {
			return rs, nil
		}
	}
	return nil, storage.ErrNotFound
}
