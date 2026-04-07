package httputil

import (
	"crypto/tls"
	"net/http"
	"time"
)

// NewClient creates an HTTP client with proxy support and h2 disabled
// (required for HTTPS proxies that don't support CONNECT over h2).
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: NewTransport(),
	}
}

// NewTransport creates an http.Transport with proxy support and h2 disabled.
func NewTransport() *http.Transport {
	return &http.Transport{
		Proxy:        http.ProxyFromEnvironment,
		TLSNextProto: make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
	}
}
