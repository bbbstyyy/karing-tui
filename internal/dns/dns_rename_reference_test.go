package dns

import (
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// V9-2：DNS tag 重命名必须在**提交后的图**上判环。
//
// 为什么单看 update 参数看不出问题：`UpdateDNSServerRenamed` 会把所有
// `AddressResolver == 旧 tag` 的条目一起改成新 tag（`storage/rulesets_dns.go` 的级联）。
// 因此「candidate 自身的 resolver」与「别人指向 candidate 的 resolver」在提交后会被
// **同时**改向，判环时若只看「改名前的库 + 改名的自己」，就少了一条边。
//
// 判决性：旧实现（8143f4c）在这条夹具上返回 nil，并在库里落下 C -> X -> C。
func TestRenameServerRejectsCycleCreatedByReferenceCascade(t *testing.T) {
	m := newTestManager(t)
	a := &config.DNSServer{Tag: "A", Type: "https", Address: "dns-a.example.test", Enabled: true}
	createRawServer(t, m, a)
	createRawServer(t, m, &config.DNSServer{
		Tag: "X", Type: "https", Address: "dns-x.example.test", AddressResolver: "A", Enabled: true})

	// 用户把 A 改名成 C，同时让 C 指向 X。提交后级联会把 `X -> A` 一并改成 `X -> C`，
	// 于是最终是 C -> X -> C。单条 update 各自都「合法」——只有组合起来才成环。
	a.Tag = "C"
	a.AddressResolver = "X"
	if err := m.UpdateServer(a); err == nil {
		t.Fatal("改名级联造出的环必须在写库之前被拒绝")
	}

	// 拒绝后库必须原样：没有任何一行被写进去。
	if raw := rawServerByTag(t, m, "A"); raw.AddressResolver != "" {
		t.Errorf("拒绝时不得写库：A.AddressResolver = %q，期望空", raw.AddressResolver)
	}
	if raw := rawServerByTag(t, m, "X"); raw.AddressResolver != "A" {
		t.Errorf("拒绝时不得级联改名：X.AddressResolver = %q，期望仍然指向 A", raw.AddressResolver)
	}
	for _, s := range mustListRaw(t, m) {
		if s.Tag == "C" {
			t.Error("拒绝时不得出现新 tag C")
		}
	}
}

// mustListRaw 直接读库（不经 ListServers 的规范化），供「库是否被改动」类断言使用。
func mustListRaw(t *testing.T, m *Manager) []*config.DNSServer {
	t.Helper()
	servers, err := m.DB.ListDNSServers()
	if err != nil {
		t.Fatal(err)
	}
	return servers
}

func graphView(t *testing.T, m *Manager) []config.DNSServer {
	t.Helper()
	raw := mustListRaw(t, m)
	out := make([]config.DNSServer, 0, len(raw))
	for _, s := range raw {
		out = append(out, *s)
	}
	return out
}

// V9-2 防漂移：校验侧用来判环的「改名后视图」必须与存储侧**真正**提交的结果一致。
//
// 这条测试同时钉住两侧，任一侧改了级联范围或顺序都会红：
//   - storage 若不再改写 address_resolver（或改成只改 dns_rules），库里的结果会与预测不符；
//   - config.ApplyResolverTagCascade 若被写成「只改候选自身」或漏掉停用行，预测会与库不符。
//
// 它本身不判决「该不该拦环」，判决性由 TestRenameServerRejectsCycleCreatedByReferenceCascade 承担。
func TestRenameCascadeMatchesConfigHelper(t *testing.T) {
	m := newTestManager(t)
	a := &config.DNSServer{Tag: "A", Type: "https", Address: "dns-a.example.test", Enabled: true}
	createRawServer(t, m, a)
	createRawServer(t, m, &config.DNSServer{
		Tag: "X", Type: "https", Address: "dns-x.example.test", AddressResolver: "A", Enabled: true})
	// 字面 IP 上的残留 resolver：级联 SQL 无条件改写它，纯函数也必须跟着改（见 config 侧用例）。
	createRawServer(t, m, &config.DNSServer{
		Tag: "Y", Type: "udp", Address: "1.1.1.1", AddressResolver: "A", Enabled: false})

	before := graphView(t, m)

	// 纯改名（不改自己的 resolver）：不得被拒绝，级联把所有指向 A 的条目改成 C。
	renamed := *a
	renamed.Tag = "C"
	if err := m.UpdateServer(&renamed); err != nil {
		t.Fatalf("纯改名不得被拒绝: %v", err)
	}
	after := graphView(t, m)

	// 先替换候选、再做级联 —— 与提交路径同样的顺序。
	view := make([]config.DNSServer, 0, len(before))
	for _, s := range before {
		if s.ID == renamed.ID {
			continue
		}
		view = append(view, s)
	}
	view = append(view, renamed)
	predicted := config.ApplyResolverTagCascade(view, "A", "C")

	if len(predicted) != len(after) {
		t.Fatalf("行数不一致：预测 %d，库里 %d", len(predicted), len(after))
	}
	byID := make(map[int64]config.DNSServer, len(predicted))
	for _, s := range predicted {
		byID[s.ID] = s
	}
	cascaded := 0
	for _, got := range after {
		want, ok := byID[got.ID]
		if !ok {
			t.Fatalf("预测视图里缺少 ID=%d（tag=%q）", got.ID, got.Tag)
		}
		if got.Tag != want.Tag {
			t.Errorf("ID=%d 的 tag：库里 %q，预测 %q", got.ID, got.Tag, want.Tag)
		}
		if got.AddressResolver != want.AddressResolver {
			t.Errorf("ID=%d(tag=%q) 的 address_resolver：库里 %q，纯函数预测 %q",
				got.ID, got.Tag, got.AddressResolver, want.AddressResolver)
		}
		if got.AddressResolver == "C" {
			cascaded++
		}
	}
	// 反向核对：否则「两侧同时不级联」也会让上面的比对通过。
	if cascaded != 2 {
		t.Errorf("应当有 2 行被级联改成 C（X 与 Y），实得 %d 行；库快照: %+v", cascaded, after)
	}
	for _, s := range after {
		if s.AddressResolver == "A" {
			t.Errorf("改名后不应再有指向旧 tag 的条目：%+v", s)
		}
	}
}
