package subscription

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

func mustParseOne(t *testing.T, content string) *config.Node {
	t.Helper()
	nodes, err := ParseContent(content)
	if err != nil {
		t.Fatalf("ParseContent 失败: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("期望 1 个节点，得到 %d 个", len(nodes))
	}
	return nodes[0]
}

func TestParseSS_SIP002_Base64Userinfo(t *testing.T) {
	// aes-256-gcm:password123 的 base64
	userinfo := base64.URLEncoding.EncodeToString([]byte("aes-256-gcm:password123"))
	n := mustParseOne(t, "ss://"+userinfo+"@1.2.3.4:8388#测试节点")
	if n.Protocol != "shadowsocks" || n.Server != "1.2.3.4" || n.Port != 8388 {
		t.Fatalf("字段错误: %+v", n)
	}
	if n.Name != "测试节点" {
		t.Fatalf("名称错误: %q", n.Name)
	}
	if n.Metadata["method"] != "aes-256-gcm" || n.Metadata["password"] != "password123" {
		t.Fatalf("元数据错误: %v", n.Metadata)
	}
}

func TestParseSS_SIP002_PlainUserinfo(t *testing.T) {
	n := mustParseOne(t, "ss://aes-256-gcm:secret@host.example:443#s1")
	if n.Metadata["method"] != "aes-256-gcm" || n.Metadata["password"] != "secret" {
		t.Fatalf("元数据错误: %v", n.Metadata)
	}
}

func TestParseSS_LegacyBase64(t *testing.T) {
	blob := base64.StdEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:pwd@5.6.7.8:9999"))
	n := mustParseOne(t, "ss://"+blob+"#legacy")
	if n.Server != "5.6.7.8" || n.Port != 9999 {
		t.Fatalf("字段错误: %+v", n)
	}
	if n.Metadata["method"] != "chacha20-ietf-poly1305" || n.Metadata["password"] != "pwd" {
		t.Fatalf("元数据错误: %v", n.Metadata)
	}
}

func TestParseVmess(t *testing.T) {
	jsonStr := `{"v":"2","ps":"香港 01","add":"hk.example.com","port":"443","id":"b831381d-6324-4d53-ad4f-8cda48b30811","aid":"0","scy":"auto","net":"ws","host":"cdn.example.com","path":"/ws","tls":"tls","sni":"hk.example.com","fp":"chrome"}`
	b64 := base64.StdEncoding.EncodeToString([]byte(jsonStr))
	n := mustParseOne(t, "vmess://"+b64)
	if n.Protocol != "vmess" || n.Name != "香港 01" || n.Server != "hk.example.com" || n.Port != 443 {
		t.Fatalf("字段错误: %+v", n)
	}
	if !n.TLS || n.Transport != "ws" {
		t.Fatalf("TLS/Transport 错误: %v %q", n.TLS, n.Transport)
	}
	if n.Metadata["uuid"] != "b831381d-6324-4d53-ad4f-8cda48b30811" ||
		n.Metadata["host"] != "cdn.example.com" || n.Metadata["path"] != "/ws" ||
		n.Metadata["sni"] != "hk.example.com" || n.Metadata["fingerprint"] != "chrome" {
		t.Fatalf("元数据错误: %v", n.Metadata)
	}
}

func TestParseVless_TCP_TLS(t *testing.T) {
	n := mustParseOne(t, "vless://b831381d-6324-4d53-ad4f-8cda48b30811@us.example.com:443?encryption=none&security=tls&sni=us.example.com&fp=chrome&type=tcp&flow=xtls-rprx-vision#美国")
	if n.Protocol != "vless" || !n.TLS || n.Transport != "" {
		t.Fatalf("字段错误: %+v", n)
	}
	if n.Metadata["flow"] != "xtls-rprx-vision" || n.Metadata["sni"] != "us.example.com" {
		t.Fatalf("元数据错误: %v", n.Metadata)
	}
}

func TestParseVless_Reality_WS(t *testing.T) {
	n := mustParseOne(t, "vless://uuid-x@1.1.1.1:443?security=reality&sni=www.apple.com&pbk=PUBKEY&sid=abcd&fp=chrome&type=ws&path=%2Fws&host=cdn.com#r1")
	if !n.TLS || n.Transport != "ws" {
		t.Fatalf("字段错误: %+v", n)
	}
	if n.Metadata["reality_public_key"] != "PUBKEY" || n.Metadata["reality_short_id"] != "abcd" {
		t.Fatalf("reality 元数据错误: %v", n.Metadata)
	}
	if n.Metadata["path"] != "/ws" || n.Metadata["host"] != "cdn.com" {
		t.Fatalf("ws 元数据错误: %v", n.Metadata)
	}
}

func TestParseTrojan(t *testing.T) {
	n := mustParseOne(t, "trojan://pass%40word@jp.example.com:443?sni=jp.example.com&type=ws&path=%2Ft#日本")
	if n.Protocol != "trojan" || !n.TLS || n.Transport != "ws" {
		t.Fatalf("字段错误: %+v", n)
	}
	if n.Metadata["password"] != "pass@word" || n.Metadata["path"] != "/t" {
		t.Fatalf("元数据错误: %v", n.Metadata)
	}
}

func TestParseHysteria2(t *testing.T) {
	n := mustParseOne(t, "hysteria2://letmein@sg.example.com:8443?sni=sg.example.com&obfs=salamander&obfs-password=ob&insecure=1#新加坡")
	if n.Protocol != "hysteria2" || !n.TLS || n.Port != 8443 {
		t.Fatalf("字段错误: %+v", n)
	}
	if n.Metadata["password"] != "letmein" || n.Metadata["obfs"] != "salamander" ||
		n.Metadata["obfs_password"] != "ob" || n.Metadata["allow_insecure"] != true {
		t.Fatalf("元数据错误: %v", n.Metadata)
	}
}

func TestParseSingBoxHysteriaV1AuthString(t *testing.T) {
	n := mustParseOne(t, `{"outbounds":[{"type":"hysteria","tag":"hy","server":"hy.example.com","server_port":443,"auth_str":"secret"}]}`)
	if n.Protocol != "hysteria" || n.Metadata["password"] != "secret" {
		t.Fatalf("Hysteria v1 auth_str 未导入为 password: %+v", n)
	}
}

func TestParseHysteria2_MultiServer(t *testing.T) {
	if _, err := ParseContent("hy2://pwd@a.com:1,b.com:2?sni=a.com#multi"); err == nil {
		t.Fatal("不支持的多端点 hysteria2 链接应明确报错")
	}
}

func TestParseMbpsUnits(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  int
	}{
		{"100", 100}, {"100 Mbps", 100}, {"0.5 Gbps", 500}, {"1g", 1000},
	} {
		got, ok := parseMbps(tc.input)
		if !ok || got != tc.want {
			t.Errorf("parseMbps(%q) = %d, %v; want %d, true", tc.input, got, ok, tc.want)
		}
	}
	if _, ok := parseMbps("0.5 Mbps"); ok {
		t.Fatal("小于 1 Mbps 的值不应生成 0 Mbps")
	}
}

