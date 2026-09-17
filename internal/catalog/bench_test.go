package catalog

import "testing"

// BenchmarkCatalogSearch 度量分类库搜索的物化成本（C-BENCH0）。
//
// 两种查询分别代表两类场景：
//   - "google"：命中数少的精确查询，成本主要来自全量扫描；
//   - "a"：命中数接近全量（geosite ≈1953），是 C9 要压制的物化最坏情况。
//
// C9 引入带 limit 的搜索 API 后，同一个 benchmark 的 allocs/op 应显著下降。
func BenchmarkCatalogSearch(b *testing.B) {
	for _, query := range []string{"google", "a"} {
		b.Run("geosite/"+query, func(b *testing.B) {
			for b.Loop() {
				_ = Search(KindGeosite, query, 0)
			}
		})
	}
}

// BenchmarkCatalogCodes 度量清单读取路径（load 已由 sync.Once 缓存）。
func BenchmarkCatalogCodes(b *testing.B) {
	for b.Loop() {
		_ = Codes(KindGeosite)
	}
}
