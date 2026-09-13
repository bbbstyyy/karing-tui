package rules

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/catalog"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	db, err := storage.Open(paths)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewManager(db, paths, nil, nil)
}

// mustGroup 建一个带规则的分流组（绕过 routing 层校验，直接写库）。
func mustGroup(t *testing.T, m *Manager, name string, rules ...config.Rule) {
	t.Helper()
	if err := m.DB.CreateRoutingGroup(&config.RoutingGroup{
		Name: name, Target: "DIRECT", Enabled: true, Rules: rules,
	}); err != nil {
		t.Fatalf("创建分流组 %s: %v", name, err)
	}
}

// TestEnsureBuiltinRuleSetsNoLongerSeeds 4.3 起新库不再预置 7 条内置规则集记录——
// 内置分类改由 catalog 按需提供，rulesets 表只存用户自定义规则集。
func TestEnsureBuiltinRuleSetsNoLongerSeeds(t *testing.T) {
	m := newTestManager(t)

	if err := m.EnsureBuiltinRuleSets(); err != nil {
		t.Fatalf("EnsureBuiltinRuleSets: %v", err)
	}
	sets, err := m.DB.ListRuleSets()
	if err != nil {
		t.Fatalf("ListRuleSets: %v", err)
	}
	if len(sets) != 0 {
		t.Errorf("新库 rulesets 表应为空，实得 %d 条: %v", len(sets), sets)
	}
}

func TestFetchUsesConfiguredProxy(t *testing.T) {
	m := newTestManager(t)
	const proxyURL = "http://127.0.0.1:7890"
	m.Proxy = func() string { return proxyURL }
	var gotProxy string
	m.Fetch = func(_ context.Context, _ string, proxy, _ string, _ int64) ([]byte, error) {
		gotProxy = proxy
		return []byte("cached"), nil
	}

	if err := m.DownloadCatalog(context.Background(), catalog.Ref{Kind: catalog.KindGeosite, Code: "cn"}); err != nil {
		t.Fatalf("DownloadCatalog: %v", err)
	}
	if gotProxy != proxyURL {
		t.Fatalf("下载代理 = %q, 期望 %q", gotProxy, proxyURL)
	}
}

// TestReferencedCatalogScansRoutingAndDNS 只有被规则真正引用的分类才算数——
// 这正是「按需」：两千余条分类不会全部下载。
func TestReferencedCatalogScansRoutingAndDNS(t *testing.T) {
	m := newTestManager(t)

	mustGroup(t, m, "国内",
		config.Rule{Type: "rule_set", Value: "geosite:cn,geoip:cn", Enabled: true},
		config.Rule{Type: "domain_suffix", Value: "example.com", Enabled: true}, // 非 rule_set，忽略
	)
	mustGroup(t, m, "逻辑", config.Rule{
		Type: "logical", Mode: "and", Enabled: true,
		Conditions: []config.RuleCondition{
			{Type: "rule_set", Value: "acl:Netflix"}, // 子条件里的引用也要算
			{Type: "domain", Value: "a.com"},
		},
	})
	if err := m.DB.CreateDNSRule(&config.DNSRule{
		Type: "rule_set", Value: "geosite:category-ads-all", Server: "local", Enabled: true,
	}); err != nil {
		t.Fatalf("创建 DNS 规则: %v", err)
	}

	refs, err := m.ReferencedCatalog()
	if err != nil {
		t.Fatalf("ReferencedCatalog: %v", err)
	}
	got := make([]string, 0, len(refs))
	for _, r := range refs {
		got = append(got, r.Tag())
	}
	// 按 tag 排序返回，结果稳定
	want := []string{"acl-Netflix", "geoip-cn", "geosite-category-ads-all", "geosite-cn"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("引用中的分类 = %v, 期望 %v", got, want)
	}
}

// TestReferencedCatalogSkipsDisabled 停用的分流组与停用的规则不产生引用，
// 避免为用不上的分类下载缓存。
func TestReferencedCatalogSkipsDisabled(t *testing.T) {
	m := newTestManager(t)

	if err := m.DB.CreateRoutingGroup(&config.RoutingGroup{
		Name: "停用组", Target: "DIRECT", Enabled: false,
		Rules: []config.Rule{{Type: "rule_set", Value: "geosite:cn", Enabled: true}},
	}); err != nil {
		t.Fatal(err)
	}
	mustGroup(t, m, "启用组",
		config.Rule{Type: "rule_set", Value: "geoip:jp", Enabled: false}, // 规则停用
		config.Rule{Type: "rule_set", Value: "geoip:kr", Enabled: true},
	)

	refs, err := m.ReferencedCatalog()
	if err != nil {
		t.Fatalf("ReferencedCatalog: %v", err)
	}
	if len(refs) != 1 || refs[0].Tag() != "geoip-kr" {
		t.Errorf("引用中的分类 = %v, 期望仅 geoip-kr", refs)
	}
}

