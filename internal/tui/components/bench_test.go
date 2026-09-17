package components

import (
	"fmt"
	"testing"
)

// benchOptionCount 是 C19 的夹具规模：geosite 分类的实际选项数（≈1953），
// 与 C9 的 catalog 数据同源，也是「表单选项面板」最重的一档。
const benchOptionCount = 1953

// benchFormOptions 构造一个「选项面板已打开」的表单，焦点字段带 n 个选项。
//
// 夹具形态刻意贴近真实数据：选项是「中文标签 · 英文取值」，标签里含大写与空格。
// 这一点很重要——无大写的 ASCII 串上 strings.ToLower 直接返回入参（零分配），
// 若夹具全小写，「拼接 + ToLower」里的那次分配有一半会被藏起来（C8 踩过同样的坑）。
func benchFormOptions(n int, query string) *Form {
	f := NewForm("规则值", []string{"引用"}, []string{"value"}, []string{""})
	opts := make([]Option, n)
	for i := range opts {
		opts[i] = Option{
			Value: fmt.Sprintf("geosite-cn-%04d", i),
			Label: fmt.Sprintf("规则集 %04d · CN 广告拦截", i),
		}
	}
	f.SetChoices("value", opts)
	f.Field("value").Multi = true
	f.FocusKey("value")
	f.openOptions()
	f.query.SetValue(query)
	return &f
}

// BenchmarkFormOptionsFilter 度量选项面板「每次按键」的过滤成本（C19 目标）。
//
// NoHit 变体把下游 SetItems 剥干净，剩下的就是过滤循环本身：C19 之前每次按键
// 都要为每个选项现算 strings.ToLower(Label+" "+Value)，拼接与 ToLower 各一次分配，
// 1953 项即 ≈3900 allocs/op；缓存落地后应为 ≈0。
// Hit / All 变体保留真实路径，用于确认收益没有被下游建行成本吃掉。
func BenchmarkFormOptionsFilter(b *testing.B) {
	cases := []struct {
		name  string
		query string
		hits  bool
	}{
		{"NoHit", "zzzz-无此选项", false},
		{"Hit", "CN", true},
		{"All", "", true},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			f := benchFormOptions(benchOptionCount, tc.query)
			f.filterOptions()
			if got := len(f.options.Items); (got > 0) != tc.hits {
				b.Fatalf("夹具命中 %d 条，与预期不符；基准失去意义", got)
			}
			b.ReportAllocs()
			for b.Loop() {
				f.filterOptions()
			}
		})
	}
}
