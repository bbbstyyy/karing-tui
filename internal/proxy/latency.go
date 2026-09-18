package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/core"
)

// DefaultTestURL 默认延迟测试地址。
const DefaultTestURL = config.DefaultTestURL

// defaultConcurrency 同时测速的节点数上限。
const defaultConcurrency = 8

// defaultProbeAttempts 是「测速 API 端口被抢占」时的最大尝试次数（V7-6）。
const defaultProbeAttempts = 3

// probeAPIReadyTimeout 等待临时核心的 clash API 就绪的上限。
const probeAPIReadyTimeout = 5 * time.Second

// testNodes 用一次性 sing-box 实例批量测速。
// 配置生成交给 config.GenerateLatencyProbe：它复用正式配置的 DNS 语义，
// 把「节点服务器域名 → IP」的解析来源（dns + route.default_domain_resolver）
// 显式带进临时核心。此前这里手工拼「只有出站 + clash_api」的配置，节点服务器
// 是域名时就落在与运行核心完全不同的解析环境里，表现为批量假失败。
//
// 本函数只负责：临时文件、启动 adhoc、等 API、并发调 delay、汇总结果。
//
// 端口分配与内核 bind 之间是 TOCTOU 窗口（见 probeAddrInUse），因此
// 「分配端口 → 生成配置 → 启动 → 等 API」整体是一个有限重试单元（V7-6）：
// 每次重试都重新分配端口并重新生成配置（端口写进了配置，复用旧 plan 等于继续
// 用被抢走的那个端口），且只对明确的 bind 冲突重试。
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

	dnsCfg, err := m.loadProbeDNS()
	if err != nil {
		return nil, err
	}

	var (
		inst     *core.Adhoc
		apiPort  int
		tasks    []delayTask
		cfgPaths []string
	)
	// 临时配置要等临时核心读完之后才能删（cmd.Start 返回不代表子进程已读完配置），
	// 因此统一在函数退出时清理——每次重试都会留下一个待清理的路径。
	defer func() {
		for _, p := range cfgPaths {
			_ = os.Remove(p)
		}
	}()

	reported := false
	for attempt := 1; attempt <= defaultProbeAttempts; attempt++ {
		apiPort, err = m.allocatePort()
		if err != nil {
			return results, fmt.Errorf("分配测速 API 端口失败: %w", err)
		}
		plan, planErr := config.GenerateLatencyProbe(config.LatencyProbe{Nodes: nodes, DNS: dnsCfg, APIPort: apiPort})
		if planErr != nil {
			return results, planErr
		}
		if !reported {
			reported = true
			// 不可生成的节点先逐个回报：用户需要看到「哪个节点、为什么」，而不是一个总数。
			// Skipped 与端口无关，重试时不重复回报（否则用户会同一节点收到两条）。
			skips := make([]string, 0, len(plan.Skipped))
			for _, skip := range plan.Skipped {
				skips = append(skips, skip.Err.Error())
				results[skip.ID] = -1
				if report != nil {
					report(NodeTestResult{ID: skip.ID, Name: skip.Name, LatencyMS: -1, Err: skip.Err})
				}
			}
			if len(plan.Targets) == 0 {
				// results 必须一起返回（V7-7）：它已经带着「每个被 skip 节点 → -1」，
				// 上层据此仍会写入 last_tested 与 latency_ms，
				// AutoClean 的判据（last_tested IS NOT NULL AND latency_ms < 0）才成立。
				return results, fmt.Errorf("没有可测速的节点（%d 个被跳过），首条: %s", len(plan.Skipped), firstStr(skips))
			}
			tasks = make([]delayTask, 0, len(plan.Targets))
			for _, t := range plan.Targets {
				tasks = append(tasks, delayTask{id: t.ID, tag: t.Tag, name: t.Name})
			}
		}

		bin, binErr := m.Bin.Ensure(ctx)
		if binErr != nil {
			return results, fmt.Errorf("sing-box 二进制不可用: %w", binErr)
		}
		cfgPath, writeErr := m.writeProbeConfig(plan.Data)
		if writeErr != nil {
			return results, writeErr
		}
		cfgPaths = append(cfgPaths, cfgPath)

		inst, err = m.startAdhoc(ctx, bin, cfgPath, m.Paths.Cache)
		if err != nil {
			if attempt < defaultProbeAttempts && probeAddrInUse(err) {
				continue
			}
			return results, err
		}
		if waitErr := waitAPI(inst, apiPort, probeAPIReadyTimeout); waitErr != nil {
			inst.Stop()
			inst = nil
			if attempt < defaultProbeAttempts && probeAddrInUse(waitErr) {
				continue
			}
			return results, waitErr
		}
		break
	}
	if inst == nil {
		// 只有 defaultProbeAttempts <= 0 时才可能走到这里；留作防御，
		// 避免将来有人把它调成 0 时 panic 在 inst.Stop 上。
		return results, fmt.Errorf("测速临时核心启动失败：已尝试 %d 次", defaultProbeAttempts)
	}
	defer inst.Stop()

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

