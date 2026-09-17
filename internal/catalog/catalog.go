// Package catalog 提供内置规则集分类库：geosite/geoip/acl 三套分类码清单
// （随二进制嵌入 data/*.txt），以及分类引用（"geosite:cn"）到 sing-box
// rule_set tag / 下载地址的映射。
//
// 用户在 rule_set 规则里以 "geosite:cn" 形式引用某个分类，生成器按需产出
// route.rule_set 条目并按需下载缓存——无需预先把每个分类写进 rulesets 表。
// 派生 tag 采用 "kind-code"（如 geosite-cn），与项目早期硬编码的 7 个内置
// 规则集 tag 命名一致，故老数据与分类引用互不冲突。
//
// 清单更新见 scripts/gen-catalog.sh。这是零依赖叶子包，config 与 rules 均可引用。
package catalog

import (
	"bufio"
	"embed"
	"fmt"
	"sort"
	"strings"
	"sync"
)

//go:embed data/geosite.txt data/geoip.txt data/acl.txt data/acl-ip.txt
var catalogFS embed.FS

// Built-in rule-set binaries are embedded in the executable so the first
// configuration generation does not need network access.  The files are
// copied from the karing rule-set bundle at build/update time and can still be
// replaced in the user's cache by DownloadCatalog later.
//
// Keep this pattern deliberately scoped to the three supported catalog kinds;
// it prevents unrelated files from becoming part of the executable by
// accident.
//
//go:embed data/rulesets/geosite/*.srs data/rulesets/geoip/*.srs data/rulesets/acl/*.srs
var builtinRuleSetFS embed.FS

// 分类库种类。
const (
	KindGeosite = "geosite"
	KindGeoIP   = "geoip"
	KindACL     = "acl"
)

// Kinds 按展示顺序列出全部种类。
var Kinds = []string{KindGeosite, KindGeoIP, KindACL}

// 分类规则集下载地址前缀。geosite/geoip 取 meta-rules-dat（上游权威）；
// ACL4SSR 仅 karing-ruleset 提供 sing 格式（.srs），与 karing 同源。
//
// 注意：码表（data/*.txt）的事实源是**内嵌快照**，不是这两个仓库的当前文件树。
// 上游只用于发现「没有内嵌 .srs 的候选码」（data/*.txt 头部注释里列出的那些），
// 它们仍可引用，只是首次生成配置时要联网下载。见 scripts/gen-catalog.sh。
const (
	geoBaseURL = "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo"
	aclBaseURL = "https://github.com/KaringX/karing-ruleset/raw/sing/ACL4SSR"
)

// Ref 一个内置分类引用。
type Ref struct {
	Kind string // geosite / geoip / acl
	Code string // 分类码，如 cn、category-ai-!cn、ChinaDomain
}

// String 返回引用写法 "kind:code"，即用户在规则里填的值。
func (r Ref) String() string { return r.Kind + ":" + r.Code }

// Tag 返回派生的 sing-box rule_set tag "kind-code"。
// 与既有内置规则集的 tag 命名一致（geosite-cn 等），故老数据与分类引用互不冲突。
func (r Ref) Tag() string { return r.Kind + "-" + r.Code }

// URL 返回分类规则集的 .srs 下载地址。
func (r Ref) URL() string {
	if r.Kind == KindACL {
		return aclBaseURL + "/" + r.Code + ".srs"
	}
	return geoBaseURL + "/" + r.Kind + "/" + r.Code + ".srs"
}

// EmbeddedRuleSet returns the compile-time bundled .srs bytes for a catalog
// reference.  A false result means this particular catalog item is not in the
// bundled snapshot (the catalog list may be newer than the bundled assets),
// in which case callers may fall back to the normal remote URL.
func (r Ref) EmbeddedRuleSet() ([]byte, bool) {
	if !KindValid(r.Kind) || strings.TrimSpace(r.Code) == "" {
		return nil, false
	}
	b, err := builtinRuleSetFS.ReadFile("data/rulesets/" + r.Kind + "/" + r.Code + ".srs")
	if err != nil {
		return nil, false
	}
	return b, true
}

// HasEmbeddedRuleSet reports whether a catalog reference has a bundled binary
// rule-set asset.
func (r Ref) HasEmbeddedRuleSet() bool {
	_, ok := r.EmbeddedRuleSet()
	return ok
}

var (
	once    sync.Once
	byKind  = map[string][]string{}        // kind → 排序后的分类码
	setKind = map[string]map[string]bool{} // kind → 码集合
	aclIP   = map[string]bool{}            // 含 IP 类条件的 acl 分类码
	loadErr error
)