// TestReferencedCatalogSkipsCustomTag 自定义规则集占用了同名 tag 时，该值走自定义
// 规则集路径，不重复算作分类引用（否则会生成重复的 rule_set 条目）。
func TestReferencedCatalogSkipsCustomTag(t *testing.T) {
	m := newTestManager(t)

	if err := m.DB.CreateRuleSet(&config.RuleSet{
		Name: "自建 cn", Tag: "geosite-cn", SourceType: "remote", Format: "srs",
		URL: "https://example.com/cn.srs", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	mustGroup(t, m, "组", config.Rule{Type: "rule_set", Value: "geosite-cn", Enabled: true})

	refs, err := m.ReferencedCatalog()
	if err != nil {
		t.Fatalf("ReferencedCatalog: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("同名自定义规则集应接管，实得分类引用 %v", refs)
	}
}

// TestCatalogCachePath 缓存路径按派生 tag 命名，与老库内置规则集的缓存文件同名，
// 从而可直接复用已有缓存。
func TestCatalogCachePath(t *testing.T) {
	m := newTestManager(t)

	ref := catalog.Ref{Kind: catalog.KindGeosite, Code: "cn"}
	want := filepath.Join(m.Paths.Cache, "rulesets", "geosite-cn.srs")
	if got := m.CatalogCachePath(ref); got != want {
		t.Errorf("缓存路径 = %q, 期望 %q", got, want)
	}
	if m.CatalogCached(ref) {
		t.Error("尚未下载时 CatalogCached 应为 false")
	}
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !m.CatalogCached(ref) {
		t.Error("缓存文件存在时 CatalogCached 应为 true")
	}
}

// TestEnsureCatalogCachedDownloadsOnlyReferenced 只下载被引用的分类，且已有缓存的跳过。
func TestEnsureCatalogCachedInstallsEmbeddedAssets(t *testing.T) {
	m := newTestManager(t)

	mustGroup(t, m, "组", config.Rule{Type: "rule_set", Value: "geosite:cn,geoip:cn", Enabled: true})

	// 预置其中一个的缓存，应被跳过
	cnPath := m.CatalogCachePath(catalog.Ref{Kind: catalog.KindGeosite, Code: "cn"})
	if err := os.MkdirAll(filepath.Dir(cnPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cnPath, []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.EnsureCatalogCached(context.Background()); err != nil {
		t.Fatalf("EnsureCatalogCached: %v", err)
	}
	// 已缓存的内容未被覆盖
	if b, _ := os.ReadFile(cnPath); string(b) != "cached" {
		t.Errorf("已缓存文件被覆盖: %q", b)
	}
	// 新下载的落盘
	ipPath := m.CatalogCachePath(catalog.Ref{Kind: catalog.KindGeoIP, Code: "cn"})
	if b, err := os.ReadFile(ipPath); err != nil || len(b) == 0 {
		t.Errorf("geoip:cn 缓存 = %q (err=%v), 期望嵌入规则集内容", b, err)
	}
}

func TestEnsureCachedSkipsUnreferencedCustomRuleSet(t *testing.T) {
	m := newTestManager(t)
	if err := m.DB.CreateRuleSet(&config.RuleSet{
		Name: "未引用坏集", Tag: "unused", SourceType: "remote", Format: "srs",
		URL: "https://example.invalid/unused.srs", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.DB.CreateRuleSet(&config.RuleSet{
		Name: "已引用规则集", Tag: "used", SourceType: "remote", Format: "srs",
		URL: "https://example.invalid/used.srs", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	mustGroup(t, m, "引用组", config.Rule{Type: "rule_set", Value: "used", Enabled: true})
	m.Fetch = func(_ context.Context, rawURL, _, _ string, _ int64) ([]byte, error) {
		if strings.Contains(rawURL, "/unused") {
			return nil, os.ErrNotExist
		}
		return []byte("used"), nil
	}
	if err := m.EnsureCached(context.Background()); err != nil {
		t.Fatalf("未引用坏规则集不应阻塞缓存: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.Paths.Cache, "rulesets", "unused.srs")); !os.IsNotExist(err) {
		t.Fatalf("未引用规则集不应下载，stat err=%v", err)
	}
}

// TestUpdateCatalogCachedRefreshesAll 更新时连已有缓存一并重新下载。
func TestUpdateCatalogCachedRefreshesAll(t *testing.T) {
	var hits int
	m := newTestManager(t)
	m.Fetch = func(context.Context, string, string, string, int64) ([]byte, error) {
		hits++
		return []byte("new-data"), nil
	}
	mustGroup(t, m, "组", config.Rule{Type: "rule_set", Value: "geosite:cn", Enabled: true})

	path := m.CatalogCachePath(catalog.Ref{Kind: catalog.KindGeosite, Code: "cn"})
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.UpdateCatalogCached(context.Background()); err != nil {
		t.Fatalf("UpdateCatalogCached: %v", err)
	}
	if hits != 1 {
		t.Errorf("应重新下载 1 次，实际 %d 次", hits)
	}
	if b, _ := os.ReadFile(path); string(b) != "new-data" {
		t.Errorf("缓存未更新: %q", b)
	}
}

// TestAddRuleSetRejectsCatalogTagCollision 自定义 tag 与分类派生 tag 冲突时拒绝——
// 否则规则里写该 tag 会静默解析成自定义规则集，分类引用被遮蔽。
func TestAddRuleSetRejectsCatalogTagCollision(t *testing.T) {
	m := newTestManager(t)

	if _, err := m.AddRuleSet("撞名", "geosite-cn", "https://example.com/x.srs", "srs"); err == nil {
		t.Error("与内置分类冲突的 tag 应被拒绝")
	}
	// 不冲突的照常通过
	if _, err := m.AddRuleSet("正常", "my-rules", "https://example.com/x.srs", "srs"); err != nil {
		t.Errorf("普通 tag 应被接受: %v", err)
	}
	// 重复 tag 拒绝
	if _, err := m.AddRuleSet("重复", "my-rules", "https://example.com/y.srs", "srs"); err == nil {
		t.Error("重复 tag 应被拒绝")
	}
}

// TestDeleteRuleSetChecksLogicalAndDNSRefs 删除校验要覆盖逻辑子条件与 DNS 规则，
// 以及多值里的引用——漏掉任一处都会删出一个悬空引用、让核心启动失败。
func TestDeleteRuleSetChecksLogicalAndDNSRefs(t *testing.T) {
	t.Run("多值引用", func(t *testing.T) {
		m := newTestManager(t)
		rs := &config.RuleSet{Name: "X", Tag: "x-set", SourceType: "remote", Format: "srs",
			URL: "https://example.com/x.srs", Enabled: true}
		if err := m.DB.CreateRuleSet(rs); err != nil {
			t.Fatal(err)
		}
		mustGroup(t, m, "组", config.Rule{Type: "rule_set", Value: "geosite:cn,x-set", Enabled: true})
		if err := m.DeleteRuleSet(rs.ID); err == nil {
			t.Error("多值里的引用应阻止删除")
		}
	})

	t.Run("逻辑子条件引用", func(t *testing.T) {
		m := newTestManager(t)
		rs := &config.RuleSet{Name: "X", Tag: "x-set", SourceType: "remote", Format: "srs",
			URL: "https://example.com/x.srs", Enabled: true}
		if err := m.DB.CreateRuleSet(rs); err != nil {
			t.Fatal(err)
		}
		mustGroup(t, m, "组", config.Rule{
			Type: "logical", Mode: "and", Enabled: true,
			Conditions: []config.RuleCondition{{Type: "rule_set", Value: "x-set"}},
		})
		if err := m.DeleteRuleSet(rs.ID); err == nil {
			t.Error("逻辑子条件的引用应阻止删除")
		}
	})

	t.Run("DNS 规则引用", func(t *testing.T) {
		m := newTestManager(t)
		rs := &config.RuleSet{Name: "X", Tag: "x-set", SourceType: "remote", Format: "srs",
			URL: "https://example.com/x.srs", Enabled: true}
		if err := m.DB.CreateRuleSet(rs); err != nil {
			t.Fatal(err)
		}
		if err := m.DB.CreateDNSRule(&config.DNSRule{
			Type: "rule_set", Value: "x-set", Server: "local", Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
		if err := m.DeleteRuleSet(rs.ID); err == nil {
			t.Error("DNS 规则的引用应阻止删除")
		}
	})

	t.Run("无引用可删", func(t *testing.T) {
		m := newTestManager(t)
		rs := &config.RuleSet{Name: "X", Tag: "x-set", SourceType: "remote", Format: "srs",
			URL: "https://example.com/x.srs", Enabled: true}
		if err := m.DB.CreateRuleSet(rs); err != nil {
			t.Fatal(err)
		}
		if err := m.DeleteRuleSet(rs.ID); err != nil {
			t.Errorf("无引用时应可删除: %v", err)
		}
	})
}