// allocatePort 分配测速 API 端口；AllocatePort 未注入时用 core.FreePort。
func (m *Manager) allocatePort() (int, error) {
	if m.AllocatePort != nil {
		return m.AllocatePort()
	}
	return core.FreePort()
}

// startAdhoc 启动临时测速核心；StartAdhoc 未注入时用 core.StartAdhoc。
func (m *Manager) startAdhoc(ctx context.Context, bin, configPath, cacheDir string) (*core.Adhoc, error) {
	if m.StartAdhoc != nil {
		return m.StartAdhoc(ctx, bin, configPath, cacheDir)
	}
	return core.StartAdhoc(ctx, bin, configPath, cacheDir)
}

// writeProbeConfig 把探测配置写进临时文件并返回路径（调用方负责删除）。
func (m *Manager) writeProbeConfig(data []byte) (string, error) {
	file, err := os.CreateTemp(m.Paths.Runtime, "latency-*.json")
	if err != nil {
		return "", fmt.Errorf("创建测速配置失败: %w", err)
	}
	path := file.Name()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("写入测速配置失败: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("写入测速配置失败: %w", err)
	}
	return path, nil
}

// probeAddrInUse 判断错误是否由「测速 API 端口被抢占」造成。
//
// 判据刻意收窄到内核实际打出的那串文本：core.FreePort 返回的只是**候选**端口
// （它监听 :0 拿到号码后立刻关闭），从关掉 listener 到 sing-box 真正 bind 之间
// 还有几百毫秒（生成配置、落临时文件、确保二进制可用），任何进程都可能插进来。
// 这类失败偶发、重试即好。
//
// 放宽成「任何启动失败都重试」会把配置非法、二进制不可用之类的真故障伪装成
// 偶发，反而更难定位，因此非 bind 错误一律原样返回、不重试。
// waitAPI 在实例提前退出时会把日志尾部带进错误文本，bind 冲突的原文因此能匹配到。
func probeAddrInUse(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "address already in use")
}

// loadProbeDNS 取「当前已保存的 DNS 配置」作为独立测速核心的解析环境。
//
// 与代理组测速的区别是刻意的：运行中的核心用**已应用**的配置，而独立测速在核心
// 停止时也必须能工作，因此只能读已保存配置。用户改了 DNS 但还没应用时，两条路径
// 短暂不一致属于预期行为。本次修复的 bug 不是「Applied vs Saved」，而是
// 「Applied/Saved DNS vs 完全没有 DNS」。
//
// LoadDNS 未注入属于**装配错误**（生产路径由 application.NewApp 注入 a.DNS.LoadConfig），
// 因此 fail-closed：静默按「没有 DNS」处理会悄悄退回本次要修的行为——临时核心不带
// dns / default_domain_resolver，域名型节点又变成批量假失败，而且没有任何报错。
// 注意「没有配置 DNS 服务器」（LoadDNS 正常返回空 DNSConfig）是另一回事，那种情况
// 与主核心语义一致，仍然允许。
func (m *Manager) loadProbeDNS() (*config.DNSConfig, error) {
	if m.LoadDNS == nil {
		return nil, fmt.Errorf("测速 DNS loader 未初始化：proxy.Manager.LoadDNS 必须由调用方注入（见 application.NewApp）")
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
//
// 附日志尾部是有意的：bind 冲突只在 run 阶段暴露（`sing-box check` 对此返回 0），
// 错误文本里的 "address already in use" 是重试判据（probeAddrInUse）的唯一来源。
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
