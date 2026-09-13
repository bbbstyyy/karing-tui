package catalog

import "testing"

// TestLoadCounts 清单能正常加载，且数量与上游一致（更新清单后需同步此处）。
func TestLoadCounts(t *testing.T) {
	load()
	if loadErr != nil {
		t.Fatalf("加载分类清单失败: %v", loadErr)
	}
	want := map[string]int{KindGeosite: 1899, KindGeoIP: 260, KindACL: 171}
	for kind, n := range want {
		if got := Count(kind); got != n {
			t.Errorf("%s 分类码数量 = %d, 期望 %d（更新清单后请同步此断言）", kind, got, n)
		}
	}
	if got := len(aclIP); got != 33 {
		t.Errorf("acl IP 类分类数 = %d, 期望 33", got)
	}
	// 排序保证 TUI 浏览顺序稳定
	codes := Codes(KindGeoIP)
	for i := 1; i < len(codes); i++ {
		if codes[i-1] > codes[i] {
			t.Fatalf("geoip 清单未排序: %q 在 %q 之前", codes[i-1], codes[i])
		}
	}
}

func TestKindValid(t *testing.T) {
	for _, k := range Kinds {
		if !KindValid(k) {
			t.Errorf("KindValid(%q) = false", k)
		}
	}
	for _, k := range []string{"", "GEOSITE", "site", "geo"} {
		if KindValid(k) {
			t.Errorf("KindValid(%q) = true, 期望 false", k)
		}
	}
}

func TestHas(t *testing.T) {
	cases := []struct {
		kind, code string
		want       bool
	}{
		{KindGeosite, "cn", true},
		{KindGeosite, "category-ai-!cn", true}, // 含 ! 的码
		{KindGeosite, "acer@cn", true},         // 含 @ 的码
		{KindGeosite, "不存在的码", false},
		{KindGeoIP, "cn", true},
		{KindGeoIP, "jp", true},
		{KindGeoIP, "category-ai-!cn", false}, // geosite 的码不在 geoip 里
		{KindACL, "ChinaDomain", true},
		{KindACL, "chinadomain", false}, // acl 码大小写敏感
		{"bogus", "cn", false},
	}
	for _, c := range cases {
		if got := Has(c.kind, c.code); got != c.want {
			t.Errorf("Has(%q, %q) = %v, 期望 %v", c.kind, c.code, got, c.want)
		}
	}
}

// TestParse 覆盖两种引用写法与非分类值。
func TestParse(t *testing.T) {
	cases := []struct {
		in       string
		wantOK   bool
		wantKind string
		wantCode string
	}{
		// 冒号形式（推荐）
		{"geosite:cn", true, KindGeosite, "cn"},
		{"geoip:jp", true, KindGeoIP, "jp"},
		{"acl:ChinaDomain", true, KindACL, "ChinaDomain"},
		{" geosite : cn ", true, KindGeosite, "cn"}, // 容忍空白
		{"GeoSite:cn", true, KindGeosite, "cn"},     // 种类名忽略大小写
		// 连字符形式（派生 tag，便于照抄生成的配置）
		{"geosite-cn", true, KindGeosite, "cn"},
		{"geoip-cn", true, KindGeoIP, "cn"},
		{"geosite-category-ai-!cn", true, KindGeosite, "category-ai-!cn"}, // 码自身含连字符
		{"geosite-geolocation-!cn", true, KindGeosite, "geolocation-!cn"},
		{"acl-ChinaDomain", true, KindACL, "ChinaDomain"},
		// 非分类引用：应交给自定义规则集处理
		{"", false, "", ""},
		{"my-rules", false, "", ""},
		{"geosite:压根不存在", false, "", ""},
		{"geosite-压根不存在", false, "", ""},
		{"bogus:cn", false, "", ""},
		{"cn", false, "", ""},
	}
	for _, c := range cases {
		r, ok := Parse(c.in)
		if ok != c.wantOK {
			t.Errorf("Parse(%q) ok = %v, 期望 %v", c.in, ok, c.wantOK)
			continue
		}
		if ok && (r.Kind != c.wantKind || r.Code != c.wantCode) {
			t.Errorf("Parse(%q) = {%s %s}, 期望 {%s %s}", c.in, r.Kind, r.Code, c.wantKind, c.wantCode)
		}
	}
}

