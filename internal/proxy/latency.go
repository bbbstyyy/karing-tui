package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/core"
)

// DefaultTestURL 默认延迟测试地址。
const DefaultTestURL = config.DefaultTestURL

// defaultConcurrency 同时测速的节点数上限。
const defaultConcurrency = 8

// testNodes 用一次性 sing-box 实例批量测速。
// 配置生成交给 config.GenerateLatencyProbe：它复用正式配置的 DNS 语义，
// 把「节点服务器域名 → IP」的解析来源（dns + route.default_domain_resolver）
// 显式带进临时核心。此前这里手工拼「只有出站 + clash_api」的配置，节点服务器
// 是域名时就落在与运行核心完全不同的解析环境里，表现为批量假失败。
//
// 本函数只负责：临时文件、启动 adhoc、等 API、并发调 delay、汇总结果。
func (m *Manager) testNodes(ctx context.Context, nodes []*config.Node, testURL string, timeoutMS int, report func(NodeTestResult)) (map[int64]int64, error) {
	results := map[int64]int64{}
	if len(nodes) == 0 {
		return results, nil
	}
	if testURL == "" {
		testURL = DefaultTestURL
	}
	if timeoutMS <= 0 {
		timeoutMS = config.DefaultLatencyTimeoutMS
	}

	apiPort, err := core.FreePort()
	if err != nil {
		return nil, fmt.Errorf("分配测速 API 端口失败: %w", err)
	}

	dnsCfg, err := m.loadProbeDNS()
	if err != nil {
		return nil, err
	}
	plan, err := config.GenerateLatencyProbe(config.LatencyProbe{Nodes: nodes, DNS: dnsCfg, APIPort: apiPort})
	if err != nil {
		return nil, err
	}
	// 不可生成的节点先逐个回报：用户需要看到「哪个节点、为什么」，而不是一个总数。
	skips := make([]string, 0, len(plan.Skipped))
	for _, skip := range plan.Skipped {
		skips = append(skips, skip.Err.Error())
		results[skip.ID] = -1
		if report != nil {
			report(NodeTestResult{ID: skip.ID, Name: skip.Name, LatencyMS: -1, Err: skip.Err})
		}
	}
	if len(plan.Targets) == 0 {
		return nil, fmt.Errorf("没有可测速的节点（%d 个被跳过），首条: %s", len(plan.Skipped), firstStr(skips))
	}
	tasks := make([]delayTask, 0, len(plan.Targets))
	for _, t := range plan.Targets {
		tasks = append(tasks, delayTask{id: t.ID, tag: t.Tag, name: t.Name})
	}

	file, err := os.CreateTemp(m.Paths.Runtime, "latency-*.json")
	if err != nil {
		return nil, fmt.Errorf("创建测速配置失败: %w", err)
	}
	cfgPath := file.Name()
	defer os.Remove(cfgPath)
	if _, err := file.Write(plan.Data); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("写入测速配置失败: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("写入测速配置失败: %w", err)
	}

	bin, err := m.Bin.Ensure(ctx)
	if err != nil {
		return nil, fmt.Errorf("sing-box 二进制不可用: %w", err)
	}
	inst, err := core.StartAdhoc(ctx, bin, cfgPath, m.Paths.Cache)
	if err != nil {
		return nil, err
	}
	defer inst.Stop()

	// 等待 clash API 端口就绪
	if err := waitAPI(inst, apiPort, 5*time.Second); err != nil {
		return nil, err
	}

	// 并发探测
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Loopback core API never uses an environment proxy.
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Duration(timeoutMS)*time.Millisecond + 2*time.Second}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, defaultConcurrency)
	)
	for _, t := range tasks {
		wg.Add(1)
		go func(t delayTask) {
			defer wg.Done()
			ms := int64(-1)
			var err error
			select {
			case sem <- struct{}{}:
				ms, err = queryDelay(ctx, client, apiPort, t.tag, testURL, timeoutMS)
				<-sem
			case <-ctx.Done():
				err = ctx.Err()
			}
			mu.Lock()
			results[t.id] = ms
			mu.Unlock()
			if report != nil {
				report(NodeTestResult{ID: t.id, Name: t.name, LatencyMS: ms, Err: err})
			}
		}(t)
	}
	wg.Wait()
	return results, nil
}

// loadProbeDNS 取「当前已保存的 DNS 配置」作为独立测速核心的解析环境。
//
// 与代理组测速的区别是刻意的：运行中的核心用**已应用**的配置，而独立测速在核心
// 停止时也必须能工作，因此只能读已保存配置。用户改了 DNS 但还没应用时，两条路径
// 短暂不一致属于预期行为。本次修复的 bug 不是「Applied vs Saved」，而是
// 「Applied/Saved DNS vs 完全没有 DNS」。
func (m *Manager) loadProbeDNS() (*config.DNSConfig, error) {
	if m.LoadDNS == nil {
		return nil, nil
	}
	cfg, err := m.LoadDNS()
	if err != nil {
		return nil, fmt.Errorf("加载测速 DNS 配置失败: %w", err)
	}
	return &cfg, nil
}

// delayTask 一个节点的测速任务。
type delayTask struct {
	id   int64
	tag  string
	name string
}

// waitAPI 轮询等待 clash API 端口可连接；实例提前退出时报错并附日志尾部。
func waitAPI(inst *core.Adhoc, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if inst.Exited() {
			return fmt.Errorf("临时 sing-box 实例启动失败，最近日志:\n%s", inst.Output.Tail(10))
		}
		if core.PortOpen("127.0.0.1", port) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("等待测速 API 就绪超时")
}

// queryDelay 调用 clash API 测单节点延迟；失败返回 -1。
func queryDelay(ctx context.Context, client *http.Client, apiPort int, tag, testURL string, timeoutMS int) (int64, error) {
	api := fmt.Sprintf("http://127.0.0.1:%d/proxies/%s/delay?timeout=%d&url=%s",
		apiPort, url.PathEscape(tag), timeoutMS, url.QueryEscape(testURL))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return -1, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	var out struct {
		Delay   int64  `json:"delay"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return -1, err
	}
	if resp.StatusCode != http.StatusOK || out.Delay <= 0 {
		return -1, fmt.Errorf("测速失败（HTTP %d）: %s", resp.StatusCode, out.Message)
	}
	return out.Delay, nil
}

func firstStr(list []string) string {
	if len(list) > 0 {
		return list[0]
	}
	return ""
}
