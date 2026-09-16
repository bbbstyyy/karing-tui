package routing

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/catalog"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// -update 重新生成预置方案的 golden 文件（go test ./internal/routing/ -update）
var update = flag.Bool("update", false, "重新生成 golden 快照文件")

// Phase 7：CN 地区默认分流方案（对照 karing assets/datas/preset/cn.json）。

// TestLoadPresetCNMaps28To27 7.1/7.2：28 条 → 27 组。
// 跳过的「📢 苹果推送通知」没有 rule_set_build_in（原条目只靠包名/进程名与非规则集条件，
// 桌面端无按应用分流），导入时跳过并出现在报告里。
func TestLoadPresetCNMaps28To27(t *testing.T) {
	entries, err := LoadPreset(PresetCN)
	if err != nil {
		t.Fatalf("LoadPreset: %v", err)
	}
	var groups []*config.RoutingGroup
	var skipped []string
	for _, entry := range entries {
		if entry.Skipped != "" {
			skipped = append(skipped, entry.Name)
			continue
		}
		groups = append(groups, entry.Group)
	}
	if len(skipped) != 1 || skipped[0] != "📢 苹果推送通知" {
		t.Errorf("应恰好跳过「📢 苹果推送通知」，实得 %v", skipped)
	}
	if len(groups) != 27 {
		t.Fatalf("27 组期望，实得 %d", len(groups))
	}

	// 层内 Position 取 0..26 连续值（跳过第 4 条后按入选顺序重编号）
	for i, g := range groups {
		if g.Position != i {
			t.Errorf("第 %d 组 %q 的层内 Position = %d, 期望 %d（Position 必须显式赋值）", i, g.Name, g.Position, i)
		}
	}
	// Kind 一律 custom（依据见 config/routing_kind.go：引入层序不是为了重新归类预置）
	for _, g := range groups {
		if g.Kind != config.KindCustom {
			t.Errorf("预置组 %q 的 Kind = %q, 期望 custom", g.Name, g.Kind)
		}
		if len(g.Rules) != 1 || g.Rules[0].Type != "rule_set" {
			t.Errorf("预置组 %q 应是唯一一条 rule_set 规则，实得 %+v", g.Name, g.Rules)
		}
	}

	// 抽查目标三态映射（direct → DIRECT、block → BLOCK、currentSelected → Auto）
	want := map[string]struct {
		target  string
		enabled bool
	}{
		"🛑 广告拦截":        {"BLOCK", false},
		"🍎 苹果服务":        {"DIRECT", true},
		"🌏 Google Play": {"Auto", true},
		"🌏 Google":      {"Auto", true},
		"Ⓜ️ 微软云盘":       {"DIRECT", false},
		"📺 哔哩哔哩":        {"DIRECT", true},
		"🎯 国内直连":        {"DIRECT", true},
		"🌏 国外穿墙":        {"Auto", true},
	}
	for name, expect := range want {
		found := false
		for _, g := range groups {
			if g.Name != name {
				continue
			}
			found = true
			if g.Target != expect.target {
				t.Errorf("%q 目标 = %q, 期望 %q", name, g.Target, expect.target)
			}
			if g.Enabled != expect.enabled {
				t.Errorf("%q 默认启用 = %v, 期望 %v", name, g.Enabled, expect.enabled)
			}
		}
		if !found {
			t.Errorf("预置缺少分组 %q", name)
		}
	}

	// 全部 27 组里应有 6 个启用 + 21 个停用（7.2）
	enabled := 0
	for _, g := range groups {
		if g.Enabled {
			enabled++
		}
	}
	if enabled != 6 {
		t.Errorf("默认启用的组数 = %d, 期望 6（停用组同样建出，供用户一键启用）", enabled)
	}
}

