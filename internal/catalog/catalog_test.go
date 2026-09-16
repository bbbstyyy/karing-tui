package catalog

import (
	"sort"
	"strings"
	"testing"
)

// TestLoadCounts 清单能正常加载，数量与内嵌快照一致（更新清单后需同步此处）。
func TestLoadCounts(t *testing.T) {
	load()
	if loadErr != nil {
		t.Fatalf("加载分类清单失败: %v", loadErr)
	}
	// geosite 比快照多 8 个：上游仍提供、但没有内嵌 .srs 的候选码（见 geosite.txt 头部注释）
	want := map[string]int{KindGeosite: 1961, KindGeoIP: 278, KindACL: 171}
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

// embeddedSnapshotCodes 列出编译期嵌入的 .srs 快照里的分类码（即发布物本身）。
func embeddedSnapshotCodes(t *testing.T, kind string) []string {
	t.Helper()
	entries, err := builtinRuleSetFS.ReadDir("data/rulesets/" + kind)
	if err != nil {
		t.Fatalf("读取内嵌快照 %s 失败: %v", kind, err)
	}
	codes := make([]string, 0, len(entries))
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".srs")
		if !ok {
			continue
		}
		codes = append(codes, name)
	}
	sort.Strings(codes)
	return codes
}

// TestCodeTableCoversEmbeddedSnapshot 6.0 的回归门：内嵌快照里每一个 .srs 都必须在码表
// 里有对应分类码。否则该 .srs 虽随二进制发布，catalog.Parse 却判为未知码，规则写入时被
// checkRuleSetTags 直接拒绝——即「已发布却无法引用」的悬空分类。
//
// 修复前（2026-09-15）：geosite 62 个、geoip 18 个 .srs 悬空，其中包含地区方案要用的
// geosite:malware / phishing / cryptominers 与 geoip:malware / phishing / openai / github。
func TestCodeTableCoversEmbeddedSnapshot(t *testing.T) {
	load()
	if loadErr != nil {
		t.Fatalf("加载分类清单失败: %v", loadErr)
	}
	for _, kind := range Kinds {
		codes := embeddedSnapshotCodes(t, kind)
		if len(codes) == 0 {
			t.Fatalf("内嵌快照 %s 为空（go:embed 是否失效？）", kind)
		}
		var missing []string
		for _, code := range codes {
			if !Has(kind, code) {
				missing = append(missing, code)
			}
		}
		if len(missing) > 0 {
			t.Errorf("%s: %d 个内嵌 .srs 没有码表条目（悬空分类）: %s\n  修复: scripts/gen-catalog.sh",
				kind, len(missing), strings.Join(missing, ", "))
		}
	}
}

// TestEmbeddedSnapshotCounts 内嵌快照的数量基线，与 scripts/gen-catalog.sh 的输出一致。
func TestEmbeddedSnapshotCounts(t *testing.T) {
	want := map[string]int{KindGeosite: 1953, KindGeoIP: 278, KindACL: 171}
	for _, kind := range Kinds {
		if got := len(embeddedSnapshotCodes(t, kind)); got != want[kind] {
			t.Errorf("内嵌快照 %s 分类数 = %d, 期望 %d", kind, got, want[kind])
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

// TestNeedsResolveKindInvariants 把「geosite 一律无 IP 条件、geoip 一律有 IP 条件」
// 这两条隐含假设固化成断言。二者都是 2026-09-15 对内嵌快照逐个反编译得到的实测结论
// （geosite 0/1953、geoip 278/278、acl 33/171），单条反例就会被这条测试抓住，
// 避免分类更新后 resolve 规则静默插错位置。
func TestNeedsResolveKindInvariants(t *testing.T) {
	for _, code := range Codes(KindGeosite) {
		if (Ref{Kind: KindGeosite, Code: code}).NeedsResolve() {
			t.Errorf("geosite:%s 被判为含 IP 条件，但实测 geosite 全量不含 IP 条件", code)
		}
	}
	for _, code := range Codes(KindGeoIP) {
		if !(Ref{Kind: KindGeoIP, Code: code}).NeedsResolve() {
			t.Errorf("geoip:%s 未判为含 IP 条件，但 geoip 全量为 IP 条件", code)
		}
	}
	aclIPCount := 0
	for _, code := range Codes(KindACL) {
		if (Ref{Kind: KindACL, Code: code}).NeedsResolve() {
			aclIPCount++
		}
	}
	if aclIPCount != 33 {
		t.Errorf("acl 含 IP 条件的分类数 = %d, 期望 33（与 data/acl-ip.txt 一致）", aclIPCount)
	}
	// acl-ip.txt 里的每一条都必须真的有内嵌 .srs，否则清单是凭空写出来的
	for code := range aclIP {
		if !(Ref{Kind: KindACL, Code: code}).HasEmbeddedRuleSet() {
			t.Errorf("acl-ip.txt 含 %s，但快照里没有对应 .srs", code)
		}
	}
}

// TestPhase7CatalogRefsResolvable 地区方案（cn 预置）要用的分类引用必须都能解析。
// 这是 6.0 的直接验收：修复前其中 7 个因码表缺条目而被 catalog.Parse 判为未知码。
// geoip:bing 是**真悬空引用**（快照无 bing.srs，karing 的 geoip_codes.txt 也没有该码），
// 故地区方案必须剔除它只保留 geosite:bing——这里把「它仍然不可解析」也固定下来，
// 防止有人「顺手」把它加回码表却依然没有可下载的规则集。
func TestPhase7CatalogRefsResolvable(t *testing.T) {
	needed := []string{
		"geosite:malware", "geosite:phishing", "geosite:cryptominers",
		"geoip:malware", "geoip:phishing", "geoip:openai", "geoip:github",
		"acl:Gemini", "acl:Claude", "acl:GoogleFCM", "acl:Steam", "acl:Nintendo",
		"acl:BilibiliHMT", "acl:NetEaseMusic", "acl:ChinaIp", "acl:ChinaDomain",
		"acl:ChinaCompanyIp", "acl:UnBan", "acl:SteamCN", "acl:Download", "acl:ChinaMedia",
		"acl:ProxyGFWlist", "acl:ProxyMedia", "geosite:geolocation-!cn", "geosite:bing",
	}
	for _, value := range needed {
		if !IsRef(value) {
			t.Errorf("地区方案引用的分类 %q 无法解析（码表缺失或被改名）", value)
		}
	}
	if IsRef("geoip:bing") {
		t.Error("geoip:bing 应为悬空引用（快照无 bing.srs），不应出现在码表中")
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
