package handlers

import (
	"net/http"
	"time"
)

// buildHTTPClient returns an *http.Client that respects HTTP_PROXY / HTTPS_PROXY / ALL_PROXY
// environment variables (SOCKS5 supported via ALL_PROXY=socks5://host:port).
func buildHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}
}
