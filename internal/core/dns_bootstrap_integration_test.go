package core

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// 真实内核的 bootstrap 安全门禁。
//
// **历史**（V9-5）：V8-1 允许删掉 `local` 之后，「只剩一台经代理组的 DNS」+「代理节点的
// 服务器地址是域名」会陷入 bootstrap 环 —— 真机判决结论 **(b)：内核不报错，但在解析阶段
// 静默卡死**，既不解析节点域名、也永远连不上节点。当时的四组对照为：
//
//	组  default_domain_resolver  节点 server   DNS 收到 node.test   节点被真正连上
//	1   local（直连）            node.test     是                  是        ← V8-1 之前的状态
//	2   remote（**直连** detour 为空）node.test 是                  是        ← 排除「fixture / udp 传输不支持」
//	3   remote（**经 Auto**）     node.test    **否**              **否**     ← 缺陷
//	4   remote（**经 Auto**）     127.0.0.1    否                  是        ← 排除「经 Auto 的 DNS 本身不可用」
//
// 组 2 与组 4 把病因精确夹逼到「resolver 经代理组」×「该组的节点服务器是域名」这一个组合上。
// 内核**不会拒绝启动**（无 FATAL），日志里只有反复的
// `outbound/http[n]: outbound connection to www.gstatic.com:80`，没有任何解析失败的 ERROR
// —— 比启动报错更难排查。V7-8/V8-1 的「写库前拦环」救不了它：那个环跨了 DNS 与出站两张图，
// 两张图各自都是合法无环的。
//
// **现在（V9-6 之后）本用例的职责变了**：组 3 不再交给内核——生成阶段就 fail-closed
// （`ErrNoBootstrapDNS`），因此改成断言 `config.Generate` 拒绝；组 1/2/4 继续真启动、
// 真解析、真拨号，作为「合法形态不得被新判据误伤」的非回归护栏。
//
// 长期要守的两件事：① 生成器不会再把这个缺陷形态交给内核；② 合法形态仍能真跑起来。
func TestRealSingBoxDNSBootstrapSafety(t *testing.T) {
	src := os.Getenv("SINGBOX_BIN")
	if src == "" {
		t.Skip("未设置 SINGBOX_BIN，跳过真实 sing-box 集成测试")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读取 SINGBOX_BIN: %v", err)
	}
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.CoreBin, data, 0o755); err != nil {
		t.Fatal(err)
	}

	dnsSrv := startBootstrapProbeDNS(t)
	proxy := startBootstrapProbeProxy(t)

	directRemote := []config.DNSServer{
		{Tag: "remote", Type: "udp", Address: dnsSrv.addr, Enabled: true},
	}
	proxyRemote := []config.DNSServer{
		{Tag: "remote", Type: "udp", Address: dnsSrv.addr, Detour: "Auto", Enabled: true},
	}
	withLocal := []config.DNSServer{
		{Tag: "local", Type: "udp", Address: dnsSrv.addr, Enabled: true},
		{Tag: "remote", Type: "udp", Address: dnsSrv.addr, Detour: "Auto", Enabled: true},
	}

	cases := []struct {
		name         string
		dns          []config.DNSServer
		wantTag      string // 期望被选中的 default_domain_resolver（wantRejected 为真时不看）
		nodeServer   string
		wantResolve  bool // 期望测试 DNS 收到 node.test（节点是字面 IP 时无意义，填 false 且不看）
		wantDial     bool // 期望节点被真正连上
		wantRejected bool // 期望在**生成阶段**就 fail-closed（V9-6）
	}{
		{"控制组-local直连", withLocal, "local", "node.test", true, true, false},
		{"只剩remote但直连", directRemote, "remote", "node.test", true, true, false},
		{"只剩remote经Auto-域名节点", proxyRemote, "", "node.test", false, false, true},
		{"只剩remote经Auto-IP节点", proxyRemote, "remote", "127.0.0.1", false, true, false},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dnsSrv.reset()
			proxy.reset()
			settings := config.DefaultSettings()
			settings.MixedPort = freePort(t)
			settings.ClashAPIPort = 0
			// 必须关掉：否则到 127.0.0.1 的连接会直连，绕过 Auto，组 3 就验不到东西。
			settings.PrivateDirect = false
			snap := config.Snapshot{
				Settings: settings,
				Nodes: []*config.Node{{ID: 1, Name: "HK-01", Protocol: "http", Server: tc.nodeServer,
					Port: proxy.port, Enabled: true}},
				ProxyGroups: []*config.ProxyGroup{{ID: 1, Name: "Auto", Type: "urltest",
					TestURL: config.DefaultTestURL, IntervalS: 1,
					Members: []config.ProxyGroupMember{{Type: "node", ID: 1}}}},
				// final 指向 Auto，让「任何流量」都必须经过那个节点 —— 用 urltest 的
				// 周期探测作为触发器（它拨号节点的方式与真实流量完全一样）。
				RoutingGroups: []*config.RoutingGroup{{ID: 1, Name: "final", Target: "Auto",
					Kind: config.KindFinal, Position: 0, Enabled: true,
					Rules: []config.Rule{{Type: "final", Enabled: true}}}},
				DNS: &config.DNSConfig{Strategy: "prefer_ipv4", Servers: tc.dns,
					Final: tc.dns[len(tc.dns)-1].Tag},
			}
			out, err := config.Generate(snap)
			if tc.wantRejected {
				// V9-6：缺陷形态必须在**生成阶段**被拒，不再交给内核静默卡死。
				var noBootstrap config.ErrNoBootstrapDNS
				if !errors.As(err, &noBootstrap) {
					t.Fatalf("该形态必须在生成阶段 fail-closed，实得 err=%v（配置 %d 字节）", err, len(out))
				}
				if out != nil {
					t.Error("fail-closed 时不得产出半可用配置")
				}
				for _, want := range []string{"remote", "Auto", "HK-01", "node.test"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("错误文案应包含 %q，实得: %v", want, err)
					}
				}
				t.Logf("生成阶段已拒绝: %v", err)
				return
			}
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			// 前提断言：先证明我们复现的确实是「解析器被选成了谁」那个形状。
			var parsed map[string]any
			if err := json.Unmarshal(out, &parsed); err != nil {
				t.Fatal(err)
			}
			route, ok := parsed["route"].(map[string]any)
			if !ok {
				t.Fatalf("配置里没有 route 段: %v", parsed["route"])
			}
			if got := route["default_domain_resolver"]; got != tc.wantTag {
				t.Fatalf("夹具失效：default_domain_resolver = %v，期望 %q", got, tc.wantTag)
			}

			cfgPath := filepath.Join(paths.Runtime, "bootstrap-probe-"+string(rune('a'+i))+".json")
			if err := os.WriteFile(cfgPath, out, 0o600); err != nil {
				t.Fatal(err)
			}
			inst, err := StartAdhoc(context.Background(), paths.CoreBin, cfgPath, paths.Cache)
			if err != nil {
				t.Fatalf("StartAdhoc: %v", err)
			}
			defer inst.Stop()

			// 给内核 4 秒：urltest 的 interval=1s，这段时间内它一定尝试过拨号。
			time.Sleep(4 * time.Second)

			if inst.Exited() {
				t.Fatalf("内核不应启动失败（本项结论是「静默卡死」而非启动报错），日志:\n%s",
					strings.Join(inst.Output.Tail(20), "\n"))
			}
			// 反向核对的底座：日志里必须真的出现过拨号尝试，否则「什么都没发生」
			// 可能只是内核还没开始干活，而不是 bootstrap 环。
			attempts := strings.Count(strings.Join(inst.Output.Tail(50), "\n"), "outbound connection to")
			if attempts < 1 {
				t.Fatalf("夹具失效：4 秒内没有观察到任何拨号尝试\n日志:\n%s",
					strings.Join(inst.Output.Tail(20), "\n"))
			}
			// 负例需要更强的前置：必须看到**重复**尝试。实测正例只留 1 条
			// （拨通之后 urltest 不再重复记日志），所以这条门槛只对负例成立。
			if !tc.wantDial && attempts < 2 {
				t.Fatalf("夹具失效：负例应观察到重复拨号尝试，实得 %d 次，无法判定「卡住」还是「没开始」\n日志:\n%s",
					attempts, strings.Join(inst.Output.Tail(20), "\n"))
			}

			if tc.nodeServer != "127.0.0.1" {
				if got := dnsSrv.sawQuery("node.test"); got != tc.wantResolve {
					t.Errorf("测试 DNS 收到 node.test = %v，期望 %v（查询记录 %v）",
						got, tc.wantResolve, dnsSrv.queries())
				}
			}
			dialled := proxy.conns() > 0
			if dialled != tc.wantDial {
				t.Errorf("节点被真正连上 = %v，期望 %v（%d 次拨号尝试，%d 次到达假代理）",
					dialled, tc.wantDial, attempts, proxy.conns())
			}
			t.Logf("%d 次拨号尝试 / DNS=%v / 到达代理=%d", attempts, dnsSrv.queries(), proxy.conns())
		})
	}
}

