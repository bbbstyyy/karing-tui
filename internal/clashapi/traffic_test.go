package clashapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrafficConnectionsParsesOnlyTrafficFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"uploadTotal":123,"downloadTotal":456,"connections":[{"id":"one","upload":11,"download":22,"chains":["node","AI","Auto"],"metadata":{"large":["ignored",1,2,3]}},{"id":"two","upload":3,"download":4,"chains":["Auto"]}]}`)
	}))
	defer server.Close()

	c := &Client{base: server.URL, hc: server.Client()}
	stats, conns, err := c.TrafficConnections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats != (Connections{UploadTotal: 123, DownloadTotal: 456, Count: 2}) {
		t.Fatalf("stats = %+v", stats)
	}
	if len(conns) != 2 || conns[0].ID != "one" || conns[0].Upload != 11 || conns[0].Download != 22 {
		t.Fatalf("connections = %+v", conns)
	}
	if len(conns[0].Chains) != 3 || conns[0].Chains[1] != "AI" || conns[1].Chains[0] != "Auto" {
		t.Fatalf("chains = %+v", conns)
	}
}

func TestProxiesPreservesRuntimeGroupMarker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"proxies":{"node":{"type":"Trojan","name":"node","history":[]},"Auto":{"type":"Selector","name":"Auto","now":"node","all":["node"],"history":[]}}}`)
	}))
	defer server.Close()

	c := &Client{base: server.URL, hc: server.Client()}
	proxies, err := c.Proxies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if proxies["node"].All != nil {
		t.Fatalf("plain outbound unexpectedly marked as group: %+v", proxies["node"])
	}
	if proxies["Auto"].All == nil || len(proxies["Auto"].All) != 1 || proxies["Auto"].All[0] != "node" {
		t.Fatalf("runtime group marker lost: %+v", proxies["Auto"])
	}
}
