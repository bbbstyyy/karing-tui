package subscription

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestKaringUserAgent(t *testing.T) {
	ua := KaringUserAgent()
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	if !strings.HasPrefix(ua, "Karing/1.1.3.703 platform/"+platform+" ") {
		t.Fatalf("unexpected Karing User-Agent: %q", ua)
	}
	for _, want := range []string{
		"mihomo/1.19.28",
		"clash-verge",
		"sing-box 1.13.0",
		"HiddifyNext",
	} {
		if !strings.Contains(ua, want) {
			t.Errorf("Karing User-Agent missing %q: %q", want, ua)
		}
	}
}

func TestDownloaderFetchUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ss://aes-256-gcm:password@example.com:443#node"))
	}))
	defer srv.Close()

	d := &Downloader{}
	if _, err := d.Fetch(context.Background(), srv.URL, ""); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != KaringUserAgent() {
		t.Fatalf("default User-Agent = %q, want %q", got, KaringUserAgent())
	}

	if _, err := d.Fetch(context.Background(), srv.URL, "custom-agent"); err != nil {
		t.Fatalf("Fetch custom UA: %v", err)
	}
	if got != "custom-agent" {
		t.Fatalf("custom User-Agent = %q, want custom-agent", got)
	}
}

func TestDownloaderStrategies(t *testing.T) {
	content := "ss://aes-256-gcm:password@example.com:443#node"
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	defer proxy.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	defer origin.Close()

	got, _, err := (&Downloader{ProxyURL: proxy.URL, Strategy: "only_proxy"}).FetchMeta(context.Background(), origin.URL, "")
	if err != nil || string(got) != content || proxyHits.Load() != 1 {
		t.Fatalf("only_proxy: body=%q err=%v hits=%d", got, err, proxyHits.Load())
	}
	got, _, err = (&Downloader{ProxyURL: proxy.URL, Strategy: "only_direct"}).FetchMeta(context.Background(), origin.URL, "")
	if err != nil || string(got) != content || proxyHits.Load() != 1 {
		t.Fatalf("only_direct: body=%q err=%v hits=%d", got, err, proxyHits.Load())
	}

	failingProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failingProxy.Close()
	got, _, err = (&Downloader{ProxyURL: failingProxy.URL, Strategy: "prefer_proxy"}).FetchMeta(context.Background(), origin.URL, "")
	if err != nil || string(got) != content {
		t.Fatalf("prefer_proxy fallback: body=%q err=%v", got, err)
	}
	got, _, err = (&Downloader{ProxyURL: proxy.URL, Strategy: "prefer_direct"}).FetchMeta(context.Background(), "http://127.0.0.1:1/unreachable", "")
	if err != nil || string(got) != content {
		t.Fatalf("prefer_direct fallback: body=%q err=%v", got, err)
	}
}

func TestDownloaderOnlyProxyRequiresConfiguredProxy(t *testing.T) {
	_, _, err := (&Downloader{Strategy: "only_proxy"}).FetchMeta(context.Background(), "http://127.0.0.1:1", "")
	if err == nil || !strings.Contains(err.Error(), "未配置下载代理") {
		t.Fatalf("only_proxy 空代理错误 = %v", err)
	}
}
