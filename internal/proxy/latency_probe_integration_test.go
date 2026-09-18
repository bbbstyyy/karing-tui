package proxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/core"
)

// TestLatencyProbeResolvesNodeDomain 是本故障的判决性集成测试：节点 server 是域名时，
// 让真实 sing-box 分别在有 / 无 probe DNS 的条件下发起延迟探测，观察解析与建连是否发生。
//
// 判据（不依赖公网）：
//
//	带 probe DNS    → 测试 DNS 收到 node.test 查询，且假代理收到连接（探测进入连接阶段）
//	去掉 probe DNS  → 两者都不发生（系统解析器答不出 .test，探测在解析阶段就失败）
//
// 注意这是本机内核实测的结论：sing-box 的 /proxies/{tag}/delay 会忽略 url 参数、
// 固定探测 https://www.gstatic.com:443。因此这里断言「解析与建连是否发生」，
// 而不断言延迟值——探测的 TLS 阶段需要真实公网。
//
// 删掉 config.GenerateLatencyProbe 里的 dns / default_domain_resolver 输出，
// 「带 probe DNS」这一组会立刻变红。
func TestLatencyProbeResolvesNodeDomain(t *testing.T) {
	m := newProbeTestManager(t)
	dnsSrv := startTestDNS(t, "node.test")
	proxy := startFakeProxy(t)

	node := &config.Node{ID: 1, Name: "http-proxy", Protocol: "http", Server: "node.test", Port: proxy.port, Enabled: true}

	t.Run("带 probe DNS 时节点域名可解析并进入连接阶段", func(t *testing.T) {
		dnsSrv.reset()
		proxy.reset()
		m.LoadDNS = func() (config.DNSConfig, error) {
			return config.DNSConfig{
				Strategy: "prefer_ipv4",
				Servers:  []config.DNSServer{{Tag: "test", Type: "udp", Address: dnsSrv.addr, Enabled: true}},
			}, nil
		}
		probeOnce(t, m, node)

		if !dnsSrv.sawQuery("node.test") {
			t.Errorf("测速核心未通过 probe DNS 解析节点域名，查询记录: %v", dnsSrv.queries())
		}
		if n := proxy.conns(); n == 0 {
			t.Error("节点域名解析成功却未建立连接：探测没有进入连接阶段")
		}
		if target := proxy.firstTarget(); !strings.Contains(target, "gstatic.com") {
			t.Errorf("假代理收到的目标 = %q, 期望 sing-box 固定的探测目标", target)
		}
	})

	t.Run("删除 probe DNS 后同一 fixture 必须失败", func(t *testing.T) {
		// 空 DNS 配置 = 探针不带 dns 段，退化成修复前的行为：临时核心只能靠
		// 系统解析器，node.test 无从解析。这一组是 §27 回退实验的常驻形式。
		dnsSrv.reset()
		proxy.reset()
		m.LoadDNS = func() (config.DNSConfig, error) { return config.DNSConfig{}, nil }
		latency, err := probeOnce(t, m, node)
		if err != nil {
			t.Fatalf("testNodes: %v", err)
		}
		if latency != -1 {
			t.Fatalf("缺少 probe DNS 时域名型节点不应测通，实际 %d ms", latency)
		}
		if dnsSrv.sawQuery("node.test") {
			t.Error("探针不应在删除 dns 段后仍然解析节点域名")
		}
		if proxy.conns() != 0 {
			t.Error("节点域名未能解析，却仍然建立了连接")
		}
	})

	t.Run("IP 型节点无回归", func(t *testing.T) {
		// 地址是字面 IP 的节点不依赖 DNS，修复前后行为必须一致：无 dns 段也能直接建连。
		proxy.reset()
		m.LoadDNS = func() (config.DNSConfig, error) { return config.DNSConfig{}, nil }
		ipNode := &config.Node{ID: 2, Name: "ip", Protocol: "http", Server: "127.0.0.1", Port: proxy.port, Enabled: true}
		probeOnce(t, m, ipNode)
		if n := proxy.conns(); n == 0 {
			t.Error("IP 型节点应直接建连，无需任何 DNS")
		}
	})
}

// probeOnce 跑一次单节点测速并返回延迟（失败为 -1），失败原因打到测试日志便于排错。
func probeOnce(t *testing.T, m *Manager, node *config.Node, testURL ...string) (int64, error) {
	t.Helper()
	url := config.DefaultTestURL
	if len(testURL) > 0 {
		url = testURL[0]
	}
	var detail error
	results, err := m.testNodes(context.Background(), []*config.Node{node}, url, 0, func(r NodeTestResult) {
		if r.Err != nil {
			detail = r.Err
		}
	})
	if err != nil {
		return -1, err
	}
	if detail != nil {
		t.Logf("节点 %d 探测结果: %v（延迟 %d）", node.ID, detail, results[node.ID])
	}
	return results[node.ID], nil
}

