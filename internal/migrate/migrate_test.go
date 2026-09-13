package migrate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// setupDB 准备隔离的 KARING_HOME 与数据库；预建内置规则集本地缓存避免联网。
func setupDB(t *testing.T) *storage.DB {
	t.Helper()
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	db, err := storage.Open(paths)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

const clashSample = `
proxies:
  - name: 香港 01
    type: ss
    server: hk.example.com
    port: 8388
    cipher: aes-128-gcm
    password: pw1
  - name: 美国 02
    type: trojan
    server: us.example.com
    port: 443
    password: pw2
    skip-cert-verify: true
  - name: 日本 03
    type: vmess
    server: jp.example.com
    port: 443
    uuid: 11111111-2222-3333-4444-555555555555
    alterId: 0
    cipher: auto
    tls: true
    network: ws
    ws-opts:
      path: /ws
proxy-groups:
  - name: 节点选择
    type: select
    proxies: [自动选择, 香港 01, 美国 02, DIRECT]
  - name: 自动选择
    type: urltest
    url: http://www.gstatic.com/generate_204
    interval: 300
    proxies: [香港 01, 日本 03]
  - name: 故障转移
    type: fallback
    proxies: [香港 01, 自动选择]
rules:
  - DOMAIN-SUFFIX,cn,DIRECT
  - IP-CIDR,192.168.0.0/16,DIRECT,no-resolve
  - DOMAIN-SUFFIX,google.com,节点选择
  - DOMAIN,example.org,节点选择
  - RULE-SET,provider1,节点选择
  - GEOIP,CN,DIRECT
  - MATCH,节点选择
`

func TestImportClash(t *testing.T) {
	db := setupDB(t)

	rep, err := ImportClash(db, clashSample)
	if err != nil {
		t.Fatalf("ImportClash: %v", err)
	}
	if rep.Nodes != 3 {
		t.Errorf("节点数 = %d, 期望 3", rep.Nodes)
	}
	if rep.Groups != 3 {
		t.Errorf("代理组数 = %d, 期望 3", rep.Groups)
	}

	groups, _ := db.ListProxyGroups()
	byName := map[string]*config.ProxyGroup{}
	for _, g := range groups {
		byName[g.Name] = g
	}
	sel := byName["节点选择"]
	if sel == nil || sel.Type != "select" || len(sel.Members) != 3 {
		t.Fatalf("节点选择组不符: %+v", sel)
	}
	// DIRECT 成员被跳过
	auto := byName["自动选择"]
	if auto == nil || auto.Type != "urltest" || auto.IntervalS != 300 || len(auto.Members) != 2 {
		t.Errorf("自动选择组不符: %+v", auto)
	}
	// fallback 映射为 urltest
	fb := byName["故障转移"]
	if fb == nil || fb.Type != "urltest" {
		t.Errorf("故障转移应映射为 urltest: %+v", fb)
	}

	routings, _ := db.ListRoutingGroups()
	rgByName := map[string]*config.RoutingGroup{}
	for _, g := range routings {
		rgByName[g.Name] = g
	}
	direct := rgByName["迁移-DIRECT"]
	if direct == nil || direct.Target != "DIRECT" || len(direct.Rules) != 2 {
		t.Errorf("迁移-DIRECT 组不符: %+v", direct)
	}
	proxy := rgByName["迁移-节点选择"]
	if proxy == nil || proxy.Target != "节点选择" || len(proxy.Rules) != 2 {
		t.Errorf("迁移-节点选择 组不符: %+v", proxy)
	}
	directLater := rgByName["迁移-DIRECT 2"]
	if directLater == nil || directLater.Target != "DIRECT" || len(directLater.Rules) != 1 || directLater.Rules[0].Type != "geoip" {
		t.Errorf("后续 DIRECT 规则不应被提前合并: %+v", directLater)
	}
	fin := rgByName["迁移-Final"]
	if fin == nil || len(fin.Rules) != 1 || fin.Rules[0].Type != "final" {
		t.Errorf("迁移-Final 组不符: %+v", fin)
	}

	// RULE-SET 与保留成员出现在跳过清单
	joined := strings.Join(rep.Skipped, "\n")
	if !strings.Contains(joined, "RULE-SET") || !strings.Contains(joined, "DIRECT") {
		t.Errorf("跳过清单缺预期项: %v", rep.Skipped)
	}

	// 导入的状态可生成配置
	snap, err := buildSnapshot(db)
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	if _, err := config.Generate(snap); err != nil {
		t.Errorf("导入状态生成配置失败: %v", err)
	}
}

func TestImportClashPreservesNonConsecutiveRuleOrder(t *testing.T) {
	db := setupDB(t)
	content := `
proxies:
  - name: node
    type: ss
    server: 1.2.3.4
    port: 8388
    cipher: aes-128-gcm
    password: pw
rules:
  - DOMAIN,first.example,DIRECT
  - DOMAIN,second.example,REJECT
  - DOMAIN,third.example,DIRECT
`
	rep, err := ImportClash(db, content)
	if err != nil {
		t.Fatalf("ImportClash: %v", err)
	}
	if rep.Routings != 3 {
		t.Fatalf("应为 3 个连续规则段，得到 %d", rep.Routings)
	}
	routings, err := db.ListRoutingGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(routings) != 3 {
		t.Fatalf("分流组数量 = %d, 期望 3", len(routings))
	}
	// REJECT is normalized to BLOCK; the third rule remains DIRECT and must
	// not be moved before the BLOCK segment.
	wantTargets := []string{"DIRECT", "BLOCK", "DIRECT"}
	wantValues := []string{"first.example", "second.example", "third.example"}
	for i, rg := range routings {
		if rg.Target != wantTargets[i] || len(rg.Rules) != 1 || rg.Rules[0].Value != wantValues[i] {
			t.Errorf("分流组 %d = target=%q rules=%+v, 期望 target=%q value=%q", i, rg.Target, rg.Rules, wantTargets[i], wantValues[i])
		}
	}
}

func TestImportClashNameConflict(t *testing.T) {
	db := setupDB(t)
	// 预置一个 "Manual" 组（默认组仅在 application.New 创建，纯 storage 无默认组）
	if err := db.CreateProxyGroup(&config.ProxyGroup{Name: "Manual", Type: "select"}); err != nil {
		t.Fatalf("预置组: %v", err)
	}
	yaml := `
proxies:
  - name: n1
    type: ss
    server: a
    port: 1
    cipher: aes-128-gcm
    password: p
proxy-groups:
  - name: Manual
    type: select
    proxies: [n1]
rules:
  - DOMAIN-SUFFIX,example.com,Manual
`
	rep, err := ImportClash(db, yaml)
	if err != nil {
		t.Fatalf("ImportClash: %v", err)
	}
	if rep.Groups != 1 {
		t.Fatalf("代理组数 = %d, 期望 1", rep.Groups)
	}
	groups, _ := db.ListProxyGroups()
	names := map[string]int{}
	for _, g := range groups {
		names[g.Name]++
	}
	if names["Manual"] != 1 {
		t.Errorf("组名冲突处理不符: %v", names)
	}
	found := false
	for _, g := range groups {
		if strings.HasPrefix(g.Name, "Manual ") {
			found = true
		}
	}
	if !found {
		t.Error("重名组应追加序号后缀")
	}
	routings, err := db.ListRoutingGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(routings) != 1 || routings[0].Target != "Manual 2" {
		t.Fatalf("组名冲突后路由目标未映射到新组名: %+v", routings)
	}
}

const singboxSample = `{
  "outbounds": [
    {"type": "shadowsocks", "tag": "SS-港", "server": "hk.example.com", "server_port": 8388,
     "method": "aes-128-gcm", "password": "pw"},
    {"type": "vless", "tag": "VLESS-美", "server": "us.example.com", "server_port": 443,
     "uuid": "11111111-2222-3333-4444-555555555555", "flow": "xtls-rprx-vision",
     "tls": {"enabled": true, "server_name": "us.example.com", "utls": {"enabled": true, "fingerprint": "chrome"},
             "reality": {"enabled": true, "public_key": "pbk", "short_id": "sid"}}},
    {"type": "selector", "tag": "Proxy", "outbounds": ["Auto", "SS-港", "direct"], "default": "SS-港"},
    {"type": "urltest", "tag": "Auto", "outbounds": ["SS-港", "VLESS-美"],
     "url": "http://www.gstatic.com/generate_204", "interval": "5m"},
    {"type": "direct", "tag": "direct"},
    {"type": "block", "tag": "block"}
  ],
  "route": {
    "rule_set": [
      {"type": "remote", "tag": "geosite-cn", "format": "srs", "url": "https://example.com/cn.srs"},
      {"type": "local", "tag": "local-rules", "format": "source", "path": "/x.json"}
    ],
    "rules": [
      {"action": "sniff"},
      {"domain_suffix": ["cn"], "outbound": "direct"},
      {"rule_set": ["geosite-cn"], "ip_cidr": ["10.0.0.0/8"], "invert": true, "outbound": "block"},
      {"domain_suffix": ["openai.com"], "port": 443, "outbound": "Proxy"},
      {"domain": ["example.org"], "outbound": "SS-港"},
      {"outbound": "missing-tag"}
    ],
    "final": "Proxy"
  }
}
`

func TestImportSingBox(t *testing.T) {
	db := setupDB(t)

	rep, err := ImportSingBox(db, singboxSample)
	if err != nil {
		t.Fatalf("ImportSingBox: %v", err)
	}
	if rep.Nodes != 2 {
		t.Errorf("节点数 = %d, 期望 2: %+v", rep.Nodes, rep)
	}
	if rep.Groups != 2 {
		t.Errorf("代理组数 = %d, 期望 2", rep.Groups)
	}
	if rep.RuleSets != 1 {
		t.Errorf("规则集数 = %d, 期望 1（local 跳过）", rep.RuleSets)
	}
	if rep.Routings != 4 {
		t.Errorf("分流组数 = %d, 期望 4（DIRECT/BLOCK/节点目标 + final）: %+v", rep.Routings, rep.Skipped)
	}

	// 节点字段映射
	nodes, _ := db.ListNodes(0)
	byName := map[string]*config.Node{}
	for _, n := range nodes {
		byName[n.Name] = n
	}
	vless := byName["VLESS-美"]
	if vless == nil || !vless.TLS || vless.Metadata["flow"] != "xtls-rprx-vision" ||
		vless.Metadata["reality_public_key"] != "pbk" || vless.Metadata["fingerprint"] != "chrome" {
		t.Errorf("VLESS 节点映射不符: %+v", vless)
	}

	// 组：default 持久化、DIRECT 成员跳过、urltest interval
	groups, _ := db.ListProxyGroups()
	byGroupName := map[string]*config.ProxyGroup{}
	for _, g := range groups {
		byGroupName[g.Name] = g
	}
	proxy := byGroupName["Proxy"]
	if proxy == nil || len(proxy.Members) != 2 || !strings.HasPrefix(proxy.Selected, "node:") {
		t.Errorf("Proxy 组不符: %+v", proxy)
	}
	auto := byGroupName["Auto"]
	if auto == nil || auto.IntervalS != 300 {
		t.Errorf("Auto 组 interval 应为 300s: %+v", auto)
	}

	// 分流：含 port 的规则跳过（语义不可保），多字段规则→逻辑规则，missing 目标跳过
	routings, _ := db.ListRoutingGroups()
	var blockRG, nodeRG *config.RoutingGroup
	for _, g := range routings {
		switch g.Target {
		case "BLOCK":
			blockRG = g
		case "SS-港":
			nodeRG = g
		}
	}
	if blockRG == nil || len(blockRG.Rules) != 1 || blockRG.Rules[0].Type != "logical" {
		t.Fatalf("BLOCK 分流组应为逻辑规则: %+v", blockRG)
	}
	lr := blockRG.Rules[0]
	if lr.Mode != "and" || !lr.Invert || len(lr.Conditions) != 2 {
		t.Errorf("逻辑规则不符: %+v", lr)
	}
	if nodeRG == nil || len(nodeRG.Rules) != 1 || nodeRG.Rules[0].Type != "domain" {
		t.Errorf("节点目标分流组不符: %+v", nodeRG)
	}

	// 自动为节点目标创建的 select 组存在
	if byGroupName["SS-港"] == nil {
		t.Errorf("节点目标应自动创建 select 组: %v", groupNames(groups))
	}

	// 跳过清单：sniff / port / missing-tag / local 规则集
	joined := strings.Join(rep.Skipped, "\n")
	for _, want := range []string{"sniff", "port", "missing-tag", "local-rules"} {
		if !strings.Contains(joined, want) {
			t.Errorf("跳过清单缺 %q: %v", want, rep.Skipped)
		}
	}

	// 导入状态可生成配置（含逻辑规则与规则集引用）
	snap, err := buildSnapshot(db)
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	if _, err := config.Generate(snap); err != nil {
		t.Errorf("导入状态生成配置失败: %v", err)
	}
}

func TestImportSingBoxPreservesNonConsecutiveRuleOrder(t *testing.T) {
	db := setupDB(t)
	content := `{
  "outbounds": [
    {"type":"direct","tag":"direct"},
    {"type":"block","tag":"block"}
  ],
  "route": {
    "rules": [
      {"domain":["first.example"],"outbound":"direct"},
      {"domain":["second.example"],"outbound":"block"},
      {"domain":["third.example"],"outbound":"direct"}
    ]
  }
}`

	rep, err := ImportSingBox(db, content)
	if err != nil {
		t.Fatalf("ImportSingBox: %v", err)
	}
	if rep.Routings != 3 {
		t.Fatalf("应为 3 个连续规则段，得到 %d", rep.Routings)
	}
	routings, err := db.ListRoutingGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(routings) != 3 {
		t.Fatalf("分流组数量 = %d, 期望 3", len(routings))
	}
	wantTargets := []string{"DIRECT", "BLOCK", "DIRECT"}
	wantValues := []string{"first.example", "second.example", "third.example"}
	for i, rg := range routings {
		if rg.Target != wantTargets[i] || len(rg.Rules) != 1 || rg.Rules[0].Value != wantValues[i] {
			t.Errorf("分流组 %d = target=%q rules=%+v, 期望 target=%q value=%q", i, rg.Target, rg.Rules, wantTargets[i], wantValues[i])
		}
	}
}

func TestImportSingBoxReusesExistingRuleSet(t *testing.T) {
	db := setupDB(t)
	existing := &config.RuleSet{
		Name: "已有规则集", Tag: "existing", SourceType: "remote", Format: "srs",
		URL: "https://example.com/existing.srs", Enabled: true,
	}
	if err := db.CreateRuleSet(existing); err != nil {
		t.Fatal(err)
	}
	content := `{
  "outbounds": [{"type":"direct","tag":"direct"}],
  "route": {
    "rule_set": [{"type":"remote","tag":"existing","format":"srs","url":"https://other.example/existing.srs"}],
    "rules": [{"rule_set":["existing"],"outbound":"direct"}]
  }
}`
	rep, err := ImportSingBox(db, content)
	if err != nil {
		t.Fatalf("ImportSingBox: %v", err)
	}
	if rep.RuleSets != 0 {
		t.Fatalf("已有规则集不应重复导入，RuleSets=%d", rep.RuleSets)
	}
	routings, err := db.ListRoutingGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(routings) != 1 || len(routings[0].Rules) != 1 || routings[0].Rules[0].Value != "existing" {
		t.Fatalf("已有规则集引用不应丢失: %+v", routings)
	}
}

func TestImportSingBoxServerPorts(t *testing.T) {
	db := setupDB(t)
	content := `{"outbounds":[{"type":"hysteria2","tag":"hy-range","server":"hy.example.com","server_ports":["20000:50000"],"password":"p"}]}`
	rep, err := ImportSingBox(db, content)
	if err != nil {
		t.Fatalf("ImportSingBox: %v", err)
	}
	if rep.Nodes != 1 {
		t.Fatalf("节点数 = %d, 期望 1: %+v", rep.Nodes, rep)
	}
	nodes, err := db.ListNodes(0)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("读取节点失败: %v, %+v", err, nodes)
	}
	if nodes[0].Port != 20000 || fmt.Sprint(nodes[0].Metadata["server_ports"]) != "[20000:50000]" {
		t.Fatalf("server_ports 节点映射不符: %+v", nodes[0])
	}
}

