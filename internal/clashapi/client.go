// Package clashapi 是 sing-box experimental.clash_api 的 Go 客户端，
// 供 Dashboard 查询流量统计、代理组当前节点与延迟、触发组测速。
package clashapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ProxyInfo 单个出站/代理组的运行时信息。
type ProxyInfo struct {
	Type     string   // selector / urltest / shadowsocks / ...
	Name     string   // 出站 tag
	Now      string   // 组当前生效的成员 tag（非组为空）
	All      []string // 组成员 tag 列表（非组为空）
	DelayMS  int64    // 最近一次 URLTest 延迟；0 表示无记录
	TestedAt time.Time
}

// Connections 流量与连接快照。
type Connections struct {
	UploadTotal   int64 // 进程启动以来累计上行字节
	DownloadTotal int64 // 进程启动以来累计下行字节
	Count         int   // 当前活跃连接数
}

// Connection 是 Clash API /connections 的稳定子集，便于 headless 诊断脚本使用。
type Connection struct {
	ID          string   `json:"id,omitempty"`
	Metadata    any      `json:"metadata,omitempty"`
	Upload      int64    `json:"upload,omitempty"`
	Download    int64    `json:"download,omitempty"`
	Start       string   `json:"start,omitempty"`
	Chains      []string `json:"chains,omitempty"`
	Rule        string   `json:"rule,omitempty"`
	RulePayload string   `json:"rulePayload,omitempty"`
}

// DetailedConnections 返回累计流量与当前连接详情。
func (c *Client) DetailedConnections(ctx context.Context) (Connections, []Connection, error) {
	var raw struct {
		UploadTotal   int64        `json:"uploadTotal"`
		DownloadTotal int64        `json:"downloadTotal"`
		Connections   []Connection `json:"connections"`
	}
	if err := c.get(ctx, "/connections", &raw); err != nil {
		return Connections{}, nil, err
	}
	return Connections{UploadTotal: raw.UploadTotal, DownloadTotal: raw.DownloadTotal, Count: len(raw.Connections)}, raw.Connections, nil
}

// Client clash API 客户端。所有方法可安全并发调用。
type Client struct {
	base   string // 如 http://127.0.0.1:9090
	secret string
	hc     *http.Client
}

// New 创建客户端。host 一般为 127.0.0.1（AllowLAN 时生成配置监听 0.0.0.0，仍可用本机回环访问）。
func New(host string, port int, secret string) *Client {
	return &Client{
		base:   fmt.Sprintf("http://%s:%d", host, port),
		secret: secret,
		hc:     &http.Client{Timeout: 10 * time.Second},
	}
}

// get 发起 GET 并解析 JSON 响应。
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("clash API %s 返回 %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// urlTestHistory clash API 的延迟记录（数组至多一项）。
type urlTestHistory struct {
	Time  time.Time `json:"time"`
	Delay int64     `json:"delay"`
}

type proxyRaw struct {
	Type    string           `json:"type"`
	Name    string           `json:"name"`
	Now     string           `json:"now"`
	All     []string         `json:"all"`
	History []urlTestHistory `json:"history"`
}

// Proxies 返回全部出站的运行时信息，键为出站 tag。
func (c *Client) Proxies(ctx context.Context) (map[string]ProxyInfo, error) {
	var raw struct {
		Proxies map[string]proxyRaw `json:"proxies"`
	}
	if err := c.get(ctx, "/proxies", &raw); err != nil {
		return nil, err
	}
	out := make(map[string]ProxyInfo, len(raw.Proxies))
	for tag, p := range raw.Proxies {
		info := ProxyInfo{Type: p.Type, Name: p.Name, Now: p.Now, All: p.All}
		if len(p.History) > 0 {
			// 取最新一条记录（当前实现至多一条）
			last := p.History[len(p.History)-1]
			info.DelayMS, info.TestedAt = last.Delay, last.Time
		}
		out[tag] = info
	}
	return out, nil
}

// Connections 返回流量累计与活跃连接数。
func (c *Client) Connections(ctx context.Context) (Connections, error) {
	stats, _, err := c.DetailedConnections(ctx)
	return stats, err
}

// Select 切换 selector 组的当前出站。
func (c *Client) Select(ctx context.Context, group, name string) error {
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.base+"/proxies/"+url.PathEscape(group), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("切换代理组 %q 失败: HTTP %d", group, resp.StatusCode)
	}
	return nil
}

// GroupDelay 对组内全部成员测速，返回 成员tag→延迟毫秒。
// url 为空时由 sing-box 使用其默认测速地址。
func (c *Client) GroupDelay(ctx context.Context, group, testURL string, timeoutMS int) (map[string]int64, error) {
	q := url.Values{}
	if testURL != "" {
		q.Set("url", testURL)
	}
	q.Set("timeout", fmt.Sprintf("%d", timeoutMS))
	var out map[string]int64
	if err := c.get(ctx, "/group/"+url.PathEscape(group)+"/delay?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return out, nil
}
