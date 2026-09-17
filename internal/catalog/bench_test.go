package catalog

import "testing"

// BenchmarkCatalogSearch 度量分类库搜索的**全量**物化成本（C-BENCH0）。
//
// 两种查询分别代表两类场景：
//   - "google"：命中数少的精确查询，成本主要来自全量扫描；
//   - "a"：命中数超过千条（geosite），是 C9 要压制的物化最坏情况。
//
// 注意 C9 之后 Search 本身**未变**（CLI 的 `rule-set search` 需要 limit+1 的提前
// 退出，仍走它）；被限制的是界面路径，见 BenchmarkCatalogSearchLimited。
func BenchmarkCatalogSearch(b *testing.B) {
	for _, query := range []string{"google", "a"} {
		b.Run("geosite/"+query, func(b *testing.B) {
			for b.Loop() {
				_ = Search(KindGeosite, query, 0)
			}
		})
	}
}

// BenchmarkCatalogSearchLimited 与 BenchmarkCatalogSearch/geosite/a 配对：
// 同一个查询、同一次全量扫描（O(N) 不变），但只物化前 200 条。
//
// C9 之前物化成本随**命中总数**走（geosite/a 命中 1000+ 条 → 119KB / 12 allocs）；
// 现在应与 limit 同阶，而不是与命中数同阶。
func BenchmarkCatalogSearchLimited(b *testing.B) {
	for b.Loop() {
		_, _ = SearchLimited(KindGeosite, "a", 200)
	}
}

// BenchmarkCatalogCodes 度量清单读取路径（load 已由 sync.Once 缓存）。
func BenchmarkCatalogCodes(b *testing.B) {
	for b.Loop() {
		_ = Codes(KindGeosite)
	}
}
