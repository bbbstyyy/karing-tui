package clashapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConnectionsStabilitySummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"uploadTotal":123,"downloadTotal":456,"connections":[{"id":"1","metadata":{"nested":[1,2,3]}},{"id":"2"}]} `)
	}))
	defer server.Close()
	c := &Client{base: server.URL, hc: server.Client()}
	for i := 0; i < 100; i++ {
		stats, err := c.Connections(context.Background())
		if err != nil || stats != (Connections{UploadTotal: 123, DownloadTotal: 456, Count: 2}) {
			t.Fatalf("summary: %+v, %v", stats, err)
		}
	}
	stats, details, err := c.DetailedConnections(context.Background())
	if err != nil || stats.Count != 2 || len(details) != 2 || details[0].ID != "1" {
		t.Fatalf("detailed endpoint changed: %+v, %+v, %v", stats, details, err)
	}
}

func TestAPIStabilityResponseBounds(t *testing.T) {
	for _, payload := range []string{
		`{"connections":null}`,
		`{"connections":[]}`,
	} {
		var raw struct {
			Connections connectionCount `json:"connections"`
		}
		if err := decodeAPIResponse(strings.NewReader(payload), &raw); err != nil || raw.Connections != 0 {
			t.Fatalf("empty connections: %v", err)
		}
	}
	for _, payload := range []string{`{} {}`, `{"connections":`, `[] trailing`} {
		var raw any
		if err := decodeAPIResponse(strings.NewReader(payload), &raw); err == nil {
			t.Fatalf("accepted malformed response %q", payload)
		}
	}
	var raw any
	oversized := `"` + strings.Repeat("x", maxAPIResponseBytes+1) + `"`
	if err := decodeAPIResponse(strings.NewReader(oversized), &raw); !errors.Is(err, errAPIResponseTooLarge) {
		t.Fatalf("unbounded chunked response: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(maxAPIResponseBytes+1))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	c := &Client{base: server.URL, hc: server.Client()}
	if _, err := c.Connections(context.Background()); !errors.Is(err, errAPIResponseTooLarge) {
		t.Fatalf("oversized Content-Length was not rejected: %v", err)
	}
}