func TestParseHysteria2_PortRange(t *testing.T) {
	n := mustParseOne(t, "hysteria2://pwd@a.com:20000-50000#range")
	if n.Server != "a.com" || n.Port != 20000 {
		t.Fatalf("端口范围首端口解析错误: %+v", n)
	}
	ports, ok := n.Metadata["server_ports"].([]string)
	if !ok || len(ports) != 1 || ports[0] != "20000:50000" {
		t.Fatalf("端口范围未保留: %#v", n.Metadata["server_ports"])
	}
}

func TestParseCredentialSingleFieldAndWhitespaceLinks(t *testing.T) {
	n := mustParseOne(t, "anytls://secret@host.example:443?sni=cdn.example&insecure=1#any")
	if n.Metadata["password"] != "secret" || n.Metadata["sni"] != "cdn.example" || n.Metadata["allow_insecure"] != true {
		t.Fatalf("AnyTLS 单字段凭据或 TLS 参数错误: %#v", n.Metadata)
	}
	links := "anytls://a@a.example:443 trojan://b@b.example:443#b"
	nodes, err := ParseContent(links)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("同一行空白分隔链接解析失败: %d, %v", len(nodes), err)
	}
}

func TestParseTUIC(t *testing.T) {
	n := mustParseOne(t, "tuic://uuid-1:pass-1@tk.example.com:443?congestion_control=bbr&udp_relay_mode=native&alpn=h3&sni=tk.example.com#TUIC")
	if n.Protocol != "tuic" || !n.TLS {
		t.Fatalf("字段错误: %+v", n)
	}
	if n.Metadata["uuid"] != "uuid-1" || n.Metadata["password"] != "pass-1" ||
		n.Metadata["congestion_control"] != "bbr" || n.Metadata["udp_relay_mode"] != "native" {
		t.Fatalf("元数据错误: %v", n.Metadata)
	}
}

