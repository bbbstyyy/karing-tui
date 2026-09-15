package redact

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestURLRedactionCoversCredentialsOpaquePathsAndShareLinks(t *testing.T) {
	for _, raw := range []string{
		"https://fixture-user:fixture-password@example.invalid/sub/fixture-token?unknown=fixture-query#fixture-fragment",
		"trojan://fixture-password@example.invalid:443",
		"vmess://fixture-base64-credentials",
	} {
		out := Text("download failed: " + raw)
		if strings.Contains(out, "fixture-") {
			t.Fatalf("credential leaked: %s", out)
		}
		if got := Text(out); got != out {
			t.Fatalf("redaction is not idempotent: %q => %q", out, got)
		}
	}
	underlying := errors.New("connection reset")
	err := Error(fmt.Errorf("https://example.invalid/private: %w", underlying))
	if !errors.Is(err, underlying) {
		t.Fatal("redaction lost the error chain")
	}
}
