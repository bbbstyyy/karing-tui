package config

import (
	"reflect"
	"testing"
)

// V9-2：ApplyResolverTagCascade 必须逐字复刻 storage.UpdateDNSServerRenamed 的级联
// （`UPDATE dns_servers SET address_resolver = newTag WHERE address_resolver = oldTag`）。
//
// 它是「判环时看到的图」与「提交后的图」之间唯一的桥：写错范围或顺序，
// 校验就会放行一条提交后成环的链。
func TestApplyResolverTagCascade(t *testing.T) {
	base := []DNSServer{
		{ID: 1, Tag: "A", Type: "https", Address: "dns-a.example", AddressResolver: "", Detour: "Auto", Enabled: true, Position: 0},
		{ID: 2, Tag: "X", Type: "https", Address: "dns-x.example", AddressResolver: "A", Detour: "Auto", Enabled: true, Position: 1},
		// 字面 IP 上挂着的残留 resolver：级联 SQL 是无条件的，所以这里也必须被改写。
		// 「用不到就无视」是 **环检测** 的规则（DNSServerNeedsDomainResolver），
		// 不是级联的规则——两者混在一起就是漂移。
		{ID: 3, Tag: "Y", Type: "udp", Address: "1.1.1.1", AddressResolver: "A", Enabled: false, Position: 2},
		{ID: 4, Tag: "Z", Type: "udp", Address: "8.8.8.8", AddressResolver: "B", Enabled: true, Position: 3},
	}

	t.Run("改写所有指向旧 tag 的条目且只改该字段", func(t *testing.T) {
		got := ApplyResolverTagCascade(base, "A", "C")
		want := []DNSServer{
			{ID: 1, Tag: "A", Type: "https", Address: "dns-a.example", AddressResolver: "", Detour: "Auto", Enabled: true, Position: 0},
			{ID: 2, Tag: "X", Type: "https", Address: "dns-x.example", AddressResolver: "C", Detour: "Auto", Enabled: true, Position: 1},
			{ID: 3, Tag: "Y", Type: "udp", Address: "1.1.1.1", AddressResolver: "C", Enabled: false, Position: 2},
			{ID: 4, Tag: "Z", Type: "udp", Address: "8.8.8.8", AddressResolver: "B", Enabled: true, Position: 3},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("级联结果不符：\n got  %+v\n want %+v", got, want)
		}
	})

	t.Run("oldTag 为空或等于 newTag 时恒等", func(t *testing.T) {
		for _, tc := range []struct{ old, new string }{{"", "C"}, {"A", "A"}} {
			got := ApplyResolverTagCascade(base, tc.old, tc.new)
			if !reflect.DeepEqual(got, base) {
				t.Errorf("old=%q new=%q 应为恒等变换，实得 %+v", tc.old, tc.new, got)
			}
		}
	})

	t.Run("恒等路径也返回新切片，不复用入参底层数组", func(t *testing.T) {
		// 调用方（dns.Manager）会把结果与库里的行比对，若复用底层数组，
		// 调用方后续对入参的改动会静默污染「预测视图」。
		for _, tc := range []struct{ old, new string }{{"A", "C"}, {"", "C"}, {"A", "A"}} {
			got := ApplyResolverTagCascade(base, tc.old, tc.new)
			got[0].Tag = "MUTATED"
			if base[0].Tag != "A" {
				t.Fatalf("old=%q new=%q：返回值与入参共享底层数组，入参被改成了 %q", tc.old, tc.new, base[0].Tag)
			}
		}
	})

	t.Run("空输入不 panic", func(t *testing.T) {
		if got := ApplyResolverTagCascade(nil, "A", "C"); len(got) != 0 {
			t.Fatalf("空输入应返回空切片，实得 %+v", got)
		}
	})
}