func TestParseBase64Subscription(t *testing.T) {
	links := strings.Join([]string{
		"ss://aes-256-gcm:p1@1.1.1.1:1#a",
		"trojan://p2@2.2.2.2:443#b",
		"",
	}, "\n")
	b64 := base64.StdEncoding.EncodeToString([]byte(links))
	nodes, err := ParseContent(b64)
	if err != nil {
		t.Fatalf("ParseContent 失败: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("期望 2 个节点，得到 %d", len(nodes))
	}
	if DetectFormat(b64) != FormatBase64 {
		t.Fatalf("格式识别错误: %v", DetectFormat(b64))
	}
}

func TestParseHeadlessProtocolLinks(t *testing.T) {
	cases := []struct {
		link, protocol string
	}{
		{"socks://user:pass@127.0.0.1:1080#s", "socks"},
		{"http://user:pass@127.0.0.1:8080#h", "http"},
		{"ssh://root:pass@host:22#ssh", "ssh"},
		{"anytls://user:pass@host:443#any", "anytls"},
		{"shadowtls://user:pass@host:443?version=3#shadow", "shadowtls"},
		{"naive://user:pass@host:443#naive", "naive"},
	}
	for _, tc := range cases {
		nodes, err := ParseContent(tc.link)
		if err != nil {
			t.Errorf("ParseContent(%q): %v", tc.link, err)
			continue
		}
		if len(nodes) != 1 || nodes[0].Protocol != tc.protocol {
			t.Errorf("ParseContent(%q) = %+v, want %s", tc.link, nodes, tc.protocol)
		}
	}
}

func TestParseUserInfoPreservesMissingAndInvalidFields(t *testing.T) {
	info, ok := parseUserInfo(http.Header{"Subscription-Userinfo": []string{"upload=12;download=bad;expire=1700000000"}})
	if !ok || !info.hasUpload || info.upload != 12 || info.hasDownload || !info.hasExpire {
		t.Fatalf("解析订阅流量头部不符: %+v, ok=%v", info, ok)
	}
	if !info.expire.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("expire = %v", info.expire)
	}
	if _, ok := parseUserInfo(http.Header{"Subscription-Userinfo": []string{"upload=bad"}}); ok {
		t.Fatal("完全非法的订阅流量头部不应报告成功")
	}
}