func TestImportSingBoxHeadlessProtocols(t *testing.T) {
	db := setupDB(t)
	content := `{"outbounds":[
		{"type":"socks","tag":"socks","server":"127.0.0.1","server_port":1080},
		{"type":"http","tag":"http","server":"127.0.0.1","server_port":8080},
		{"type":"ssh","tag":"ssh","server":"host","server_port":22,"user":"root"},
		{"type":"anytls","tag":"anytls","server":"host","server_port":443,"password":"p"},
		{"type":"shadowtls","tag":"shadowtls","server":"host","server_port":443,"password":"p"},
		{"type":"naive","tag":"naive","server":"host","server_port":443,"username":"u","password":"p"},
		{"type":"tor","tag":"tor"}
	]}`
	rep, err := ImportSingBox(db, content)
	if err != nil {
		t.Fatalf("ImportSingBox: %v", err)
	}
	if rep.Nodes != 7 {
		t.Fatalf("新协议节点数 = %d，期望 7；跳过: %v", rep.Nodes, rep.Skipped)
	}
	nodes, err := db.ListNodes(0)
	if err != nil || len(nodes) != 7 {
		t.Fatalf("导入节点数 = %d，err=%v", len(nodes), err)
	}
}

func groupNames(groups []*config.ProxyGroup) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.Name)
	}
	return out
}

