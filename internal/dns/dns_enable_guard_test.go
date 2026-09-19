package dns

import (
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// V9-1：启用 DNS 服务器必须与新增/编辑走同一套校验。
//
// 为什么单独钉：`SetServerEnabled` 是三条写整行的入口（AddServer / UpdateServer /
// SetServerEnabled）里唯一**没有** normalize + validateServer + 判环的那一条。
// 于是「两项都停用时先把它们编成互相引用、再依次启用」这种操作可以一路成功，
// 库里留下一张「启用项引用停用项」甚至成环的图，直到生成配置才失败——
// 而报错内容与用户刚才那个「启用」动作看不出关系。

// assertLoadConfigSelfConsistent 是 V9-1 的验收不变量：`LoadConfig` 的输出必须自洽。
//   - 每条**真的会用到** resolver 的服务器，其 AddressResolver 必须在输出集合内；
//   - 整张 domain_resolver 图不得成环。
//
// （「真的会用到」= 地址是域名，见 V8-1 的唯一判据。）
func assertLoadConfigSelfConsistent(t *testing.T, m *Manager) {
	t.Helper()
	cfg, err := m.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	tags := make(map[string]bool, len(cfg.Servers))
	for i := range cfg.Servers {
		tags[cfg.Servers[i].Tag] = true
	}
	for i := range cfg.Servers {
		s := &cfg.Servers[i]
		if config.DNSServerNeedsDomainResolver(s) && s.AddressResolver != "" && !tags[s.AddressResolver] {
			t.Errorf("配置自洽性被破坏：DNS 服务器 %q 引用 %q，但它不在生效集合里",
				s.Tag, s.AddressResolver)
		}
	}
	if err := config.ValidateDNSResolverGraph(cfg.Servers); err != nil {
		t.Errorf("配置自洽性被破坏：%v", err)
	}
}

// 判决性：启用一条「引用了停用 resolver」的服务器必须被拒绝，且不得写库。
//
// 夹具完全走公开入口构造，证明复核稿描述的起点**真的可达**：停用状态下编辑不会触发
// 「引用的 resolver 必须启用」这条检查（`validateServer` 里该判断带 `s.Enabled` 前置），
// 所以用户可以先把两条都停用、再让它们互相引用。
func TestEnableServerRejectsDisabledResolver(t *testing.T) {
	m := newTestManager(t)
	a, err := m.AddServer("A", "https", "dns-a.example.test", "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.AddServer("B", "https", "dns-b.example.test", "", "")
	if err != nil {
		t.Fatal(err)
	}

	// 1) 停用 A（此刻无人引用它） 2) 再停用 B
	if err := m.SetServerEnabled(a.ID, false); err != nil {
		t.Fatalf("停用 A: %v", err)
	}
	if err := m.SetServerEnabled(b.ID, false); err != nil {
		t.Fatalf("停用 B: %v", err)
	}

	// 3) 4) 停用状态下把两条编成互相引用 —— 必须允许，否则夹具不成立
	sa := rawServerByTag(t, m, "A")
	sa.AddressResolver = "B"
	if err := m.UpdateServer(sa); err != nil {
		t.Fatalf("停用状态下引用停用的 resolver 应当允许（复核稿描述的起点在这里）：%v", err)
	}
	sb := rawServerByTag(t, m, "B")
	sb.AddressResolver = "A"
	if err := m.UpdateServer(sb); err != nil {
		t.Fatalf("停用状态下引用停用的 resolver 应当允许：%v", err)
	}

	// 5) 现在启用 A：它会引用仍然停用的 B —— 必须被拒绝
	err = m.SetServerEnabled(a.ID, true)
	if err == nil {
		t.Fatal("启用一条引用了停用 resolver 的服务器必须被拒绝")
	}
	if !strings.Contains(err.Error(), "B") {
		t.Errorf("错误文案应点名被引用的 B，实得 %v", err)
	}
	if raw := rawServerByTag(t, m, "A"); raw.Enabled {
		t.Error("拒绝时不得写库：A 仍应处于停用状态")
	}
	assertLoadConfigSelfConsistent(t, m)

	// 复核稿给出的第二条后果：若第一次启用被放行，第二次启用 B 会把 A<->B 的环写进库。
	// 现在两次都必须被拒绝，库里不许出现环。
	if err := m.SetServerEnabled(b.ID, true); err == nil {
		t.Error("启用 B 同样必须被拒绝（A 仍停用）")
	}
	if rawA, rawB := rawServerByTag(t, m, "A"), rawServerByTag(t, m, "B"); rawA.Enabled || rawB.Enabled {
		t.Errorf("两条都不该被启用: A.enabled=%v B.enabled=%v", rawA.Enabled, rawB.Enabled)
	}
	assertLoadConfigSelfConsistent(t, m)
}

// 判决性（接线）：即使库里已经是「A 启用 -> B、B 停用 -> A」，启用 B 也必须被**环检测**
// 拦住 —— 而不是被「引用的 resolver 必须启用」先拦住（那会让本用例变成假通过）。
//
// 特意先断言前提：启用 B 本身要能通过 validateServer，否则这条测试验的根本不是判环。
//
// 可达性说明：该状态在当前公开入口下已**不可达**（A 能变成「启用且指向 B」需要 B 在编辑
// 时是启用的；而此后停用 B 又会被「有启用的服务器引用我」拦住）。它只可能来自老库或
// 手工导入。本用例钉的是：数据库已经是这样时，不许把它变成环。
func TestEnableServerRejectsResolverCycleBeforeWrite(t *testing.T) {
	m := newTestManager(t)
	createRawServer(t, m, &config.DNSServer{
		Tag: "A", Type: "https", Address: "dns-a.example.test", AddressResolver: "B", Enabled: true})
	b := &config.DNSServer{
		Tag: "B", Type: "https", Address: "dns-b.example.test", AddressResolver: "A", Enabled: false}
	createRawServer(t, m, b)

	cand := *b
	cand.Enabled = true
	if err := m.validateServer(&cand, b.ID); err != nil {
		t.Fatalf("夹具无效：启用 B 应当先通过 validateServer（否则验的不是判环），实得 %v", err)
	}

	err := m.SetServerEnabled(b.ID, true)
	if err == nil {
		t.Fatal("启用 B 会立刻成环（B -> A -> B），必须被拒绝")
	}
	if !strings.Contains(err.Error(), "环") {
		t.Errorf("错误文案应指出「环」，实得 %v", err)
	}
	if raw := rawServerByTag(t, m, "B"); raw.Enabled {
		t.Error("拒绝时不得写库：B 仍应处于停用状态")
	}
	if raw := rawServerByTag(t, m, "A"); raw.AddressResolver != "B" {
		t.Errorf("拒绝时不得改动其他行：A.AddressResolver = %q", raw.AddressResolver)
	}
}

// 老库形态：字面 IP 上挂着残留 resolver。启用时顺带收敛它（与 V8-1 的「用不到的引用
// = 不存在的引用」一致），不得因为一条假依赖而拒绝启用。
func TestEnableServerClearsStaleResolverOnLiteralIP(t *testing.T) {
	m := newTestManager(t)
	createRawServer(t, m, &config.DNSServer{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true})
	remote := &config.DNSServer{
		Tag: "remote", Type: "https", Address: "8.8.8.8", AddressResolver: "local", Enabled: false}
	createRawServer(t, m, remote)

	if err := m.SetServerEnabled(remote.ID, true); err != nil {
		t.Fatalf("字面 IP 上的残留 resolver 不该拦住启用: %v", err)
	}
	raw := rawServerByTag(t, m, "remote")
	if !raw.Enabled {
		t.Error("启用未生效")
	}
	if raw.AddressResolver != "" {
		t.Errorf("启用时应顺带收敛残留 resolver，实得 %q", raw.AddressResolver)
	}
	assertLoadConfigSelfConsistent(t, m)
}

// 反向核对（回归护栏）：**停用**是脏数据行唯一的自救动作，不得被它自己那条悬空
// resolver 拦住。这条测试不是判决性的（改动前后都该绿），它防的是「顺手在两个分支
// 都加上 validateServer」这种图省事的写法。
func TestDisableServerWithDanglingResolverStillAllowed(t *testing.T) {
	m := newTestManager(t)
	broken := &config.DNSServer{
		Tag: "broken", Type: "https", Address: "dns-broken.example.test",
		AddressResolver: "ghost", Enabled: true}
	createRawServer(t, m, broken)

	if err := m.SetServerEnabled(broken.ID, false); err != nil {
		t.Fatalf("停用不得因为自身的悬空 resolver 而被拒绝: %v", err)
	}
	if raw := rawServerByTag(t, m, "broken"); raw.Enabled {
		t.Error("停用未生效")
	}
}

// 两个分支都写规范化后的 candidate：停用同样收敛「字面 IP + 残留 resolver」，
// 否则同一行会因用户点了「启用」还是「停用」而落在两个不同的状态上。这条钉住对称性。
func TestDisableServerAlsoNormalizesStaleResolver(t *testing.T) {
	m := newTestManager(t)
	createRawServer(t, m, &config.DNSServer{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true})
	remote := &config.DNSServer{
		Tag: "remote", Type: "https", Address: "8.8.8.8", AddressResolver: "local", Enabled: true}
	createRawServer(t, m, remote)

	if err := m.SetServerEnabled(remote.ID, false); err != nil {
		t.Fatalf("停用: %v", err)
	}
	raw := rawServerByTag(t, m, "remote")
	if raw.Enabled {
		t.Error("停用未生效")
	}
	if raw.AddressResolver != "" {
		t.Errorf("停用也应顺带收敛残留 resolver，实得 %q", raw.AddressResolver)
	}
}

// 反向核对：合法启用必须照常成功，且重复启用幂等 —— 证明上一条不是「把启用整体关掉」
// 就能变绿的那种实现。
func TestEnableServerStillAcceptsNormalServers(t *testing.T) {
	m := newTestManager(t)
	createRawServer(t, m, &config.DNSServer{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true})
	doh := &config.DNSServer{
		Tag: "doh", Type: "https", Address: "dns.example.test", AddressResolver: "local", Enabled: false}
	createRawServer(t, m, doh)

	if err := m.SetServerEnabled(doh.ID, true); err != nil {
		t.Fatalf("合法启用不得被拒绝: %v", err)
	}
	if err := m.SetServerEnabled(doh.ID, true); err != nil {
		t.Fatalf("重复启用应当幂等: %v", err)
	}
	if raw := rawServerByTag(t, m, "doh"); !raw.Enabled || raw.AddressResolver != "local" {
		t.Errorf("启用后的行不符合预期: %+v", raw)
	}
	assertLoadConfigSelfConsistent(t, m)
}

// 启用一条「域名地址 + 悬空的 resolver」也必须被拒绝（validateServer 的既有覆盖，
// 顺带证明启用路径确实调到了它，而不是只调了判环）。
func TestEnableServerRejectsMissingResolver(t *testing.T) {
	m := newTestManager(t)
	s := &config.DNSServer{
		Tag: "doh", Type: "https", Address: "dns.example.test", AddressResolver: "ghost", Enabled: false}
	createRawServer(t, m, s)

	err := m.SetServerEnabled(s.ID, true)
	if err == nil {
		t.Fatal("启用一条引用了不存在 resolver 的服务器必须被拒绝")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("错误文案应点名缺失的 tag，实得 %v", err)
	}
	if raw := rawServerByTag(t, m, "doh"); raw.Enabled {
		t.Error("拒绝时不得写库")
	}
}
