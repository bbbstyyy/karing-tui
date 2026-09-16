package cli

import (
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// 6.6：CLI 按层操作。route list 按层分组输出；route add 会回显推断出的层；
// route move 是显式的跨层操作；route test 补充所属层。
//
// 注意：夹具应用初始化时会写入默认分流方案（中国大陆/Telegram/AI + Final），
// 所以各用例的层内组数都包含这 4 组——这也正是要验证的「默认方案落在哪一层」。

func TestRouteListGroupsByLayer(t *testing.T) {
	setupHome(t)
	runOK(t, "route", "add", "手工规则", "--target", "DIRECT", "geosite:cn", "geoip:cn")
	runOK(t, "route", "add", "域名组", "--target", "DIRECT", "--kind", "geosite", "geosite:apple")

	code, out, errOut := run(t, "route", "list")
	if code != 0 {
		t.Fatalf("route list 退出码 = %d, stderr: %s", code, errOut)
	}
	for _, want := range []string{
		"层序（优先级由高到低）: custom < geosite < geoip < acl < final",
		// 默认方案（cn 预置）= 27 个 custom 组（6 启用）+ 1 个 final 兜底组；再加手工规则
		"自定义分流组（custom，层序 0）· 28 组 / 启用 7",
		"GeoSite（geosite，层序 1）· 1 组 / 启用 1",
		"GeoIP（geoip，层序 2）· 0 组 / 启用 0",
		"    （空层）",
		"final（final，层序 4）· 1 组 / 启用 1",
		"兜底 · 不可选「不处理」",
		"手工规则",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("route list 输出缺 %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "自定义分流组") > strings.Index(out, "GeoSite") {
		t.Errorf("层序不正确:\n%s", out)
	}
}

func TestRouteAddInfersAndEchoesKind(t *testing.T) {
	setupHome(t)

	// 全部引用同一个分类库种类 → 推断到对应层，并回显推断结果
	code, out, errOut := run(t, "route", "add", "域名组", "--target", "DIRECT", "geosite:cn", "geosite:apple")
	if code != 0 {
		t.Fatalf("route add 退出码 = %d, stderr: %s", code, errOut)
	}
	for _, want := range []string{"已创建分流组 \"域名组\"", "推断层 GeoSite", "层序 1", "层内序号: 第 1 位"} {
		if !strings.Contains(out, want) {
			t.Errorf("route add 输出缺 %q:\n%s", want, out)
		}
	}
	// 混合种类 → custom（层序最高，语义最安全）
	_, out, _ = run(t, "route", "add", "混合组", "--target", "DIRECT", "geosite:google", "geoip:google")
	if !strings.Contains(out, "推断层 自定义分流组") {
		t.Errorf("混合种类应推断到 custom 层:\n%s", out)
	}
	// --kind 覆盖推断
	_, out, _ = run(t, "route", "add", "强指层", "--target", "DIRECT", "--kind", "acl", "geosite:cn")
	if !strings.Contains(out, "ACL 层") || strings.Contains(out, "推断层") {
		t.Errorf("--kind 显式指定时不应回显推断:\n%s", out)
	}
	// 同层第二个组，层内序号递增
	_, out, _ = run(t, "route", "add", "域名组2", "--target", "DIRECT", "geosite:bing")
	if !strings.Contains(out, "层内序号: 第 2 位") {
		t.Errorf("层内序号应递增:\n%s", out)
	}
	// 错误用法
	if code, _, _ := run(t, "route", "add", "没有目标"); code != 2 {
		t.Errorf("缺 --target 退出码 = %d, 期望 2", code)
	}
	if code, _, _ := run(t, "route", "add", "x", "--target", "DIRECT", "--kind", "nosuch"); code != 2 {
		t.Errorf("未知层退出码 = %d, 期望 2", code)
	}
	if code, _, _ := run(t, "route", "add", "x", "--target", "DIRECT", "不存在的规则集"); code != 1 {
		t.Errorf("非法规则集退出码 = %d, 期望 1", code)
	}
	// final 层结构性约束：默认方案已有 Final 组，第二个 final 组必须被拒
	code, _, errOut = run(t, "route", "add", "第二个兜底", "--target", "DIRECT", "--kind", "final")
	if code != 1 || !strings.Contains(errOut, "final 层最多只能有一个分流组") {
		t.Errorf("第二个 final 组应被拒绝: code=%d stderr=%s", code, errOut)
	}
}

func TestRouteMoveAcrossLayers(t *testing.T) {
	setupHome(t)
	runOK(t, "route", "add", "a", "--target", "DIRECT", "geosite:cn", "geoip:cn")
	runOK(t, "route", "add", "b", "--target", "DIRECT", "geosite:cn", "geoip:cn")
	if code, _, _ := run(t, "route", "add", "c", "--target", "DIRECT", "---kind", "geosite"); code != 2 {
		t.Errorf("未知参数退出码 = %d, 期望 2", code)
	}
	runOK(t, "route", "add", "域名组", "--target", "DIRECT", "--kind", "geosite", "geosite:cn")

	code, out, errOut := run(t, "route", "move", "a", "--kind", "geosite")
	if code != 0 {
		t.Fatalf("route move 退出码 = %d, stderr: %s", code, errOut)
	}
	for _, want := range []string{"a: 自定义分流组 层 → GeoSite 层", "层尾", "跨层移动改变了该组的优先级归属"} {
		if !strings.Contains(out, want) {
			t.Errorf("route move 输出缺 %q:\n%s", want, out)
		}
	}
	// --pos 0 插到层首
	code, out, _ = run(t, "route", "move", "b", "--kind=geosite", "--pos=0")
	if code != 0 {
		t.Fatalf("route move --pos 退出码 = %d", code)
	}
	if !strings.Contains(out, "第 1 位") {
		t.Errorf("--pos 0 应插到层首:\n%s", out)
	}
	// 列表顺序：b, 域名组, a（geosite 层内）
	_, list, _ := run(t, "route", "list")
	geosite := list[strings.Index(list, "GeoSite"):]
	for _, want := range []string{"b", "域名组", "a"} {
		if !strings.Contains(geosite, want) {
			t.Errorf("geosite 层缺少 %q:\n%s", want, geosite)
		}
	}
	// 移到 final 层会被结构约束拒绝（普通规则组不含 final 规则）
	if code, _, errOut := run(t, "route", "move", "b", "--kind", "final"); code != 1 || !strings.Contains(errOut, "final") {
		t.Errorf("移入 final 层应报错: code=%d stderr=%s", code, errOut)
	}
	// 不存在的组
	if code, _, _ := run(t, "route", "move", "不存在", "--kind", "geosite"); code != 1 {
		t.Errorf("不存在的组退出码 = %d, 期望 1", code)
	}
	if code, _, _ := run(t, "route", "move", "b"); code != 2 {
		t.Errorf("缺 --kind 退出码 = %d, 期望 2", code)
	}
}

func TestRouteTestShowsLayer(t *testing.T) {
	paths := setupHome(t)
	db, err := storage.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	// route test 无法离线展开规则集内容（rule_set 记为未知），故这里直接用可判定条件建组。
	// 该用例走只读命令（不初始化默认方案），所以 final 组也在这里显式建出来。
	if err := db.CreateRoutingGroup(&config.RoutingGroup{
		Name: "域名组", Target: "DIRECT", Kind: config.KindGeosite, Position: 0, Enabled: true,
		Rules: []config.Rule{{Type: "domain_suffix", Value: "example.com", Enabled: true}},
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.CreateRoutingGroup(&config.RoutingGroup{
		Name: "兜底", Target: "DIRECT", Kind: config.KindFinal, Position: 0, Enabled: true,
		Rules: []config.Rule{{Type: "final", Enabled: true}},
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	code, out, errOut := run(t, "route", "test", "www.example.com")
	if code != 0 {
		t.Fatalf("route test 退出码 = %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "命中分流组 域名组（层 GeoSite，层内第 1 位）") {
		t.Errorf("route test 应输出所属层:\n%s", out)
	}
	// 兜底命中同样带层
	_, out, _ = run(t, "route", "test", "203.0.113.9")
	if !strings.Contains(out, "使用兜底分流组 兜底（层 final）") {
		t.Errorf("兜底输出应带层:\n%s", out)
	}
}

// runOK 执行命令并断言成功，返回标准输出。
func runOK(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := run(t, args...)
	if code != 0 {
		t.Fatalf("%v 退出码 = %d, stderr: %s", args, code, errOut)
	}
	return out
}
