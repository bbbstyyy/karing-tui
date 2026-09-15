// Package redact removes URL credentials from user-facing errors and logs.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

var urls = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s\x1b"'<>，；）]+`)

// URL hides user info, opaque paths, query values and fragments. Subscription
// tokens can live in arbitrary path segments, so a key-name allowlist is unsafe.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[地址已隐藏]"
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks" && u.Scheme != "socks5" {
		return u.Scheme + "://***"
	}
	result := u.Scheme + "://"
	if u.User != nil {
		result += "***@"
	}
	result += u.Host
	if u.Path != "" && u.Path != "/" {
		result += "/*"
	} else {
		result += u.Path
	}
	if u.RawQuery != "" {
		result += "?***"
	}
	if u.Fragment != "" {
		result += "#***"
	}
	return result
}

func Text(text string) string {
	return urls.ReplaceAllStringFunc(text, func(raw string) string {
		trimmed := strings.TrimRight(raw, ").,;]")
		return URL(trimmed) + raw[len(trimmed):]
	})
}

type safeError struct{ err error }

func (e safeError) Error() string { return Text(e.err.Error()) }
func (e safeError) Unwrap() error { return e.err }
func Error(err error) error {
	if err == nil {
		return nil
	}
	return safeError{err: err}
}