// --- 最小 fixture：一个会记录查询的 UDP DNS + 一个只记录「被连上了」的假代理 ---

type bootstrapProbeDNS struct {
	addr string

	mu   sync.Mutex
	seen map[string]int
}

func startBootstrapProbeDNS(t *testing.T) *bootstrapProbeDNS {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s := &bootstrapProbeDNS{addr: conn.LocalAddr().String(), seen: map[string]int{}}
	go func() {
		buf := make([]byte, 512)
		for {
			n, raddr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			name, resp := bootstrapDNSReply(buf[:n])
			if name != "" {
				s.mu.Lock()
				s.seen[strings.ToLower(name)]++
				s.mu.Unlock()
			}
			if resp != nil {
				_, _ = conn.WriteToUDP(resp, raddr)
			}
		}
	}()
	return s
}

func (s *bootstrapProbeDNS) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = map[string]int{}
}

func (s *bootstrapProbeDNS) sawQuery(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[strings.ToLower(name)] > 0
}

func (s *bootstrapProbeDNS) queries() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.seen))
	for k, v := range s.seen {
		out[k] = v
	}
	return out
}

// bootstrapDNSReply 最小应答：任意 A 查询都答 127.0.0.1，其余答空。
// 不追求完整 DNS 语义，只服务「这个域名有没有被解析过」这一个观测。
func bootstrapDNSReply(q []byte) (string, []byte) {
	if len(q) < 12 || binary.BigEndian.Uint16(q[4:6]) != 1 {
		return "", nil
	}
	var labels []string
	i := 12
	for ; i < len(q); i++ {
		l := int(q[i])
		if l == 0 {
			i++
			break
		}
		if l&0xC0 != 0 || i+1+l > len(q) {
			return "", nil
		}
		labels = append(labels, string(q[i+1:i+1+l]))
		i += l
	}
	if i+4 > len(q) {
		return "", nil
	}
	name := strings.Join(labels, ".")
	end := i + 4
	hit := binary.BigEndian.Uint16(q[i:i+2]) == 1
	header := []byte{q[0], q[1], 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0}
	if hit {
		header[7] = 1
	}
	resp := append(header, q[12:end]...)
	if !hit {
		return name, resp
	}
	resp = append(resp, 0xC0, 0x0C, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x3C, 0x00, 0x04)
	return name, append(resp, net.IPv4(127, 0, 0, 1).To4()...)
}

// bootstrapProbeProxy 只做一件事：记录「有连接到达」。
// 它不转发、不实现代理协议 —— 本项要判的是「节点有没有被真正连上」这一步，
// 到达这一步就说明节点的域名已经被解析出来了。
type bootstrapProbeProxy struct {
	port int

	mu       sync.Mutex
	accepted int
}

func startBootstrapProbeProxy(t *testing.T) *bootstrapProbeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	p := &bootstrapProbeProxy{port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.accepted++
			p.mu.Unlock()
			go func(conn net.Conn) { _ = conn.Close() }(conn)
		}
	}()
	return p
}

func (p *bootstrapProbeProxy) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accepted = 0
}

func (p *bootstrapProbeProxy) conns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepted
}