func TestParseClash(t *testing.T) {
	yaml := `
proxies:
  - name: "ss节点"
    type: ss
    server: 1.2.3.4
    port: 8388
    cipher: aes-256-gcm
    password: "pw"
    udp: true
  - name: "vmess节点"
    type: vmess
    server: v.example.com
    port: 443
    uuid: b831381d-6324-4d53-ad4f-8cda48b30811
    alterId: 0
    cipher: auto
    tls: true
    servername: v.example.com
    network: ws
    ws-opts:
      path: /vm
      headers:
        Host: cdn.example.com
  - name: "hy2节点"
    type: hysteria2
    server: h.example.com
    port: 443
    password: hp
    obfs: salamander
    obfs-password: op
    sni: h.example.com
  - name: "不支持"
    type: snell
    server: x
    port: 1
`
	nodes, err := ParseContent(yaml)
	if err != nil {
		t.Fatalf("ParseContent 失败: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("期望 3 个节点，得到 %d", len(nodes))
	}
	ss, vm, hy2 := nodes[0], nodes[1], nodes[2]
	if ss.Protocol != "shadowsocks" || ss.Metadata["method"] != "aes-256-gcm" || ss.Metadata["udp"] != true {
		t.Fatalf("ss 错误: %+v", ss)
	}
	if vm.Transport != "ws" || !vm.TLS || vm.Metadata["host"] != "cdn.example.com" || vm.Metadata["path"] != "/vm" {
		t.Fatalf("vmess 错误: %+v", vm)
	}
	if hy2.Protocol != "hysteria2" || hy2.Metadata["obfs"] != "salamander" || hy2.Metadata["obfs_password"] != "op" {
		t.Fatalf("hy2 错误: %+v", hy2)
	}
}

func TestParseClash_VlessReality(t *testing.T) {
	yaml := `
proxies:
  - name: r
    type: vless
    server: 1.1.1.1
    port: 443
    uuid: u-1
    tls: true
    flow: xtls-rprx-vision
    servername: www.apple.com
    reality-opts:
      public-key: PK
      short-id: SI
    client-fingerprint: chrome
`
	n := mustParseOne(t, yaml)
	if !n.TLS || n.Metadata["reality_public_key"] != "PK" || n.Metadata["flow"] != "xtls-rprx-vision" {
		t.Fatalf("vless reality 错误: %+v", n)
	}
}

func TestParseSingBoxConfig(t *testing.T) {
	jsonCfg := `{
	  "outbounds": [
	    {"type": "shadowsocks", "tag": "ss1", "server": "1.2.3.4", "server_port": 8388,
	     "method": "aes-256-gcm", "password": "p"},
	    {"type": "vless", "tag": "vl1", "server": "v.com", "server_port": 443, "uuid": "u",
	     "flow": "xtls-rprx-vision",
	     "tls": {"enabled": true, "server_name": "v.com", "utls": {"enabled": true, "fingerprint": "chrome"},
	             "reality": {"enabled": true, "public_key": "PK", "short_id": "SI"}},
	     "transport": {"type": "ws", "path": "/w", "headers": {"Host": "h.com"}}},
	    {"type": "hysteria2", "tag": "hy1", "server": "h.com", "server_port": 443, "password": "p2",
	     "obfs": {"type": "salamander", "password": "op"}, "up_mbps": 100, "down_mbps": 200},
	    {"type": "direct", "tag": "direct"},
	    {"type": "selector", "tag": "auto", "outbounds": ["ss1"]}
	  ]
	}`
	nodes, err := ParseContent(jsonCfg)
	if err != nil {
		t.Fatalf("ParseContent 失败: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("期望 3 个节点，得到 %d", len(nodes))
	}
	if DetectFormat(jsonCfg) != FormatSingBox {
		t.Fatalf("格式识别错误: %v", DetectFormat(jsonCfg))
	}
	vl := nodes[1]
	if vl.Metadata["reality_public_key"] != "PK" || vl.Metadata["fingerprint"] != "chrome" ||
		vl.Metadata["host"] != "h.com" || vl.Transport != "ws" {
		t.Fatalf("vless 错误: %+v", vl)
	}
	hy := nodes[2]
	if hy.Metadata["obfs"] != "salamander" || hy.Metadata["up_mbps"] != 100 || hy.Metadata["down_mbps"] != 200 {
		t.Fatalf("hysteria2 错误: %+v", hy)
	}
}

func TestParseSingBoxServerPorts(t *testing.T) {
	jsonCfg := `{"outbounds":[{"type":"hysteria2","tag":"hy-range","server":"hy.example.com","server_ports":["20000:50000"],"password":"p"}]}`
	nodes, err := ParseContent(jsonCfg)
	if err != nil {
		t.Fatalf("ParseContent 失败: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("期望 1 个节点，得到 %d", len(nodes))
	}
	n := nodes[0]
	if n.Port != 20000 {
		t.Fatalf("范围端口起始值错误: %+v", n)
	}
	ports, ok := n.Metadata["server_ports"].([]string)
	if !ok || len(ports) != 1 || ports[0] != "20000:50000" {
		t.Fatalf("server_ports 未保留: %#v", n.Metadata["server_ports"])
	}
}

func TestDetectFormat_Unknown(t *testing.T) {
	for _, s := range []string{"", "hello world", "<html>404</html>", "random text"} {
		if f := DetectFormat(s); f != FormatUnknown {
			t.Errorf("%q 应识别为 unknown，得到 %v", s, f)
		}
	}
}

func TestDetectFormat_SingBoxWithURLBeforeOutbounds(t *testing.T) {
	content := `{
		"dns": {"servers": [{"server": "https://dns.example/dns-query"}]},
		"outbounds": [
			{"type": "trojan", "tag": "node", "server": "example.com", "server_port": 443, "password": "secret"}
		]
	}`
	if f := DetectFormat(content); f != FormatSingBox {
		t.Fatalf("含 URL 的 sing-box JSON 应识别为 sing-box，得到 %v", f)
	}
	nodes, err := ParseContent(content)
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Protocol != "trojan" {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}
}

func TestParseLinks_SkipBadLines(t *testing.T) {
	content := strings.Join([]string{
		"# 注释",
		"ss://aes-256-gcm:p@1.1.1.1:1#ok",
		"https://example.com/not-a-node",
		"",
	}, "\n")
	nodes, err := ParseContent(content)
	if err != nil {
		t.Fatalf("ParseContent 失败: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("期望 1 个节点，得到 %d", len(nodes))
	}
}

func TestApplyNodePolicy(t *testing.T) {
	nodes := []*config.Node{{Name: "US Fast", Server: "us.example", Protocol: "trojan", LatencyMS: 80}, {Name: "CN", Server: "cn.example", Protocol: "ss", LatencyMS: 20}, {Name: "US Slow", Server: "us2.example", Protocol: "ss", LatencyMS: 200}}
	got, err := applyNodePolicy(nodes, "us,!slow", "latency")
	if err != nil || len(got) != 1 || got[0].Name != "US Fast" {
		t.Fatalf("policy result: %+v, %v", got, err)
	}
	if _, err := applyNodePolicy(nodes, "[", "name"); err == nil {
		t.Fatal("invalid filter should fail")
	}
}

func TestParseUserInfo(t *testing.T) {
	h := http.Header{}
	h.Set("subscription-userinfo", "upload=10; download=20; total=100; expire=1700000000")
	info, ok := parseUserInfo(h)
	if !ok || info.upload != 10 || info.download != 20 || info.total != 100 || info.expire.Unix() != 1700000000 {
		t.Fatalf("unexpected userinfo: %+v %v", info, ok)
	}
}

func TestParseSSR(t *testing.T) {
	// 程序化构造 ssr:// 链接：host:port:protocol:method:obfs:b64(password)/?obfsparam=..&remarks=..
	main := "1.2.3.4:443:origin:aes-256-cfb:plain:" + base64.StdEncoding.EncodeToString([]byte("secret"))
	params := "/?obfsparam=" + base64.URLEncoding.EncodeToString([]byte("obfs-param")) +
		"&remarks=" + base64.URLEncoding.EncodeToString([]byte("SSR 节点"))
	link := "ssr://" + base64.URLEncoding.EncodeToString([]byte(main+params))

	n := mustParseOne(t, link)
	if n.Protocol != "ssr" || n.Server != "1.2.3.4" || n.Port != 443 {
		t.Errorf("SSR 基础字段不符: %+v", n)
	}
	if n.Name != "SSR 节点" {
		t.Errorf("SSR remarks 名称不符: %q", n.Name)
	}
	if n.Metadata["method"] != "aes-256-cfb" || n.Metadata["protocol"] != "origin" ||
		n.Metadata["obfs"] != "plain" || n.Metadata["password"] != "secret" {
		t.Errorf("SSR Metadata 不符: %+v", n.Metadata)
	}
	if n.Metadata["obfs_param"] != "obfs-param" {
		t.Errorf("SSR obfs_param 不符: %v", n.Metadata["obfs_param"])
	}

	// 名称片段优先于 remarks
	n2 := mustParseOne(t, link+"#别名")
	if n2.Name != "别名" {
		t.Errorf("SSR 片段名称应优先: %q", n2.Name)
	}

	// 非法 base64 报错
	if _, err := ParseContent("ssr://!!!not-base64!!!"); err == nil {
		t.Error("非法 SSR 链接应返回错误")
	}
}
