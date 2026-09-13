package core

import (
	"net/http"
	"testing"
)

func TestNewHTTPTransportDoesNotReadEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	transport, err := newHTTPTransport("")
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	if transport.Proxy != nil {
		t.Fatal("空代理地址不应读取环境代理")
	}
}

func TestNewHTTPTransportUsesExplicitProxy(t *testing.T) {
	transport, err := newHTTPTransport("http://127.0.0.1:7890")
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := transport.Proxy(req)
	if err != nil || proxy.String() != "http://127.0.0.1:7890" {
		t.Fatalf("显式代理 = %v, %v", proxy, err)
	}
}
