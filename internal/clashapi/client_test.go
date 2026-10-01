package clashapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return New(u.Hostname(), port, "")
}

func TestGroupDelayRejectsEmptySuccessfulResponse(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/group/Auto/delay" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("timeout"); got != "3000" {
			t.Fatalf("timeout = %q, want 3000", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	})

	got, err := client.GroupDelay(context.Background(), "Auto", "", 3000)
	if err == nil {
		t.Fatalf("GroupDelay returned success for empty result: %#v", got)
	}
	if !strings.Contains(err.Error(), "无成功结果") {
		t.Fatalf("error = %q, want empty-result diagnosis", err)
	}
}

func TestGroupDelayAcceptsAnySuccessfulMember(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"node-a":87}`))
	})

	got, err := client.GroupDelay(context.Background(), "Auto", "", 3000)
	if err != nil {
		t.Fatal(err)
	}
	if got["node-a"] != 87 {
		t.Fatalf("node-a delay = %d, want 87", got["node-a"])
	}
}
