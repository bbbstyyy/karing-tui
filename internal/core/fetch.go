package core

import (
	"context"
	"fmt"
	"github.com/bbbstyyy/karing-tui/internal/redact"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DefaultUserAgent 通用下载默认 User-Agent。
const DefaultUserAgent = "karing-tui/0.1"

// FetchHTTP 通过可选代理抓取 URL 内容，读取上限 limit 字节（<=0 用 10 MiB）。
// proxyURL 为空时直连，不读取环境代理。供订阅、规则集等下载共用。
func FetchHTTP(ctx context.Context, rawURL, proxyURL, userAgent string, limit int64) ([]byte, error) {
	body, _, err := FetchHTTPMeta(ctx, rawURL, proxyURL, userAgent, limit)
	return body, err
}

// FetchHTTPMeta additionally returns response headers useful to subscription clients.
func FetchHTTPMeta(ctx context.Context, rawURL, proxyURL, userAgent string, limit int64) (_ []byte, _ http.Header, err error) {
	defer func() { err = redact.Error(err) }()
	if limit <= 0 {
		limit = 10 << 20
	}
	if _, err := url.Parse(rawURL); err != nil {
		return nil, nil, fmt.Errorf("URL 非法: %w", err)
	}
	transport, err := newHTTPTransport(proxyURL)
	if err != nil {
		return nil, nil, err
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("构造请求失败: %w", err)
	}
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, fmt.Errorf("读取内容失败: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, nil, fmt.Errorf("内容超过 %d MiB 上限", limit>>20)
	}
	return body, resp.Header.Clone(), nil
}

// newHTTPTransport 构造下载用 Transport。空 proxyURL 明确表示直连，绝不读取
// HTTP_PROXY/HTTPS_PROXY 等环境变量。
func newHTTPTransport(proxyURL string) (*http.Transport, error) {
	transport := &http.Transport{}
	if proxyURL == "" {
		return transport, nil
	}
	pu, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("下载代理地址非法: %w", err)
	}
	transport.Proxy = http.ProxyURL(pu)
	return transport, nil
}
