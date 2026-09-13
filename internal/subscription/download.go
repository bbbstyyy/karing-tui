package subscription

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/core"
)

// maxSubscribeSize 单个订阅内容上限（10 MiB）。
const maxSubscribeSize = 10 << 20

// KaringUserAgent 返回与 Karing 客户端一致的默认订阅 User-Agent。
// Karing 会把多个常见客户端标识拼接在同一个请求头中，供订阅服务识别
// 期望返回的配置格式。
func KaringUserAgent() string {
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	compatible := []string{
		"mihomo/1.19.28",
		"clash-verge",
		"FLClash",
		"mihomo.party/v2.0.0 (clash.meta)",
		"ClashMeta",
		"v2ray",
		"sing-box 1.13.0",
		"NekoBox/Android/1.4.1 (Prefer ClashMeta Format)",
		"HiddifyNext",
	}
	return fmt.Sprintf("Karing/1.1.3.703 platform/%s %s", platform, strings.Join(compatible, ";"))
}

// Downloader 订阅内容下载器，支持经 HTTP(S) 代理下载。
type Downloader struct {
	ProxyURL string        // 用户自定义 HTTP(S) 代理地址；为空表示直连
	Strategy string        // prefer_proxy / prefer_direct / only_proxy / only_direct
	Timeout  time.Duration // 预留；当前由 core.FetchHTTP 统一 30s 超时
}

// Fetch 下载订阅内容。userAgent 为空时使用默认值。
func (d *Downloader) Fetch(ctx context.Context, rawURL, userAgent string) ([]byte, error) {
	body, _, err := d.FetchMeta(ctx, rawURL, userAgent)
	return body, err
}

func (d *Downloader) FetchMeta(ctx context.Context, rawURL, userAgent string) ([]byte, http.Header, error) {
	if strings.TrimSpace(userAgent) == "" {
		userAgent = KaringUserAgent()
	}
	strategy, err := config.NormalizeDownloadStrategy(d.Strategy)
	if err != nil {
		return nil, nil, err
	}
	type attempt struct {
		name  string
		proxy string
	}
	direct := attempt{name: "直连"}
	proxy := attempt{name: "代理", proxy: strings.TrimSpace(d.ProxyURL)}
	var attempts []attempt
	switch strategy {
	case config.DownloadPreferProxy:
		if proxy.proxy != "" {
			attempts = []attempt{proxy, direct}
		} else {
			attempts = []attempt{direct}
		}
	case config.DownloadPreferDirect:
		attempts = []attempt{direct}
		if proxy.proxy != "" {
			attempts = append(attempts, proxy)
		}
	case config.DownloadOnlyProxy:
		if proxy.proxy == "" {
			return nil, nil, fmt.Errorf("订阅下载策略为 only_proxy，但未配置下载代理")
		}
		attempts = []attempt{proxy}
	case config.DownloadOnlyDirect:
		attempts = []attempt{direct}
	}
	var failures []string
	for _, a := range attempts {
		body, headers, fetchErr := core.FetchHTTPMeta(ctx, rawURL, a.proxy, userAgent, maxSubscribeSize)
		if fetchErr == nil {
			return body, headers, nil
		}
		failures = append(failures, a.name+": "+fetchErr.Error())
	}
	return nil, nil, fmt.Errorf("订阅下载失败（%s）", strings.Join(failures, "; "))
}
