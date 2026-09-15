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
// 做法：把全部节点生成为出站写入临时配置，开启 clash_api，
// 逐个调用 /proxies/{tag}/delay 让 sing-box 经对应出站发探测请求。
func (m *Manager) testNodes(ctx context.Context, nodes []*config.Node, testURL string, timeoutMS int, report func(NodeTestResult)) (map[int64]int64, error) {
	results := map[int64]int64{}
	if len(nodes) == 0 {
		return results, nil
	}
	if testURL == "" {
		testURL = DefaultTestURL
	}
	if timeoutMS <= 0 {
		timeoutMS = 3000
	}

	apiPort, err := core.FreePort()
	if err != nil {
		return nil, fmt.Errorf("分配测速 API 端口失败: %w", err)
	}

	// 构建临时配置：仅出站 + clash_api
	var (
		outbounds []any
		tasks     []delayTask
		skips     []string
	)
	for _, n := range nodes {
		tag := fmt.Sprintf("t%d", n.ID)
		ob, err := config.NodeToOutbound(n, tag)
		if err != nil {
			skips = append(skips, err.Error())
			results[n.ID] = -1
			if report != nil {
				report(NodeTestResult{ID: n.ID, Name: n.Name, LatencyMS: -1, Err: err})
			}
			continue
		}
		outbounds = append(outbounds, ob)
		tasks = append(tasks, delayTask{id: n.ID, tag: tag, name: n.Name})
	}
	if len(outbounds) == 0 {
		return nil, fmt.Errorf("没有可测速的节点（%d 个被跳过），首条: %s", len(skips), firstStr(skips))
	}
	cfg := map[string]any{
		"log": map[string]any{"level": "warn"},
		"experimental": map[string]any{
			"clash_api": map[string]any{
				"external_controller": fmt.Sprintf("127.0.0.1:%d", apiPort),
			},
		},
		"outbounds": outbounds,
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化测速配置失败: %w", err)
	}
	file, err := os.CreateTemp(m.Paths.Runtime, "latency-*.json")
	if err != nil {
		return nil, fmt.Errorf("创建测速配置失败: %w", err)
	}
	cfgPath := file.Name()
	defer os.Remove(cfgPath)
	if _, err := file.Write(data); err != nil {
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