// readList 读取嵌入清单，跳过空行与 # 注释行。
func readList(name string) ([]string, error) {
	f, err := catalogFS.Open("data/" + name + ".txt")
	if err != nil {
		return nil, fmt.Errorf("打开分类清单 %s 失败: %w", name, err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取分类清单 %s 失败: %w", name, err)
	}
	return out, nil
}

func load() {
	once.Do(func() {
		for _, kind := range Kinds {
			lines, err := readList(kind)
			if err != nil {
				loadErr = err
				return
			}
			codes := make([]string, 0, len(lines))
			set := make(map[string]bool, len(lines))
			for _, line := range lines {
				if !set[line] {
					set[line] = true
					codes = append(codes, line)
				}
			}
			sort.Strings(codes)
			byKind[kind] = codes
			setKind[kind] = set
		}
		ipCodes, err := readList("acl-ip")
		if err != nil {
			loadErr = err
			return
		}
		for _, c := range ipCodes {
			aclIP[c] = true
		}
	})
}

// KindValid 报告种类名是否合法。
func KindValid(kind string) bool {
	switch kind {
	case KindGeosite, KindGeoIP, KindACL:
		return true
	}
	return false
}

// Codes 返回某种类的全部分类码（已排序）。种类非法时返回 nil。
func Codes(kind string) []string {
	load()
	return byKind[kind]
}

// Has 报告某分类码存在于清单中。
func Has(kind, code string) bool {
	load()
	set := setKind[kind]
	return set != nil && set[code]
}

// Count 返回某种类的分类码数量。
func Count(kind string) int { return len(Codes(kind)) }

// Search 在某种类中按子串（忽略大小写）搜索分类码。
// kind 为空时搜索全部种类。limit <= 0 表示不限条数。
func Search(kind, query string, limit int) []Ref {
	load()
	kinds := Kinds
	if kind != "" {
		if !KindValid(kind) {
			return nil
		}
		kinds = []string{kind}
	}
	q := normalizeQuery(query)
	out := []Ref{}
	for _, k := range kinds {
		for _, code := range byKind[k] {
			if q != "" && !strings.Contains(strings.ToLower(code), q) {
				continue
			}
			out = append(out, Ref{Kind: k, Code: code})
			if limit > 0 && len(out) >= limit {
				return out
			}
		}
	}
	return out
}

// SearchLimited 在某种类中按子串（忽略大小写）搜索分类码，最多物化 limit 条，
// 同时返回命中总数。kind 为空时搜索全部种类，limit <= 0 表示不限物化条数。
//
// 与 Search(kind, query, limit>0) 的分工：Search 一凑满 limit 就提前返回，拿不到总数；
// SearchLimited 始终扫完（仍是纯内存的 O(N)，与命中数无关），但只 append 前 limit 条。
// 界面因此可以在「不物化全部命中」的前提下显示真实命中数（C9）——geosite 单字符
// 搜索命中上千条，逐条查缓存状态与建行才是成本所在，扫描本身不是。
//
// 返回区分两种情况：种类非法时 hits 为 nil；无命中时 hits 为空的非 nil 切片。
func SearchLimited(kind, query string, limit int) (hits []Ref, total int) {
	load()
	kinds := Kinds
	if kind != "" {
		if !KindValid(kind) {
			return nil, 0
		}
		kinds = []string{kind}
	}
	q := normalizeQuery(query)
	hits = []Ref{}
	for _, k := range kinds {
		for _, code := range byKind[k] {
			if q != "" && !strings.Contains(strings.ToLower(code), q) {
				continue
			}
			total++
			if limit <= 0 || len(hits) < limit {
				hits = append(hits, Ref{Kind: k, Code: code})
			}
		}
	}
	return hits, total
}

// normalizeQuery 归一化搜索词：搜索始终忽略大小写与首尾空白。
func normalizeQuery(query string) string { return strings.ToLower(strings.TrimSpace(query)) }

// Parse 解析分类引用。接受两种写法：
//   - "kind:code"（推荐，与 karing 一致）——冒号后即分类码
//   - "kind-code"（派生 tag 形式）——生成的配置里就是这个 tag，便于直接照抄
//
// 分类码自身可含连字符（category-ai-!cn），故连字符形式按前缀剥离而非末段切分。
// 仅当码存在于清单中才返回 ok，否则调用方应按自定义规则集 tag 处理。
func Parse(value string) (Ref, bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		return Ref{}, false
	}
	if kind, code, found := strings.Cut(v, ":"); found {
		kind = strings.ToLower(strings.TrimSpace(kind))
		code = strings.TrimSpace(code)
		if KindValid(kind) && Has(kind, code) {
			return Ref{Kind: kind, Code: code}, true
		}
		return Ref{}, false
	}
	for _, kind := range Kinds {
		code, ok := strings.CutPrefix(v, kind+"-")
		if !ok {
			continue
		}
		if Has(kind, code) {
			return Ref{Kind: kind, Code: code}, true
		}
	}
	return Ref{}, false
}

// IsRef 报告值是否为合法的内置分类引用。
func IsRef(value string) bool {
	_, ok := Parse(value)
	return ok
}

// RefByTag 从派生 tag（"kind-code"）还原分类引用；非分类 tag 返回 ok=false。
func RefByTag(tag string) (Ref, bool) {
	return Parse(strings.TrimSpace(tag))
}

// NeedsResolve 报告该分类的规则集是否含 IP 类条件（ip_cidr/ip_is_private）——
// 这类条件对域名目标必须先 resolve 才能匹配（见 4.1）。判定依据是**对内嵌快照逐个
// 反编译得到的实测结论**，而非命名或上游仓库的状态：
//
//   - geoip：278 个分类全部含 IP 条件
//   - geosite：1953 个分类全部**不含** IP 条件
//   - acl：混合，33/171 含 IP 条件，按 data/acl-ip.txt 清单判定
//     （ChinaDomain、Apple 等名字不带 "ip" 的分类实际含 ip_cidr，仅凭 tag 字面判断会漏判）
//
// 复核记录（2026-09-15，判定对象 internal/catalog/data/rulesets/ 即发布物本身）：
// geosite 1953/1953 无 IP、geoip 278/278 有 IP、acl 33/171 有 IP，
// 与 data/acl-ip.txt 逐条一致。重跑方式见 scripts/gen-catalog.sh（--ip-all 覆盖三类）。
// 相应地，码表里没有内嵌 .srs 的上游候选码（如 geosite 的 8 个）按「无 IP 条件」处理，
// 与它们在生成配置时需要联网下载这一点无关。
func (r Ref) NeedsResolve() bool {
	load()
	switch r.Kind {
	case KindGeoIP:
		return true
	case KindACL:
		return aclIP[r.Code]
	default:
		return false
	}
}
