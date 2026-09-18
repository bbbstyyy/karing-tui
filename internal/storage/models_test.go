package storage

import (
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

func TestSubscriptionCRUD(t *testing.T) {
	db := newTestDB(t)

	s := &config.Subscription{Name: "机场 A", URL: "https://example.com/sub", DownloadStrategy: config.DownloadOnlyDirect}
	if err := db.CreateSubscription(s); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	if s.ID == 0 || !s.Enabled {
		t.Fatalf("新建订阅状态不符: %+v", s)
	}

	got, err := db.GetSubscription(s.ID)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if got.Name != "机场 A" || got.URL != s.URL || got.DownloadStrategy != config.DownloadOnlyDirect {
		t.Errorf("回读不符: %+v", got)
	}

	got.Name = "机场 A 改"
	got.Enabled = false
	if err := db.UpdateSubscription(got); err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}
	reloaded, _ := db.GetSubscription(s.ID)
	if reloaded.Name != "机场 A 改" || reloaded.Enabled {
		t.Errorf("更新未生效: %+v", reloaded)
	}

	// 状态更新
	if err := db.UpdateSubscriptionState(s.ID, time.Now(), 42); err != nil {
		t.Fatalf("UpdateSubscriptionState: %v", err)
	}
	reloaded, _ = db.GetSubscription(s.ID)
	if reloaded.NodeCount != 42 || reloaded.LastUpdated.IsZero() {
		t.Errorf("状态更新未生效: %+v", reloaded)
	}

	list, err := db.ListSubscriptions()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSubscriptions = %v, %v", list, err)
	}

	if err := db.DeleteSubscription(s.ID); err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}
	if _, err := db.GetSubscription(s.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后应返回 ErrNotFound, 得到 %v", err)
	}
}

