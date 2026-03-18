package fetcher

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	readability "github.com/go-shiori/go-readability"
)

const (
	maxBodyBytes    = 1 << 20 // 1 MB
	defaultMaxChars = 64_000
	connectTimeout  = 5 * time.Second
	totalTimeout    = 10 * time.Second
)

// privateRanges lists CIDRs that must be blocked to prevent SSRF.
var privateRanges []*net.IPNet

func init() {
	for _, cidr := range []string{
		"127.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	} {
		_, n, _ := net.ParseCIDR(cidr)
		privateRanges = append(privateRanges, n)
	}
}

func isPrivateIP(ip net.IP) bool {
	for _, n := range privateRanges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// resolveAndValidate resolves a hostname and returns the first non-private IP.
func resolveAndValidate(host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return nil, fmt.Errorf("blocked private/reserved IP: %s", ip)
		}
		return ip, nil
	}

	addrs, err := net.LookupHost(host)
	if err != nil {
		return nil, fmt.Errorf("DNS lookup failed for %s: %w", host, err)
	}
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil {
			continue
		}
		if isPrivateIP(ip) {
			return nil, fmt.Errorf("blocked private/reserved IP %s for host %s", ip, host)
		}
		return ip, nil
	}
	return nil, fmt.Errorf("no valid IP found for host %s", host)
}

// Fetch downloads a URL with SSRF protection and extracts the article text.
// maxChars controls the maximum length of the returned text (0 = defaultMaxChars).
func Fetch(ctx context.Context, rawURL string, maxChars int) (string, error) {
	return fetch(ctx, rawURL, maxChars, true)
}

func fetch(ctx context.Context, rawURL string, maxChars int, ssrfCheck bool) (string, error) {
	if maxChars <= 0 {
		maxChars = defaultMaxChars
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q (only http/https allowed)", scheme)
	}

	host := parsed.Hostname()
	if host == "" {
		return "", fmt.Errorf("URL has no hostname")
	}

	var client *http.Client

	if ssrfCheck {
		resolvedIP, err := resolveAndValidate(host)
		if err != nil {
			return "", err
		}

		port := parsed.Port()
		if port == "" {
			if scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		pinnedAddr := net.JoinHostPort(resolvedIP.String(), port)

		transport := &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: connectTimeout}
				return d.DialContext(ctx, network, pinnedAddr)
			},
			TLSHandshakeTimeout: connectTimeout,
		}

		client = &http.Client{
			Transport: transport,
			Timeout:   totalTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				rHost := req.URL.Hostname()
				if _, err := resolveAndValidate(rHost); err != nil {
					return fmt.Errorf("redirect blocked: %w", err)
				}
				return nil
			},
		}
	} else {
		client = &http.Client{Timeout: totalTimeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "TelegramSummarizeBot/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !isAllowedContentType(ct) {
		return "", fmt.Errorf("unsupported content type: %s", ct)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", fmt.Errorf("failed to read body: %w", err)
	}

	text := string(body)

	if strings.Contains(ct, "text/html") {
		article, err := readability.FromReader(strings.NewReader(text), parsed)
		if err == nil && strings.TrimSpace(article.TextContent) != "" {
			text = article.TextContent
		}
	}

	text = strings.TrimSpace(text)

	if runes := []rune(text); len(runes) > maxChars {
		text = string(runes[:maxChars])
	}

	if text == "" {
		return "", fmt.Errorf("no text content extracted from URL")
	}

	return text, nil
}

func isAllowedContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "text/plain")
}