// TestPresetRefsAllResolvable 7.7：把 6.0 的码表修复固化成回归——
// 预置的全部引用都必须能被 catalog.Parse 解析，否则规则写入时被 checkRuleSetTags 拒绝。
func TestPresetRefsAllResolvable(t *testing.T) {
	entries, err := LoadPreset(PresetCN)
	if err != nil {
		t.Fatalf("LoadPreset: %v", err)
	}
	for _, entry := range entries {
		if entry.Group == nil {
			continue
		}
		for _, tag := range strings.Split(entry.Group.Rules[0].Value, ",") {
			if !catalog.IsRef(strings.TrimSpace(tag)) {
				t.Errorf("预置组 %q 的引用 %q 无法解析（码表缺失或被改名）", entry.Name, tag)
			}
		}
	}
}

// TestPresetDropsDanglingGeoIPBing 7.1：geoip:bing 是真悬空引用
// （快照无 bing.srs，karing 的 geoip_codes.txt 也没有该码），必须剔除且只保留 geosite:bing，
// 并把偏差记录在案（不因单个悬空码使整条预置失败）。
func TestPresetDropsDanglingGeoIPBing(t *testing.T) {
	entries, err := LoadPreset(PresetCN)
	if err != nil {
		t.Fatalf("LoadPreset: %v", err)
	}
	for _, entry := range entries {
		if entry.Group == nil || entry.Group.Name != "Ⓜ️ 微软Bing" {
			continue
		}
		value := entry.Group.Rules[0].Value
		if strings.Contains(value, "geoip:bing") {
			t.Errorf("geoip:bing 是悬空引用，不应保留: %s", value)
		}
		if value != "geosite:bing" {
			t.Errorf("微软Bing 应只保留 geosite:bing，实得 %q", value)
		}
		if len(entry.Notes) == 0 {
			t.Error("剔除悬空引用必须记录偏差，不能静默丢弃")
		}
		return
	}
	t.Fatal("预置缺少「Ⓜ️ 微软Bing」")
}

// newPresetTestManager 造一个带 Auto/Manual 代理组与两个节点的库，供预置对齐测试用。
func newPresetTestManager(t *testing.T) *Manager {
	t.Helper()
	m := newTestManager(t) // newTestManager 已建 Auto（urltest）代理组
	if err := m.DB.CreateProxyGroup(&config.ProxyGroup{
		Name: storage.DefaultGroupManual, Type: "select", Members: []config.ProxyGroupMember{{Type: "all"}},
	}); err != nil {
		t.Fatalf("预置 Manual: %v", err)
	}
	if err := m.DB.CreateNode(&config.Node{
		Name: "节点-1", Protocol: "trojan", Server: "1.1.1.1", Port: 443, Enabled: true,
		Metadata: map[string]any{"password": "p"},
	}); err != nil {
		t.Fatalf("预置节点: %v", err)
	}
	return m
}

// TestApplyPresetMerge 7.4：merge 按名称跳过已存在的组，可重复执行。
func TestApplyPresetMerge(t *testing.T) {
	m := newPresetTestManager(t)

	first, err := m.ApplyPreset(PresetCN, PresetMerge)
	if err != nil {
		t.Fatalf("ApplyPreset: %v", err)
	}
	if first.Added != 27+1 { // 27 组 + 兜底组
		t.Errorf("首次 merge 应新增 28 个分流组，实得 %d", first.Added)
	}
	if first.Failed != 0 {
		t.Errorf("首次 merge 不应有失败项: %v", first.FailedItems())
	}
	groups, err := m.DB.ListRoutingGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 28 {
		t.Fatalf("应用预置后分流组数 = %d, 期望 28（27 组 + 兜底组）", len(groups))
	}

	// 重复执行：全部跳过，不改写
	second, err := m.ApplyPreset(PresetCN, PresetMerge)
	if err != nil {
		t.Fatalf("重复 ApplyPreset: %v", err)
	}
	if second.Added != 0 || second.Failed != 0 || second.Skipped != 28 {
		t.Errorf("重复 merge 应全部跳过（新增 %d / 跳过 %d / 失败 %d）", second.Added, second.Skipped, second.Failed)
	}
	after, _ := m.DB.ListRoutingGroups()
	if len(after) != 28 {
		t.Errorf("重复 merge 后分流组数 = %d, 期望 28", len(after))
	}
}