// TestRefTagURL 派生 tag 与下载地址。tag 必须与项目早期硬编码的内置规则集一致，
// 否则老库的缓存文件与规则引用会对不上。
func TestRefTagURL(t *testing.T) {
	cases := []struct {
		ref      Ref
		wantTag  string
		wantURL  string
		wantShow string
	}{
		{Ref{KindGeosite, "cn"}, "geosite-cn",
			"https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geosite/cn.srs", "geosite:cn"},
		{Ref{KindGeoIP, "cn"}, "geoip-cn",
			"https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geoip/cn.srs", "geoip:cn"},
		{Ref{KindGeosite, "category-ai-!cn"}, "geosite-category-ai-!cn",
			"https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geosite/category-ai-!cn.srs", "geosite:category-ai-!cn"},
		{Ref{KindACL, "ChinaDomain"}, "acl-ChinaDomain",
			"https://github.com/KaringX/karing-ruleset/raw/sing/ACL4SSR/ChinaDomain.srs", "acl:ChinaDomain"},
	}
	for _, c := range cases {
		if got := c.ref.Tag(); got != c.wantTag {
			t.Errorf("%v.Tag() = %q, 期望 %q", c.ref, got, c.wantTag)
		}
		if got := c.ref.URL(); got != c.wantURL {
			t.Errorf("%v.URL() = %q, 期望 %q", c.ref, got, c.wantURL)
		}
		if got := c.ref.String(); got != c.wantShow {
			t.Errorf("%v.String() = %q, 期望 %q", c.ref, got, c.wantShow)
		}
	}
}

func TestDefaultRefsHaveEmbeddedRuleSets(t *testing.T) {
	refs := []Ref{
		{Kind: KindGeosite, Code: "cn"},
		{Kind: KindGeoIP, Code: "cn"},
		{Kind: KindGeosite, Code: "telegram"},
		{Kind: KindGeoIP, Code: "telegram"},
		{Kind: KindGeosite, Code: "category-ai-!cn"},
	}
	for _, ref := range refs {
		b, ok := ref.EmbeddedRuleSet()
		if !ok || len(b) == 0 {
			t.Errorf("默认规则集 %s 未嵌入或内容为空", ref)
		}
	}
}

// TestParseTagRoundTrip 派生 tag 必须能被 Parse 还原，否则生成的配置里的 tag
// 无法反查回分类（规则集管理页据此展示引用中的分类）。
func TestParseTagRoundTrip(t *testing.T) {
	for _, kind := range Kinds {
		codes := Codes(kind)
		for _, code := range codes {
			ref := Ref{Kind: kind, Code: code}
			got, ok := RefByTag(ref.Tag())
			if !ok {
				t.Fatalf("RefByTag(%q) 解析失败", ref.Tag())
			}
			if got != ref {
				t.Fatalf("RefByTag(%q) = %v, 期望 %v", ref.Tag(), got, ref)
			}
		}
	}
}

// TestNeedsResolve IP 类判定按实测清单，而非 tag 字面含 "ip"。
func TestNeedsResolve(t *testing.T) {
	cases := []struct {
		ref  Ref
		want bool
	}{
		{Ref{KindGeoIP, "cn"}, true},
		{Ref{KindGeoIP, "telegram"}, true},
		{Ref{KindGeosite, "cn"}, false},
		{Ref{KindGeosite, "category-ai-!cn"}, false},
		// acl 混合：名字不含 ip 却含 ip_cidr 的分类——仅凭字面判断会漏判
		{Ref{KindACL, "ChinaDomain"}, true},
		{Ref{KindACL, "Apple"}, true},
		{Ref{KindACL, "Telegram"}, true},
		{Ref{KindACL, "ChinaIp"}, true},
		// acl 纯域名分类：名字含 "ip" 却无 IP 条件——仅凭字面判断会误判
		{Ref{KindACL, "Wikipedia"}, false},
		{Ref{KindACL, "Vip"}, false},
		{Ref{KindACL, "BBCiPlayer"}, false},
		{Ref{KindACL, "MIUIPrivacy"}, false},
		{Ref{KindACL, "Steam"}, false},
	}
	for _, c := range cases {
		if got := c.ref.NeedsResolve(); got != c.want {
			t.Errorf("%s.NeedsResolve() = %v, 期望 %v", c.ref, got, c.want)
		}
	}
}

func TestSearch(t *testing.T) {
	// 限定种类
	got := Search(KindGeoIP, "telegram", 0)
	if len(got) != 1 || got[0].Code != "telegram" {
		t.Errorf("Search(geoip, telegram) = %v, 期望单条 telegram", got)
	}
	// 忽略大小写
	if len(Search(KindACL, "netflix", 0)) == 0 {
		t.Error("Search(acl, netflix) 应命中 Netflix（忽略大小写）")
	}
	// 跨种类
	all := Search("", "telegram", 0)
	kinds := map[string]bool{}
	for _, r := range all {
		kinds[r.Kind] = true
	}
	if !kinds[KindGeosite] || !kinds[KindGeoIP] || !kinds[KindACL] {
		t.Errorf("Search(全部, telegram) 未覆盖三个种类: %v", kinds)
	}
	// limit 生效
	if got := Search("", "", 7); len(got) != 7 {
		t.Errorf("Search 限 7 条，实得 %d 条", len(got))
	}
	// 空查询返回全量
	if got := Search(KindACL, "", 0); len(got) != Count(KindACL) {
		t.Errorf("空查询应返回全量 %d，实得 %d", Count(KindACL), len(got))
	}
	// 非法种类
	if got := Search("bogus", "cn", 0); got != nil {
		t.Errorf("Search(非法种类) = %v, 期望 nil", got)
	}
}