func TestReplaceSubscriptionNodesRollback(t *testing.T) {
	db := newTestDB(t)
	s := &config.Subscription{Name: "事务订阅", URL: "https://example.com"}
	if err := db.CreateSubscription(s); err != nil {
		t.Fatal(err)
	}
	old := &config.Node{Name: "旧节点", Protocol: "shadowsocks", Server: "1.2.3.4", Port: 8388, Enabled: true,
		Metadata: map[string]any{"method": "aes-128-gcm", "password": "pw"}, SubscriptionID: s.ID}
	if err := db.CreateNode(old); err != nil {
		t.Fatal(err)
	}
	bad := &config.Node{Name: "坏节点", Protocol: "shadowsocks", Server: "5.6.7.8", Port: 8388,
		Metadata: map[string]any{"unsupported": func() {}}}
	if err := db.ReplaceSubscriptionNodes(s.ID, []*config.Node{bad}, time.Now()); err == nil {
		t.Fatal("不可序列化节点应导致事务失败")
	}
	nodes, err := db.ListNodes(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Name != "旧节点" {
		t.Errorf("事务失败后旧节点应保留，得到 %+v", nodes)
	}
}

func TestReplaceSubscriptionNodesPreservesIDsForGroupMembers(t *testing.T) {
	db := newTestDB(t)
	s := &config.Subscription{Name: "稳定 ID", URL: "https://example.com"}
	if err := db.CreateSubscription(s); err != nil {
		t.Fatal(err)
	}
	n := &config.Node{Name: "节点", Protocol: "shadowsocks", Server: "1.2.3.4", Port: 8388, Enabled: true,
		Metadata: map[string]any{"method": "aes-128-gcm", "password": "pw"}, SubscriptionID: s.ID}
	if err := db.CreateNode(n); err != nil {
		t.Fatal(err)
	}
	g := &config.ProxyGroup{Name: "显式组", Type: "select", Members: []config.ProxyGroupMember{{Type: "node", ID: n.ID}}, Selected: "node:" + fmt.Sprint(n.ID)}
	if err := db.CreateProxyGroup(g); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProxyGroupSelected(g.ID, g.Selected); err != nil {
		t.Fatal(err)
	}
	updated := &config.Node{ID: n.ID, Name: n.Name, Protocol: n.Protocol, Server: n.Server, Port: n.Port, Enabled: true,
		Metadata: map[string]any{"method": "aes-128-gcm", "password": "new"}, SubscriptionID: s.ID}
	if err := db.ReplaceSubscriptionNodes(s.ID, []*config.Node{updated}, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetProxyGroup(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 1 || got.Members[0].ID != n.ID || got.Selected != "node:"+fmt.Sprint(n.ID) {
		t.Fatalf("节点 ID/组引用未保留: %+v", got)
	}
}

// V7-1：携带重复非零 ID 的输入必须被拒绝，而不是让第二个节点静默走 INSERT。
//
// 本测试的前身是 TestReplaceSubscriptionNodesDoesNotReuseDuplicateIDs，它断言的
// 正是「第二个节点落成新行」。那其实是 API 层身份算法出错时唯一的可观测信号，
// v7 轮起改为 fail-closed（见 ReplaceSubscriptionNodes 的入参约束）；下方 usedIDs
// 二次防线保留，用于兜住其它调用路径。
func TestReplaceSubscriptionNodesRejectsDuplicateIDs(t *testing.T) {
	db := newTestDB(t)
	s := &config.Subscription{Name: "重复节点", URL: "https://example.com"}
	if err := db.CreateSubscription(s); err != nil {
		t.Fatal(err)
	}
	old := &config.Node{Name: "same", Protocol: "trojan", Server: "example.com", Port: 443, Enabled: true,
		Metadata: map[string]any{"password": "old"}, SubscriptionID: s.ID}
	if err := db.CreateNode(old); err != nil {
		t.Fatal(err)
	}
	before, err := db.GetSubscription(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	updated := &config.Node{ID: old.ID, Name: old.Name, Protocol: old.Protocol, Server: old.Server, Port: old.Port,
		Enabled: true, Metadata: map[string]any{"password": "one"}, SubscriptionID: s.ID}
	duplicate := &config.Node{ID: old.ID, Name: old.Name, Protocol: old.Protocol, Server: old.Server, Port: old.Port,
		Enabled: true, Metadata: map[string]any{"password": "two"}, SubscriptionID: s.ID}
	if err := db.ReplaceSubscriptionNodes(s.ID, []*config.Node{updated, duplicate}, time.Now()); err == nil {
		t.Fatal("重复非零 ID 必须被拒绝")
	}
	// 拒绝必须发生在任何破坏性动作之前：旧节点内容与订阅状态都不能变。
	nodes, err := db.ListNodes(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].ID != old.ID {
		t.Fatalf("拒绝后应保留唯一的旧节点，得到 %+v", nodes)
	}
	if got, _ := nodes[0].Metadata["password"].(string); got != "old" {
		t.Fatalf("拒绝后旧节点内容应原样保留，密码实得 %q", got)
	}
	after, err := db.GetSubscription(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.NodeCount != before.NodeCount || !after.LastUpdated.Equal(before.LastUpdated) {
		t.Fatalf("拒绝后订阅状态不应变化: %+v -> %+v", before, after)
	}
}

func TestListNodesUsesSubscriptionLatencySort(t *testing.T) {
	db := newTestDB(t)
	s := &config.Subscription{Name: "测速排序", URL: "https://example.com", SortBy: "latency"}
	if err := db.CreateSubscription(s); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*config.Node{
		{Name: "慢", Protocol: "trojan", Server: "slow", Port: 443, Enabled: true, Metadata: map[string]any{"password": "p"}, SubscriptionID: s.ID},
		{Name: "快", Protocol: "trojan", Server: "fast", Port: 443, Enabled: true, Metadata: map[string]any{"password": "p"}, SubscriptionID: s.ID},
		{Name: "未测", Protocol: "trojan", Server: "new", Port: 443, Enabled: true, Metadata: map[string]any{"password": "p"}, SubscriptionID: s.ID},
	} {
		if err := db.CreateNode(n); err != nil {
			t.Fatal(err)
		}
		switch n.Name {
		case "慢":
			_ = db.UpdateNodeLatency(n.ID, 300, time.Now())
		case "快":
			_ = db.UpdateNodeLatency(n.ID, 50, time.Now())
		}
	}
	nodes, err := db.ListNodes(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 || nodes[0].Name != "快" || nodes[1].Name != "慢" || nodes[2].Name != "未测" {
		t.Fatalf("延迟排序未生效: %+v", nodes)
	}
}

func TestNodeCRUDAndCascade(t *testing.T) {
	db := newTestDB(t)

	s := &config.Subscription{Name: "sub", URL: "https://example.com"}
	if err := db.CreateSubscription(s); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}

	n := &config.Node{
		SubscriptionID: s.ID,
		Name:           "HK-01",
		Protocol:       "vless",
		Server:         "hk.example.com",
		Port:           443,
		TLS:            true,
		Transport:      "ws",
		Enabled:        true,
		Metadata:       map[string]any{"uuid": "uuid-1", "path": "/ws"},
	}
	if err := db.CreateNode(n); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	got, err := db.GetNode(n.ID)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.Protocol != "vless" || got.Metadata["uuid"] != "uuid-1" || !got.TLS || got.Transport != "ws" {
		t.Errorf("节点回读不符: %+v", got)
	}
	if got.LatencyMS != -1 {
		t.Errorf("未测速节点延迟应为 -1, 得到 %d", got.LatencyMS)
	}

	// 手动节点 subscription_id 为 NULL
	manual := &config.Node{Name: "manual", Protocol: "shadowsocks", Server: "1.2.3.4", Port: 8388, Enabled: true}
	if err := db.CreateNode(manual); err != nil {
		t.Fatalf("CreateNode manual: %v", err)
	}
	if manual.SubscriptionID != 0 {
		t.Errorf("手动节点 subscription_id 应为 0, 得到 %d", manual.SubscriptionID)
	}

	// 测速结果
	if err := db.UpdateNodeLatency(n.ID, 123, time.Now()); err != nil {
		t.Fatalf("UpdateNodeLatency: %v", err)
	}
	got, _ = db.GetNode(n.ID)
	if got.LatencyMS != 123 || got.LastTested.IsZero() {
		t.Errorf("测速结果未生效: %+v", got)
	}

	// 按订阅过滤
	nodes, err := db.ListNodes(s.ID)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("ListNodes(sub) = %d nodes, %v", len(nodes), err)
	}
	all, _ := db.ListNodes(0)
	if len(all) != 2 {
		t.Fatalf("ListNodes(all) = %d nodes", len(all))
	}

	// 级联删除：删订阅应连带删节点
	if err := db.DeleteSubscription(s.ID); err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}
	if _, err := db.GetNode(n.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("订阅删除后节点应级联删除, 得到 %v", err)
	}
	if _, err := db.GetNode(manual.ID); err != nil {
		t.Errorf("手动节点不应被级联删除: %v", err)
	}
}

func TestDeleteNodeAndGroupCleanPolymorphicReferences(t *testing.T) {
	db := newTestDB(t)
	n := &config.Node{Name: "引用节点", Protocol: "trojan", Server: "example.com", Port: 443, Enabled: true,
		Metadata: map[string]any{"password": "pw"}}
	if err := db.CreateNode(n); err != nil {
		t.Fatal(err)
	}
	child := &config.ProxyGroup{Name: "子组", Type: "select"}
	if err := db.CreateProxyGroup(child); err != nil {
		t.Fatal(err)
	}
	parent := &config.ProxyGroup{Name: "父组", Type: "select", Members: []config.ProxyGroupMember{
		{Type: "node", ID: n.ID}, {Type: "group", ID: child.ID},
	}}
	if err := db.CreateProxyGroup(parent); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProxyGroupSelected(parent.ID, "node:"+strconv.FormatInt(n.ID, 10)); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteNode(n.ID); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetProxyGroup(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 1 || got.Members[0].Type != "group" || got.Selected != "" {
		t.Fatalf("删除节点后引用未清理: %+v", got)
	}
	if err := db.DeleteProxyGroup(child.ID); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetProxyGroup(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 0 {
		t.Fatalf("删除嵌套组后引用未清理: %+v", got.Members)
	}
}

func TestProxyGroupAndRoutingCRUD(t *testing.T) {
	db := newTestDB(t)

	n1 := &config.Node{Name: "n1", Protocol: "shadowsocks", Server: "a", Port: 1, Enabled: true}
	n2 := &config.Node{Name: "n2", Protocol: "vmess", Server: "b", Port: 2, Enabled: true}
	db.CreateNode(n1)
	db.CreateNode(n2)

	g := &config.ProxyGroup{
		Name:    "Auto",
		Type:    "urltest",
		TestURL: "https://www.gstatic.com/generate_204",
		Members: []config.ProxyGroupMember{
			{Type: "node", ID: n1.ID},
			{Type: "node", ID: n2.ID},
		},
	}
	if err := db.CreateProxyGroup(g); err != nil {
		t.Fatalf("CreateProxyGroup: %v", err)
	}

	// 嵌套组
	manual := &config.ProxyGroup{Name: "Manual", Type: "select"}
	db.CreateProxyGroup(manual)
	auto, _ := db.GetProxyGroup(g.ID)
	auto.Members = append(auto.Members, config.ProxyGroupMember{Type: "group", ID: manual.ID})
	if err := db.UpdateProxyGroup(auto); err != nil {
		t.Fatalf("UpdateProxyGroup: %v", err)
	}

	reloaded, err := db.GetProxyGroup(g.ID)
	if err != nil {
		t.Fatalf("GetProxyGroup: %v", err)
	}
	if len(reloaded.Members) != 3 ||
		reloaded.Members[0].Type != "node" || reloaded.Members[0].ID != n1.ID ||
		reloaded.Members[2].Type != "group" || reloaded.Members[2].ID != manual.ID {
		t.Errorf("组成员回读不符: %+v", reloaded.Members)
	}

	// 分流组 + 规则
	rg := &config.RoutingGroup{
		Name:    "中国大陆",
		Target:  "DIRECT",
		Enabled: true,
		Rules: []config.Rule{
			{Type: "domain_suffix", Value: "cn", Enabled: true},
			{Type: "geoip", Value: "cn", Enabled: true},
			{Type: "final", Value: "", Enabled: true},
		},
	}
	if err := db.CreateRoutingGroup(rg); err != nil {
		t.Fatalf("CreateRoutingGroup: %v", err)
	}
	rgs, err := db.ListRoutingGroups()
	if err != nil || len(rgs) != 1 {
		t.Fatalf("ListRoutingGroups = %d, %v", len(rgs), err)
	}
	if len(rgs[0].Rules) != 3 || rgs[0].Rules[1].Type != "geoip" {
		t.Errorf("规则回读不符: %+v", rgs[0].Rules)
	}
	// position 应按切片顺序写入
	if rgs[0].Rules[2].Position != 2 {
		t.Errorf("规则 position = %d, 期望 2", rgs[0].Rules[2].Position)
	}

	// 删除分流组级联删规则
	if err := db.DeleteRoutingGroup(rg.ID); err != nil {
		t.Fatalf("DeleteRoutingGroup: %v", err)
	}
	var count int
	db.db.QueryRow(`SELECT COUNT(*) FROM rules`).Scan(&count)
	if count != 0 {
		t.Errorf("分流组删除后规则应级联删除, 剩 %d", count)
	}
}

func TestLogicalRulePersist(t *testing.T) {
	db := newTestDB(t)

	rg := &config.RoutingGroup{
		Name: "组合规则", Target: "Auto", Enabled: true,
		Rules: []config.Rule{
			{
				Type: "logical", Mode: "and", Invert: true, Enabled: true,
				Conditions: []config.RuleCondition{
					{Type: "rule_set", Value: "geosite-cn"},
					{Type: "ip_cidr", Value: "10.0.0.0/8", Invert: true},
				},
			},
			{Type: "domain_suffix", Value: "cn", Enabled: true},
		},
	}
	if err := db.CreateRoutingGroup(rg); err != nil {
		t.Fatalf("CreateRoutingGroup: %v", err)
	}

	rgs, err := db.ListRoutingGroups()
	if err != nil || len(rgs) != 1 {
		t.Fatalf("ListRoutingGroups = %d, %v", len(rgs), err)
	}
	got := rgs[0].Rules[0]
	if got.Type != "logical" || got.Mode != "and" || !got.Invert {
		t.Errorf("逻辑规则字段回读不符: %+v", got)
	}
	if len(got.Conditions) != 2 {
		t.Fatalf("子条件数 = %d, 期望 2", len(got.Conditions))
	}
	if got.Conditions[0].Type != "rule_set" || got.Conditions[0].Value != "geosite-cn" || got.Conditions[0].Invert {
		t.Errorf("子条件 0 回读不符: %+v", got.Conditions[0])
	}
	if got.Conditions[1].Type != "ip_cidr" || !got.Conditions[1].Invert {
		t.Errorf("子条件 1 回读不符: %+v", got.Conditions[1])
	}
	// 普通规则不受影响
	if rgs[0].Rules[1].Type != "domain_suffix" || rgs[0].Rules[1].Conditions != nil {
		t.Errorf("普通规则回读不符: %+v", rgs[0].Rules[1])
	}

	// 更新：改为 or 组合、删一个条件
	got.Mode = "or"
	got.Conditions = got.Conditions[:1]
	rg.Rules = []config.Rule{got}
	if err := db.UpdateRoutingGroup(&config.RoutingGroup{
		ID: rg.ID, Name: rg.Name, Target: rg.Target, Enabled: true, Rules: rg.Rules,
	}); err != nil {
		t.Fatalf("UpdateRoutingGroup: %v", err)
	}
	rgs, _ = db.ListRoutingGroups()
	lr := rgs[0].Rules[0]
	if lr.Mode != "or" || len(lr.Conditions) != 1 {
		t.Errorf("逻辑规则更新回读不符: mode=%q, %d 条件", lr.Mode, len(lr.Conditions))
	}

	// 删除规则级联删子条件
	if err := db.DeleteRoutingGroup(rg.ID); err != nil {
		t.Fatalf("DeleteRoutingGroup: %v", err)
	}
	var count int
	db.db.QueryRow(`SELECT COUNT(*) FROM rule_conditions`).Scan(&count)
	if count != 0 {
		t.Errorf("规则删除后子条件应级联删除, 剩 %d", count)
	}
}

func TestRuleSetAndDNSServerCRUD(t *testing.T) {
	db := newTestDB(t)

	rs := &config.RuleSet{
		Name:       "China",
		Tag:        "geosite-cn",
		SourceType: "remote",
		Format:     "srs",
		URL:        "https://example.com/geosite-cn.srs",
		Enabled:    true,
	}
	if err := db.CreateRuleSet(rs); err != nil {
		t.Fatalf("CreateRuleSet: %v", err)
	}
	list, err := db.ListRuleSets()
	if err != nil || len(list) != 1 || list[0].Tag != "geosite-cn" {
		t.Fatalf("ListRuleSets = %+v, %v", list, err)
	}
	rs.Enabled = false
	db.UpdateRuleSet(rs)
	list, _ = db.ListRuleSets()
	if list[0].Enabled {
		t.Error("规则集更新未生效")
	}
	db.DeleteRuleSet(rs.ID)
	list, _ = db.ListRuleSets()
	if len(list) != 0 {
		t.Error("规则集删除未生效")
	}

	srv := &config.DNSServer{Tag: "local-dns", Address: "223.5.5.5", Enabled: true}
	if err := db.CreateDNSServer(srv); err != nil {
		t.Fatalf("CreateDNSServer: %v", err)
	}
	servers, err := db.ListDNSServers()
	if err != nil || len(servers) != 1 || servers[0].Address != "223.5.5.5" {
		t.Fatalf("ListDNSServers = %+v, %v", servers, err)
	}
	srv.Address = "119.29.29.29"
	db.UpdateDNSServer(srv)
	servers, _ = db.ListDNSServers()
	if servers[0].Address != "119.29.29.29" {
		t.Error("DNS 服务器更新未生效")
	}
	db.DeleteDNSServer(srv.ID)
	servers, _ = db.ListDNSServers()
	if len(servers) != 0 {
		t.Error("DNS 服务器删除未生效")
	}
}

func TestRuleSetRejectsPathTag(t *testing.T) {
	db := newTestDB(t)
	for _, tag := range []string{"../escape", `..\\escape`, "../../runtime/config"} {
		if err := db.CreateRuleSet(&config.RuleSet{Name: tag, Tag: tag, Format: "json", URL: "https://example.com/x"}); err == nil {
			t.Errorf("路径型 Tag %q 应被拒绝", tag)
		}
	}
}

func TestSettingsModelRoundTrip(t *testing.T) {
	db := newTestDB(t)

	s, err := db.LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if s.MixedPort != 2080 {
		t.Errorf("默认 MixedPort = %d, 期望 2080", s.MixedPort)
	}
	// 空库（缺键）取默认：内网直连开、resolve 关
	if !s.PrivateDirect {
		t.Error("默认应开启内网直连")
	}
	if s.ResolveIPRules {
		t.Error("默认应关闭 IP 规则解析域名")
	}

	s.MixedPort = 7890
	s.AllowLAN = true
	s.DownloadProxy = "http://127.0.0.1:7890"
	s.LogLevel = "warn"
	s.AutoUpdateMinutes = 30
	s.PrivateDirect = false
	s.ResolveIPRules = true
	if err := db.SaveSettings(s); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	got, err := db.LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got != s {
		t.Errorf("设置回读不符: got %+v, want %+v", got, s)
	}

	// 非法值（负数）被忽略，回落默认 0
	if err := db.SetSetting("auto_update_minutes", "-5"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got, _ := db.LoadSettings(); got.AutoUpdateMinutes != 0 {
		t.Errorf("负的自动更新间隔应回落默认 0, got %d", got.AutoUpdateMinutes)
	}
	if err := db.SetSetting("auto_update_minutes", strconv.Itoa(config.MaxAutoUpdateMinutes+1)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got, _ := db.LoadSettings(); got.AutoUpdateMinutes != 0 {
		t.Errorf("溢出 time.Duration 的自动更新间隔应回落默认 0, got %d", got.AutoUpdateMinutes)
	}
}