// TestApplyPresetReplace 7.4：replace 先删除现有全部分流组再写入。
func TestApplyPresetReplace(t *testing.T) {
	m := newPresetTestManager(t)
	if _, err := m.CreateGroup("我自己的组", "Auto", []config.Rule{
		{Type: "domain_suffix", Value: "mine.example", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	report, err := m.ApplyPreset(PresetCN, PresetReplace)
	if err != nil {
		t.Fatalf("ApplyPreset: %v", err)
	}
	if report.Added != 28 || report.Failed != 0 {
		t.Errorf("replace 应新增 28 组，实得 新增 %d / 失败 %d（%v）", report.Added, report.Failed, report.FailedItems())
	}
	groups, _ := m.DB.ListRoutingGroups()
	for _, g := range groups {
		if g.Name == "我自己的组" {
			t.Error("replace 模式应删除现有分流组")
		}
	}
	if len(groups) != 28 {
		t.Errorf("replace 后分流组数 = %d, 期望 28", len(groups))
	}
}

// TestApplyPresetFinalTargetManual 7.3：兜底组 Final 的目标是 Manual（select）。
func TestApplyPresetFinalTargetManual(t *testing.T) {
	m := newPresetTestManager(t)
	if _, err := m.ApplyPreset(PresetCN, PresetMerge); err != nil {
		t.Fatal(err)
	}
	var final *config.RoutingGroup
	groups, _ := m.DB.ListRoutingGroups()
	for _, g := range groups {
		if config.KindNormalize(g.Kind) == config.KindFinal {
			if final != nil {
				t.Fatal("final 层出现多个分组")
			}
			final = g
		}
	}
	if final == nil {
		t.Fatal("预置应建立 final 兜底组")
	}
	if final.Name != PresetFinalGroup {
		t.Errorf("兜底组名 = %q, 期望 %q", final.Name, PresetFinalGroup)
	}
	if final.Target != storage.DefaultGroupManual {
		t.Errorf("兜底目标 = %q, 期望 %q（select 手动选择，与代理类目标 Auto 分开）", final.Target, storage.DefaultGroupManual)
	}
	if len(final.Rules) != 1 || final.Rules[0].Type != "final" {
		t.Errorf("兜底组必须且仅有一条 final 规则，实得 %+v", final.Rules)
	}
}

// TestApplyPresetEnsuresProxyGroups 7.4：对齐入口不能依赖 statusDefaultGroups 的展示兜底，
// 用户删掉过 Manual 时必须就地补建，否则写入的兜底组指向不存在的出站。
func TestApplyPresetEnsuresProxyGroups(t *testing.T) {
	m := newPresetTestManager(t)
	groups, _ := m.DB.ListProxyGroups()
	for _, g := range groups {
		if g.Name == storage.DefaultGroupManual {
			if err := m.DB.DeleteProxyGroup(g.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	report, err := m.ApplyPreset(PresetCN, PresetMerge)
	if err != nil {
		t.Fatalf("ApplyPreset: %v", err)
	}
	found := false
	for _, item := range report.Items {
		if strings.Contains(item.Detail, storage.DefaultGroupManual) {
			found = true
		}
	}
	if !found {
		t.Errorf("补建代理组应出现在报告里: %+v", report.Items)
	}
	proxyGroups, _ := m.DB.ListProxyGroups()
	var manual *config.ProxyGroup
	for _, g := range proxyGroups {
		if g.Name == storage.DefaultGroupManual {
			manual = g
		}
	}
	if manual == nil {
		t.Fatal("Manual 代理组应被就地补建")
	}
}

// TestApplyPresetKeepsExistingFinal 已有 final 兜底组时不新建、也不改写它的目标。
func TestApplyPresetKeepsExistingFinal(t *testing.T) {
	m := newPresetTestManager(t)
	if _, err := m.CreateGroupIn("Final", "Auto", config.KindFinal,
		[]config.Rule{{Type: "final", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	report, err := m.ApplyPreset(PresetCN, PresetMerge)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range report.Items {
		if item.Name == PresetFinalGroup && item.Action == "跳过" && strings.Contains(item.Detail, "final 层") {
			found = true
		}
	}
	if !found {
		t.Errorf("已有 final 兜底组时应跳过并说明: %+v", report.Items)
	}
	groups, _ := m.DB.ListRoutingGroups()
	for _, g := range groups {
		if config.KindNormalize(g.Kind) == config.KindFinal && g.Target != "Auto" {
			t.Errorf("既有兜底组的目标不应被改写，实得 %q", g.Target)
		}
	}
}

// presetSnapshot 构造「预置方案」的完整快照，供生成器 golden 比对。
func presetSnapshot() (config.Snapshot, []*config.RoutingGroup, error) {
	entries, err := LoadPreset(PresetCN)
	if err != nil {
		return config.Snapshot{}, nil, err
	}
	var groups []*config.RoutingGroup
	for _, entry := range entries {
		if entry.Group != nil {
			groups = append(groups, entry.Group)
		}
	}
	final := &config.RoutingGroup{
		Name: PresetFinalGroup, Target: storage.DefaultGroupManual, Kind: config.KindFinal, Position: 0, Enabled: true,
		Rules: []config.Rule{{Type: "final", Enabled: true}},
	}
	groups = append(groups, final)
	nodes := []*config.Node{
		{ID: 1, Name: "hk-01", Protocol: "trojan", Server: "1.1.1.1", Port: 443, TLS: true, Enabled: true,
			Metadata: map[string]any{"password": "p1"}},
		{ID: 2, Name: "us-01", Protocol: "vless", Server: "2.2.2.2", Port: 443, TLS: true, Enabled: true,
			Metadata: map[string]any{"uuid": "u2"}},
	}
	proxyGroups := []*config.ProxyGroup{
		{ID: 1, Name: storage.DefaultGroupAuto, Type: "urltest", IntervalS: 300,
			Members: []config.ProxyGroupMember{{Type: "all"}}},
		{ID: 2, Name: storage.DefaultGroupManual, Type: "select",
			Members: []config.ProxyGroupMember{{Type: "all"}}},
	}
	return config.Snapshot{
		Settings:      config.DefaultSettings(),
		Nodes:         nodes,
		ProxyGroups:   proxyGroups,
		RoutingGroups: groups,
	}, groups, nil
}

// TestGeneratePresetCNKeepsCNJSONOrder 7.7 第一段（先证伪）：套用 27 组后，生成的
// route.rules 顺序必须与 cn.json 完全一致 —— 证明加层没有改变优先级。
// 若此处不一致，说明 6.1–6.4 的层序实现有误，要先修模型而不是调数据。
func TestGeneratePresetCNKeepsCNJSONOrder(t *testing.T) {
	snap, groups, err := presetSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	out, err := config.Generate(snap)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var parsed struct {
		Route struct {
			Rules []struct {
				RuleSet []string `json:"rule_set,omitempty"`
				Final   string   `json:"outbound,omitempty"`
				Action  string   `json:"action,omitempty"`
			} `json:"rules"`
			Final string `json:"final"`
		} `json:"route"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("解析生成配置: %v", err)
	}
	// 生成器前置 2 条动作规则（hijack-dns / sniff）+ 1 条内网直连，其后才是用户规则
	offset := 0
	for i, rule := range parsed.Route.Rules {
		if len(rule.RuleSet) == 0 {
			offset = i + 1
			continue
		}
		break
	}
	userRules := parsed.Route.Rules[offset:]
	if len(userRules) != 6 { // 27 组中默认启用 6 组 → 6 条用户规则
		t.Fatalf("启用的用户规则数 = %d, 期望 6", len(userRules))
	}
	wantOrder := []string{
		"geosite-apple,geosite-apple@ads,geosite-apple-dev,geosite-apple-pki,geosite-apple-update", // 🍎 苹果服务
		"geosite-google-play",          // 🌏 Google Play
		"geosite-google,geoip-google",  // 🌏 Google
		"acl-BilibiliHMT,acl-Bilibili", // 📺 哔哩哔哩
		"acl-ChinaIp,acl-ChinaDomain,acl-ChinaCompanyIp,acl-UnBan,acl-SteamCN,acl-Download,acl-ChinaMedia", // 🎯 国内直连
		"geosite-geolocation-!cn,geosite-google,geoip-google,acl-ProxyGFWlist,acl-ProxyMedia",              // 🌏 国外穿墙
	}
	for i, rule := range userRules {
		if strings.Join(rule.RuleSet, ",") != wantOrder[i] {
			t.Errorf("第 %d 条用户规则 = %v, 期望 %s\n（顺序应与 cn.json 一致：层序 + 层内 Position）",
				i, rule.RuleSet, wantOrder[i])
		}
	}
	// 兜底指向 Manual，且该出站存在
	if parsed.Route.Final != storage.DefaultGroupManual {
		t.Errorf("route.final = %q, 期望 %q", parsed.Route.Final, storage.DefaultGroupManual)
	}
	var outbounds []string
	for _, g := range groups {
		_ = g
	}
	var raw struct {
		Outbounds []struct {
			Tag  string `json:"tag"`
			Type string `json:"type"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	for _, ob := range raw.Outbounds {
		outbounds = append(outbounds, ob.Tag)
	}
	if !strings.Contains(strings.Join(outbounds, ","), storage.DefaultGroupManual) {
		t.Errorf("出站缺少 Manual 组: %v", outbounds)
	}
}

// TestGeneratePresetCNGolden 用 golden 锁定预置方案的完整生成结果；
// 生成器或预置数据变更时执行 go test ./internal/routing/ -update 并逐处核对 diff。
func TestGeneratePresetCNGolden(t *testing.T) {
	snap, _, err := presetSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	out, err := config.Generate(snap)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	golden := filepath.Join("testdata", "golden-preset-cn.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, out, 0o644); err != nil {
			t.Fatalf("写入 golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("读取 golden 失败（先运行 go test ./internal/routing/ -update 生成基线）: %v", err)
	}
	if string(want) != string(out) {
		i := 0
		for i < len(want) && i < len(out) && want[i] == out[i] {
			i++
		}
		lo := i - 80
		if lo < 0 {
			lo = 0
		}
		hiW, hiO := i+80, i+80
		if hiW > len(want) {
			hiW = len(want)
		}
		if hiO > len(out) {
			hiO = len(out)
		}
		t.Fatalf("预置方案生成结果与 golden 不一致（位置 %d）:\n  golden: ...%s...\n  输出:   ...%s...",
			i, want[lo:hiW], out[lo:hiO])
	}
}

// TestPresetDownloadCounts 7.5：启用 6 条引用去重 20 个规则集（其中 IP 类 7 个）；
// 27 组全部启用时去重 66 个（评估按需下载耗时与进度提示的口径）。
func TestPresetDownloadCounts(t *testing.T) {
	entries, err := LoadPreset(PresetCN)
	if err != nil {
		t.Fatal(err)
	}
	dedup := func(enabledOnly bool) map[string]bool {
		refs := map[string]bool{}
		for _, entry := range entries {
			if entry.Group == nil {
				continue
			}
			if enabledOnly && !entry.Group.Enabled {
				continue
			}
			for _, tag := range strings.Split(entry.Group.Rules[0].Value, ",") {
				refs[strings.TrimSpace(tag)] = true
			}
		}
		return refs
	}
	enabledRefs := dedup(true)
	if len(enabledRefs) != 20 {
		t.Errorf("启用引用去重 = %d, 期望 20", len(enabledRefs))
	}
	ipRefs := 0
	for ref := range enabledRefs {
		parsed, _ := catalog.Parse(ref)
		if parsed.NeedsResolve() {
			ipRefs++
		}
	}
	if ipRefs != 7 {
		t.Errorf("启用引用中的 IP 类 = %d, 期望 7", ipRefs)
	}
	allRefs := dedup(false)
	// checklist 7.5 记的最坏情况是 66 个：按 cn.json 原文算（含 geoip:bing）。
	// 7.1 剔除 geoip:bing 这个悬空引用后为 65 个。
	if len(allRefs) != 65 {
		t.Errorf("全部启用时引用去重 = %d, 期望 65（66 减去被剔除的 geoip:bing）", len(allRefs))
	}
	// resolve_ip_rules 默认关；开启时 resolve 插到第一条 IP 类规则之前（4.1），
	// 规则顺序变了会改变插入位置，这里锁定「确实存在 IP 类规则」这一前提。
	if ipRefs == 0 {
		t.Error("应有 IP 类规则参与 ruleNeedsResolve 判定")
	}
}

// TestPresetResolveInsertPosition 7.5：resolve_ip_rules 开启时，resolve 规则插到
// **第一条 IP 类规则**之前。预置方案的规则顺序变了（第一条 IP 类是「📺 哔哩哔哩」的
// acl-BilibiliHMT，不再是旧方案的 geosite-cn），插入位置必须跟着变而不是写死。
func TestPresetResolveInsertPosition(t *testing.T) {
	snap, _, err := presetSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	snap.Settings.ResolveIPRules = true
	out, err := config.Generate(snap)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Route struct {
			Rules []struct {
				RuleSet []string `json:"rule_set,omitempty"`
				Action  string   `json:"action,omitempty"`
			} `json:"rules"`
		} `json:"route"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	resolveIdx, firstIPIdx := -1, -1
	for i, rule := range parsed.Route.Rules {
		if rule.Action == "resolve" {
			resolveIdx = i
			continue
		}
		if firstIPIdx < 0 && len(rule.RuleSet) > 0 {
			for _, tag := range rule.RuleSet {
				if ref, ok := catalog.Parse(tag); ok && ref.NeedsResolve() {
					firstIPIdx = i
					break
				}
			}
		}
	}
	if resolveIdx < 0 || firstIPIdx < 0 {
		t.Fatalf("resolve=%d firstIP=%d（两者都应存在）", resolveIdx, firstIPIdx)
	}
	// 插入后第一条 IP 类规则紧跟在 resolve 之后
	if firstIPIdx != resolveIdx+1 {
		t.Errorf("resolve 规则应紧贴第一条 IP 类规则之前（resolve=%d, firstIP=%d）", resolveIdx, firstIPIdx)
	}
	// 它前面的规则都不是 IP 类
	for i := 0; i < resolveIdx; i++ {
		for _, tag := range parsed.Route.Rules[i].RuleSet {
			if ref, ok := catalog.Parse(tag); ok && ref.NeedsResolve() {
				t.Errorf("resolve 之前存在 IP 类规则 %q（位置 %d）", tag, i)
			}
		}
	}
}

// TestManualWithoutSelectionKeepsNoDefault 7.3 风险项：Manual 未手动选择时
// （Selected 为空）生成的 selector 不写 default，sing-box 回退到成员列表首个节点
// ——「未选择 = 走订阅里排序第一个节点」，不是「不走代理」。此语义已写入 Settings 说明。
func TestManualWithoutSelectionKeepsNoDefault(t *testing.T) {
	snap, _, err := presetSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	out, err := config.Generate(snap)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, ob := range parsed.Outbounds {
		if ob["tag"] != storage.DefaultGroupManual {
			continue
		}
		if ob["type"] != "selector" {
			t.Errorf("Manual 应生成为 selector，实得 %v", ob["type"])
		}
		if _, has := ob["default"]; has {
			t.Errorf("Manual 未选择时不应输出 default 字段: %v", ob)
		}
		members, _ := ob["outbounds"].([]any)
		if len(members) == 0 {
			t.Error("Manual 成员列表为空，兜底会落到 direct")
		}
		return
	}
	t.Fatal("生成配置缺少 Manual 组")
}