// newProbeTestManager 准备一个能起临时 sing-box 的 Manager。
// 未设置 SINGBOX_BIN（与 core 集成测试同一约定）时跳过。
func newProbeTestManager(t *testing.T) *Manager {
	t.Helper()
	src := os.Getenv("SINGBOX_BIN")
	if src == "" {
		t.Skip("未设置 SINGBOX_BIN，跳过真实 sing-box 测速探针测试")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读取 SINGBOX_BIN: %v", err)
	}
	m := newTestManager(t)
	if err := os.WriteFile(m.Paths.CoreBin, data, 0o755); err != nil {
		t.Fatalf("写入受管二进制: %v", err)
	}
	m.Bin = core.NewBinaryManager(m.Paths, "")
	return m
}

// testDNSServer 只回答 A 记录的最小 UDP DNS 服务器，并把收到的查询记下来。
type testDNSServer struct {
	addr string

	mu   sync.Mutex
	seen map[string]int
}

func startTestDNS(t *testing.T, hosts ...string) *testDNSServer {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	answers := map[string]bool{}
	for _, h := range hosts {
		answers[strings.ToLower(h)] = true
	}
	srv := &testDNSServer{addr: conn.LocalAddr().String(), seen: map[string]int{}}
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return // 监听已关闭
			}
			resp, name := dnsReply(buf[:n], answers)
			if name != "" {
				srv.record(name)
			}
			if resp != nil {
				_, _ = conn.WriteToUDP(resp, addr)
			}
		}
	}()
	return srv
}

func (s *testDNSServer) record(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[strings.ToLower(name)]++
}

func (s *testDNSServer) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = map[string]int{}
}

func (s *testDNSServer) sawQuery(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[strings.ToLower(name)] > 0
}

func (s *testDNSServer) queries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.seen))
	for name := range s.seen {
		out = append(out, name)
	}
	return out
}

// dnsReply 构造应答：A 查询命中时返回 127.0.0.1，其余返回 NOERROR 空应答。
// 第二个返回值是问题段里的域名，供调用方记录观测到的查询。
// 只实现本测试需要的最小报文，不追求完整 DNS 语义。
func dnsReply(query []byte, answers map[string]bool) ([]byte, string) {
	if len(query) < 12 || binary.BigEndian.Uint16(query[4:6]) != 1 {
		return nil, ""
	}
	name, qtypeAt, ok := dnsQuestionName(query)
	if !ok || qtypeAt+4 > len(query) {
		return nil, ""
	}
	end := qtypeAt + 4
	hit := binary.BigEndian.Uint16(query[qtypeAt:qtypeAt+2]) == 1 && answers[strings.ToLower(name)]

	// QR=1、RD=1、RA=1；QDCOUNT=1、ANCOUNT 视命中而定。
	header := []byte{query[0], query[1], 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0}
	if hit {
		header[7] = 1
	}
	resp := append(header, query[12:end]...)
	if !hit {
		return resp, name
	}
	resp = append(resp,
		0xC0, 0x0C, // 名字指针指向问题段
		0x00, 0x01, // TYPE=A
		0x00, 0x01, // CLASS=IN
		0x00, 0x00, 0x00, 60, // TTL=60s
		0x00, 0x04, // RDLENGTH
	)
	return append(resp, net.IPv4(127, 0, 0, 1).To4()...), name
}

// dnsQuestionName 解析问题段的域名，返回名字与 qtype 起始偏移。
func dnsQuestionName(q []byte) (string, int, bool) {
	var labels []string
	for i := 12; i < len(q); {
		l := int(q[i])
		if l == 0 {
			return strings.Join(labels, "."), i + 1, true
		}
		if l&0xC0 != 0 || i+1+l > len(q) {
			return "", 0, false
		}
		labels = append(labels, string(q[i+1:i+1+l]))
		i += 1 + l
	}
	return "", 0, false
}

// fakeProxy 记录到达的连接：它不是真正的代理，只用来判断「探测是否进入了连接阶段」。
// 收到请求头后回一个 CONNECT 200 并立即关闭，让后续 TLS 快速失败——本测试不关心延迟值。
type fakeProxy struct {
	port int

	mu       sync.Mutex
	accepted int
	targets  []string
}

func startFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	p := &fakeProxy{port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // 监听已关闭
			}
			go p.serve(conn)
		}
	}()
	return p
}

func (p *fakeProxy) serve(conn net.Conn) {
	defer conn.Close()
	p.mu.Lock()
	p.accepted++
	p.mu.Unlock()

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return
	}
	if fields := strings.Fields(strings.TrimSpace(line)); len(fields) >= 2 {
		p.mu.Lock()
		p.targets = append(p.targets, fields[1])
		p.mu.Unlock()
	}
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
}

func (p *fakeProxy) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accepted, p.targets = 0, nil
}

func (p *fakeProxy) conns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepted
}

func (p *fakeProxy) firstTarget() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.targets) == 0 {
		return ""
	}
	return p.targets[0]
}