// buildSnapshot 从数据库收集生成快照（与 application.buildSnapshot 一致的最小集）。
func buildSnapshot(db *storage.DB) (config.Snapshot, error) {
	snap := config.Snapshot{Settings: config.DefaultSettings()}
	var err error
	if snap.Nodes, err = db.ListNodes(0); err != nil {
		return snap, err
	}
	if snap.ProxyGroups, err = db.ListProxyGroups(); err != nil {
		return snap, err
	}
	if snap.RoutingGroups, err = db.ListRoutingGroups(); err != nil {
		return snap, err
	}
	if snap.RuleSets, err = db.ListRuleSets(); err != nil {
		return snap, err
	}
	return snap, nil
}

// TestImportSingBoxDomainRegex domain_regex 单值原样导入；多值合并为等价的 or 单条正则
// （内部模型的 domain_regex 是单条正则，值不按逗号拆分）。
func TestImportSingBoxDomainRegex(t *testing.T) {
	db := setupDB(t)

	sample := `{
	  "outbounds": [{"type": "direct", "tag": "direct"}],
	  "route": {
	    "rules": [
	      {"domain_regex": ["^ads\\.[a-z]{2,4}\\.com$"], "action": "reject"},
	      {"domain_regex": ["^a\\.com$", "^b\\.com$"], "outbound": "direct"}
	    ]
	  }
	}`
	rep, err := ImportSingBox(db, sample)
	if err != nil {
		t.Fatalf("ImportSingBox: %v\n跳过: %v", err, rep.Skipped)
	}

	routings, err := db.ListRoutingGroups()
	if err != nil {
		t.Fatal(err)
	}
	var block, direct *config.RoutingGroup
	for _, g := range routings {
		switch g.Target {
		case "BLOCK":
			block = g
		case "DIRECT":
			direct = g
		}
	}
	if block == nil || len(block.Rules) != 1 {
		t.Fatalf("BLOCK 分流组不符: %+v（跳过: %v）", block, rep.Skipped)
	}
	// 含逗号的量词 {2,4} 必须原样保留
	if block.Rules[0].Type != "domain_regex" || block.Rules[0].Value != `^ads\.[a-z]{2,4}\.com$` {
		t.Errorf("domain_regex 单值导入不符: %+v", block.Rules[0])
	}
	if direct == nil || len(direct.Rules) != 1 {
		t.Fatalf("DIRECT 分流组不符: %+v", direct)
	}
	if want := `(?:^a\.com$)|(?:^b\.com$)`; direct.Rules[0].Value != want {
		t.Errorf("多值 domain_regex 应合并为 %q, 得到 %q", want, direct.Rules[0].Value)
	}
}

func TestImportClashDomainRegex(t *testing.T) {
	db := setupDB(t)

	// Clash 规则是 TYPE,VALUE,TARGET 的文本格式，正则内不能含逗号（格式固有限制）
	sample := `
proxies:
  - name: ss-a
    type: ss
    server: 1.2.3.4
    port: 8388
    cipher: aes-256-gcm
    password: pw
rules:
  - DOMAIN-REGEX,^ads\..*\.com$,DIRECT
  - MATCH,DIRECT
`
	rep, err := ImportClash(db, sample)
	if err != nil {
		t.Fatalf("ImportClash: %v\n跳过: %v", err, rep.Skipped)
	}
	routings, err := db.ListRoutingGroups()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range routings {
		for _, r := range g.Rules {
			if r.Type == "domain_regex" && r.Value == `^ads\..*\.com$` {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("DOMAIN-REGEX 未导入为 domain_regex 规则: %+v（跳过: %v）", routings, rep.Skipped)
	}
}
