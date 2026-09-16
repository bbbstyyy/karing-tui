package routing

import (
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// --- 6.1 层序模型 ---

// TestKindRankMapping 层序常量是唯一事实源：序号固定为 custom<geosite<geoip<acl<final。
func TestKindRankMapping(t *testing.T) {
	want := []struct {
		kind string
		rank int
	}{
		{config.KindCustom, 0},
		{config.KindGeosite, 1},
		{config.KindGeoIP, 2},
		{config.KindACL, 3},
		{config.KindFinal, 4},
	}
	for _, c := range want {
		if got := config.KindRank(c.kind); got != c.rank {
			t.Errorf("KindRank(%q) = %d, 期望 %d", c.kind, got, c.rank)
		}
	}
	// 空值与非法值一律按 custom——层序最高，最安全
	for _, bad := range []string{"", " ", "bogus", "CUSTOM"} {
		if got := config.KindRank(bad); got != 0 {
			t.Errorf("KindRank(%q) = %d, 期望 0（非法值按 custom）", bad, got)
		}
	}
	if !config.KindValid(config.KindGeoIP) {
		t.Error("KindValid(geoip) 应为 true")
	}
	if config.KindValid("geip") {
		t.Error("KindValid(geip) 应为 false")
	}
}

// TestListRoutingGroupsOrdersByLayer 跨层排序：先层序、再层内 position、最后 id。
// final 层恒在最后；层内 position 冲突时按 id 决胜；空层不影响结果。
func TestListRoutingGroupsOrdersByLayer(t *testing.T) {
	m := newTestManager(t)

	seed := []*config.RoutingGroup{
		// 故意乱序写入，且 position 有冲突/空洞
		{Name: "final-1", Target: "Auto", Kind: config.KindFinal, Position: 0, Enabled: true,
			Rules: []config.Rule{{Type: "final", Enabled: true}}},
		{Name: "acl-b", Target: "Auto", Kind: config.KindACL, Position: 1, Enabled: true,
			Rules: []config.Rule{{Type: "domain", Value: "b.com", Enabled: true}}},
		{Name: "custom-2", Target: "Auto", Kind: config.KindCustom, Position: 7, Enabled: true,
			Rules: []config.Rule{{Type: "domain", Value: "c2.com", Enabled: true}}},
		{Name: "geoip-1", Target: "Auto", Kind: config.KindGeoIP, Position: 0, Enabled: true,
			Rules: []config.Rule{{Type: "domain", Value: "g1.com", Enabled: true}}},
		{Name: "custom-1", Target: "Auto", Kind: config.KindCustom, Position: 0, Enabled: true,
			Rules: []config.Rule{{Type: "domain", Value: "c1.com", Enabled: true}}},
		{Name: "acl-a", Target: "Auto", Kind: config.KindACL, Position: 1, Enabled: true,
			Rules: []config.Rule{{Type: "domain", Value: "a.com", Enabled: true}}},
		{Name: "geosite-1", Target: "Auto", Kind: config.KindGeosite, Position: 0, Enabled: true,
			Rules: []config.Rule{{Type: "domain", Value: "s1.com", Enabled: true}}},
	}
	for _, g := range seed {
		if err := m.DB.CreateRoutingGroup(g); err != nil {
			t.Fatalf("预置分组 %q: %v", g.Name, err)
		}
	}

	groups, err := m.DB.ListRoutingGroups()
	if err != nil {
		t.Fatalf("ListRoutingGroups: %v", err)
	}
	got := make([]string, 0, len(groups))
	for _, g := range groups {
		got = append(got, g.Name)
	}
	// custom(0,7 → custom-1(id 小) 在 custom-2 之前) → geosite → geoip → acl(同为 1，按 id: acl-b 先写) → final
	want := []string{"custom-1", "custom-2", "geosite-1", "geoip-1", "acl-b", "acl-a", "final-1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("分层排序结果 = %v, 期望 %v", got, want)
	}

	// final 恒最后
	if groups[len(groups)-1].Name != "final-1" {
		t.Errorf("final 层应恒排最后，实得 %q", groups[len(groups)-1].Name)
	}
}

// TestGroupByKindKeepsEmptyLayers 层聚合保留空层（界面要把规则移进空层），且顺序恒为层序。
func TestGroupByKindKeepsEmptyLayers(t *testing.T) {
	groups := []*config.RoutingGroup{
		{Name: "b", Kind: config.KindGeoIP, Position: 0},
		{Name: "a", Kind: config.KindCustom, Position: 0},
		{Name: "a2", Kind: config.KindCustom, Position: 1},
	}
	layers := config.GroupByKind(groups)
	if len(layers) != len(config.Kinds) {
		t.Fatalf("层数 = %d, 期望 %d（空层也要保留）", len(layers), len(config.Kinds))
	}
	for i, kind := range config.Kinds {
		if layers[i].Kind != kind {
			t.Errorf("第 %d 层 = %q, 期望 %q", i, layers[i].Kind, kind)
		}
	}
	if len(layers[0].Groups) != 2 {
		t.Errorf("custom 层成员数 = %d, 期望 2", len(layers[0].Groups))
	}
	if len(layers[1].Groups) != 0 || len(layers[3].Groups) != 0 || len(layers[4].Groups) != 0 {
		t.Error("geosite/acl/final 层应为空")
	}
	if len(layers[2].Groups) != 1 {
		t.Errorf("geoip 层成员数 = %d, 期望 1", len(layers[2].Groups))
	}
	// 空 Kind 的组按 custom 归类（老数据兼容）
	layers = config.GroupByKind([]*config.RoutingGroup{{Name: "legacy"}})
	if len(layers[0].Groups) != 1 {
		t.Error("空 Kind 的分组应归入 custom 层")
	}
}

// --- 6.4 分层归属 ---

func TestSuggestKind(t *testing.T) {
	cases := []struct {
		name  string
		rules []config.Rule
		want  string
	}{
		{"全部 geosite", []config.Rule{{Type: "rule_set", Value: "geosite:cn", Enabled: true}}, config.KindGeosite},
		{"多个 geosite", []config.Rule{
			{Type: "rule_set", Value: "geosite:cn,geosite:apple", Enabled: true}}, config.KindGeosite},
		{"全部 geoip", []config.Rule{{Type: "rule_set", Value: "geoip:cn", Enabled: true}}, config.KindGeoIP},
		{"全部 acl", []config.Rule{{Type: "rule_set", Value: "acl:Steam", Enabled: true}}, config.KindACL},
		{"混合种类", []config.Rule{
			{Type: "rule_set", Value: "geosite:google", Enabled: true},
			{Type: "rule_set", Value: "geoip:google", Enabled: true}}, config.KindCustom},
		{"混合种类同一值内", []config.Rule{
			{Type: "rule_set", Value: "geosite:cn,geoip:cn", Enabled: true}}, config.KindCustom},
		{"含非规则集条件", []config.Rule{
			{Type: "rule_set", Value: "geosite:cn", Enabled: true},
			{Type: "domain_suffix", Value: "x.com", Enabled: true}}, config.KindCustom},
		{"自定义规则集（内容不可静态识别）", []config.Rule{
			{Type: "rule_set", Value: "my-rules", Enabled: true}}, config.KindCustom},
		{"空规则", nil, config.KindCustom},
		{"final 规则", []config.Rule{{Type: "final", Enabled: true}}, config.KindCustom},
	}
	for _, c := range cases {
		if got := config.SuggestKind(c.rules); got != c.want {
			t.Errorf("%s: SuggestKind = %q, 期望 %q", c.name, got, c.want)
		}
	}
}

// TestCreateGroupInUsesSuggestedKind 新建组按规则构成推断层，layer 内 Position 递增。
func TestCreateGroupInUsesSuggestedKind(t *testing.T) {
	m := newTestManager(t)

	g1, err := m.CreateGroup("域名组", "Auto", []config.Rule{{Type: "rule_set", Value: "geosite:cn", Enabled: true}})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if g1.Kind != config.KindGeosite {
		t.Errorf("推断的层 = %q, 期望 geosite", g1.Kind)
	}
	g2, err := m.CreateGroup("IP 组", "Auto", []config.Rule{{Type: "rule_set", Value: "geoip:cn", Enabled: true}})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	g3, err := m.CreateGroupIn("第二个域名组", "Auto", "geosite", []config.Rule{
		{Type: "rule_set", Value: "geosite:apple", Enabled: true}})
	if err != nil {
		t.Fatalf("CreateGroupIn: %v", err)
	}
	// 显式 --kind 覆盖推断
	if g3.Kind != config.KindGeosite {
		t.Errorf("显式层 = %q, 期望 geosite", g3.Kind)
	}
	g4, err := m.CreateGroupIn("被覆盖的组", "Auto", config.KindACL, []config.Rule{
		{Type: "rule_set", Value: "geosite:cn", Enabled: true}})
	if err != nil {
		t.Fatalf("CreateGroupIn: %v", err)
	}
	if g4.Kind != config.KindACL {
		t.Errorf("显式层应覆盖推断，实得 %q", g4.Kind)
	}
	if _, err := m.CreateGroupIn("坏层", "Auto", "nosuch", nil); err == nil {
		t.Error("未知层应被拒绝")
	}

	// Position 是层内序号：geosite 层两个组为 0、1；geoip 层从 0 起
	if g1.Position != 0 || g3.Position != 1 {
		t.Errorf("geosite 层内序号 = %d,%d, 期望 0,1", g1.Position, g3.Position)
	}
	if g2.Position != 0 {
		t.Errorf("geoip 层内序号 = %d, 期望 0（层内独立计数）", g2.Position)
	}
}

// --- 6.3 final 结构约束 ---

func TestFinalLayerStructuralConstraints(t *testing.T) {
	m := newTestManager(t)

	// 1) kind=final 却不含 final 规则 → 报错
	_, err := m.CreateGroupIn("空 final", "Auto", config.KindFinal, []config.Rule{
		{Type: "domain", Value: "x.com", Enabled: true}})
	if err == nil {
		t.Fatal("kind=final 却不含 final 规则应被拒绝")
	}
	if !strings.Contains(err.Error(), "空 final") {
		t.Errorf("错误信息应含组名: %v", err)
	}
	if !strings.Contains(err.Error(), "必须且仅有一条") {
		t.Errorf("错误信息应给出修复方式: %v", err)
	}

	// 2) kind=final 但含多条规则（final + 其它）→ 报错
	if _, err := m.CreateGroupIn("多规则 final", "Auto", config.KindFinal, []config.Rule{
		{Type: "final", Enabled: true},
		{Type: "domain", Value: "x.com", Enabled: true}}); err == nil {
		t.Fatal("kind=final 含多余规则应被拒绝")
	}

	// 3) kind≠final 却含 final 规则 → 报错
	_, err = m.CreateGroupIn("混入 final", "Auto", config.KindCustom, []config.Rule{
		{Type: "final", Enabled: true}})
	if err == nil {
		t.Fatal("非 final 层含 final 规则应被拒绝")
	}
	if !strings.Contains(err.Error(), "final 层") {
		t.Errorf("错误信息应给出修复方式: %v", err)
	}

	// 4) 合法：唯一的 final 组，且允许停用
	g, err := m.CreateGroupIn("Final", "Auto", config.KindFinal, []config.Rule{{Type: "final", Enabled: true}})
	if err != nil {
		t.Fatalf("创建 final 组: %v", err)
	}
	if err := m.SetEnabled(g.ID, false); err != nil {
		t.Errorf("final 组应允许停用: %v", err)
	}
	active, err := m.activeFinalGroup()
	if err != nil {
		t.Fatalf("activeFinalGroup: %v", err)
	}
	if active != nil {
		t.Errorf("final 组停用后不应有活动 final，实得 %q", active.Name)
	}
	if err := m.SetEnabled(g.ID, true); err != nil {
		t.Fatalf("重新启用 final: %v", err)
	}
	if active, _ = m.activeFinalGroup(); active == nil || active.Name != "Final" {
		t.Errorf("启用后应有活动 final 组，实得 %v", active)
	}
}

// --- 6.5 层内移动与跨层移动（模型层语义）---

func TestMoveGroupWithinLayer(t *testing.T) {
	m := newTestManager(t)
	names := []string{"c1", "c2", "c3"}
	ids := map[string]int64{}
	for _, n := range names {
		g, err := m.CreateGroup(n, "Auto", []config.Rule{{Type: "domain", Value: n + ".com", Enabled: true}})
		if err != nil {
			t.Fatalf("CreateGroup(%s): %v", n, err)
		}
		ids[n] = g.ID
	}

	order := func() string {
		groups, err := m.DB.ListRoutingGroups()
		if err != nil {
			t.Fatalf("ListRoutingGroups: %v", err)
		}
		var got []string
		for _, g := range groups {
			got = append(got, g.Name)
		}
		return strings.Join(got, ",")
	}

	if got := order(); got != "c1,c2,c3" {
		t.Fatalf("初始顺序 = %q", got)
	}
	// c2 上移
	if err := m.MoveGroup(ids["c2"], -1); err != nil {
		t.Fatalf("MoveGroup: %v", err)
	}
	if got := order(); got != "c2,c1,c3" {
		t.Errorf("上移后顺序 = %q, 期望 c2,c1,c3", got)
	}
	// c2 已在层首，再上移不动
	if err := m.MoveGroup(ids["c2"], -1); err != nil {
		t.Fatalf("边界上移应无副作用: %v", err)
	}
	if got := order(); got != "c2,c1,c3" {
		t.Errorf("边界移动后顺序 = %q, 期望不变", got)
	}
	// 移到层尾后下移不动
	if err := m.MoveGroup(ids["c2"], 1); err != nil {
		t.Fatalf("MoveGroup: %v", err)
	}
	if err := m.MoveGroup(ids["c2"], 1); err != nil {
		t.Fatalf("MoveGroup: %v", err)
	}
	if got := order(); got != "c1,c3,c2" {
		t.Errorf("下移后顺序 = %q, 期望 c1,c3,c2", got)
	}
	// 层内序号连续
	groups, _ := m.DB.ListRoutingGroups()
	for i, g := range groups {
		if g.Position != i {
			t.Errorf("层内序号不连续: %q.position = %d, 期望 %d", g.Name, g.Position, i)
		}
	}
}

func TestMoveGroupToOtherLayer(t *testing.T) {
	m := newTestManager(t)
	a, err := m.CreateGroupIn("域名组", "Auto", config.KindGeosite, []config.Rule{
		{Type: "rule_set", Value: "geosite:cn", Enabled: true}})
	if err != nil {
		t.Fatalf("CreateGroupIn: %v", err)
	}
	b, err := m.CreateGroup("手工组", "Auto", []config.Rule{{Type: "domain", Value: "a.com", Enabled: true}})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	c, err := m.CreateGroup("手工组2", "Auto", []config.Rule{{Type: "domain", Value: "b.com", Enabled: true}})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	// 跨层：手工组 → geosite 层，追加到层尾
	if err := m.MoveGroupToKind(b.ID, config.KindGeosite, -1); err != nil {
		t.Fatalf("MoveGroupToKind: %v", err)
	}
	groups, _ := m.DB.ListRoutingGroups()
	got := []string{}
	for _, g := range groups {
		got = append(got, g.Name)
	}
	// custom 层剩 手工组2；geosite 层 = 域名组, 手工组
	if strings.Join(got, ",") != "手工组2,域名组,手工组" {
		t.Errorf("跨层后顺序 = %v", got)
	}
	// 源层序号去空档
	if groups[0].Position != 0 {
		t.Errorf("源层序号未重排: %d", groups[0].Position)
	}

	// 指定位置插入：把 手工组2 插到 geosite 层首位
	if err := m.MoveGroupToKind(c.ID, config.KindGeosite, 0); err != nil {
		t.Fatalf("MoveGroupToKind: %v", err)
	}
	groups, _ = m.DB.ListRoutingGroups()
	got = got[:0]
	for _, g := range groups {
		got = append(got, g.Name)
	}
	if strings.Join(got, ",") != "手工组2,域名组,手工组" {
		t.Errorf("插入到首位后顺序 = %v, 期望 手工组2,域名组,手工组", got)
	}
	for i, g := range groups {
		if g.Position != i {
			t.Errorf("层内序号不连续: %q.position = %d, 期望 %d", g.Name, g.Position, i)
		}
	}
	_ = a
}

// TestMoveGroupIntoFinalLayerNeedsFinalRule 移入 final 层时结构约束必须生效：
// 普通规则组不能"借"跨层移动变成 final 组。
func TestMoveGroupIntoFinalLayerNeedsFinalRule(t *testing.T) {
	m := newTestManager(t)
	g, err := m.CreateGroup("手工组", "Auto", []config.Rule{{Type: "domain", Value: "a.com", Enabled: true}})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	err = m.MoveGroupToKind(g.ID, config.KindFinal, -1)
	if err == nil {
		t.Fatal("不含 final 规则的组不应能移入 final 层")
	}
	if !strings.Contains(err.Error(), "必须且仅有一条") {
		t.Errorf("错误信息应说明 final 层的结构要求: %v", err)
	}
	// 层未被改动
	after, _ := m.DB.GetRoutingGroup(g.ID)
	if after.Kind != config.KindCustom {
		t.Errorf("失败的移动不应改变层，实得 %q", after.Kind)
	}
}

// TestUpdateGroupKindChangeRelocates 在编辑表单里改层等价于跨层移动：追加到目标层末尾。
func TestUpdateGroupKindChangeRelocates(t *testing.T) {
	m := newTestManager(t)
	a, _ := m.CreateGroup("a", "Auto", []config.Rule{{Type: "domain", Value: "a.com", Enabled: true}})
	b, _ := m.CreateGroup("b", "Auto", []config.Rule{{Type: "domain", Value: "b.com", Enabled: true}})

	got, err := m.DB.GetRoutingGroup(a.ID)
	if err != nil {
		t.Fatalf("GetRoutingGroup: %v", err)
	}
	got.Kind = config.KindGeoIP
	if err := m.UpdateGroup(got, got.Rules); err != nil {
		t.Fatalf("UpdateGroup: %v", err)
	}
	groups, _ := m.DB.ListRoutingGroups()
	names := []string{}
	for _, g := range groups {
		names = append(names, g.Name)
	}
	if strings.Join(names, ",") != "b,a" {
		t.Errorf("改层后顺序 = %v, 期望 b,a（a 进 geoip 层，排在 custom 之后）", names)
	}
	back, _ := m.DB.GetRoutingGroup(a.ID)
	if back.Kind != config.KindGeoIP || back.Position != 0 {
		t.Errorf("改层后 = kind %q position %d, 期望 geoip/0", back.Kind, back.Position)
	}
	_ = b
}
