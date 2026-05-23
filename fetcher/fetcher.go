package fetcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	readability "github.com/go-shiori/go-readability"
)

const (
	maxBodyBytes    = 1 << 20 // 1 MB
	defaultMaxChars = 64_000
	connectTimeout  = 5 * time.Second
	totalTimeout    = 10 * time.Second
	// minArticleChars is the floor below which an HTML page is treated as having
	// no real article (login walls, SSO redirects, empty JS shells often extract
	// to nothing or a stray word).
	minArticleChars = 50
)

// ErrNoReadableContent means the page loaded fine but no article text could be
// extracted — typically a login/SSO wall, a redirect bootstrap, or a JS-rendered
// shell. Callers should surface a "couldn't read this page" message rather than
// summarizing the shell.
var ErrNoReadableContent = errors.New("no readable content extracted from page")

// reservedRanges lists extra non-public CIDRs not covered by net.IP's built-in
// classification helpers (CGNAT, documentation/benchmark ranges, reserved space,
// NAT64, etc.). Blocking these alongside the built-ins keeps SSRF from reaching
// internal or special-use addresses.
var reservedRanges []*net.IPNet

func init() {
	for _, cidr := range []string{
		"100.64.0.0/10",   // CGNAT (RFC 6598)
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved (incl. 255.255.255.255)
		"2001:db8::/32",   // IPv6 documentation
		"64:ff9b::/96",    // NAT64
		"100::/64",        // IPv6 discard-only
	} {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			reservedRanges = append(reservedRanges, n)
		}
	}
}

// isBlockedIP reports whether ip is loopback, private, link-local, multicast,
// unspecified, or in a reserved/special-use range — i.e. not a routable public
// address we are willing to fetch from.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	for _, n := range reservedRanges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// pickPublicIPs parses addr strings and returns those that are routable public
// addresses, dropping any blocked (loopback/private/reserved/etc.) ones.
func pickPublicIPs(addrs []string) []net.IP {
	var ips []net.IP
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil || isBlockedIP(ip) {
			continue
		}
		ips = append(ips, ip)
	}
	return ips
}

// resolveAllValidated resolves host and returns only its public IPs. A literal
// IP must itself be public. For a hostname, blocked addresses are dropped rather
// than rejecting the whole host (some hosts return a mix of public and
// special-use/filtered records, e.g. an ISP-injected bogus AAAA); it errors only
// when no public IP remains. SSRF safety is preserved because the caller dials
// exactly the validated IPs returned here — never a blocked one.
func resolveAllValidated(host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return nil, fmt.Errorf("blocked non-public IP: %s", ip)
		}
		return []net.IP{ip}, nil
	}

	addrs, err := net.LookupHost(host)
	if err != nil {
		return nil, fmt.Errorf("DNS lookup failed for %s: %w", host, err)
	}
	ips := pickPublicIPs(addrs)
	if len(ips) == 0 {
		return nil, fmt.Errorf("no public IP found for host %s", host)
	}
	return ips, nil
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
		dialer := &net.Dialer{Timeout: connectTimeout}
		transport := &http.Transport{
			// Validate at dial time so the IP we connect to is exactly the IP we
			// validated — for the initial request and every redirect hop. This
			// closes the resolve/dial TOCTOU and re-pins correctly on redirects.
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				h, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := resolveAllValidated(h)
				if err != nil {
					return nil, err
				}
				var lastErr error
				for _, ip := range ips {
					conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
					if derr == nil {
						return conn, nil
					}
					lastErr = derr
				}
				return nil, lastErr
			},
			TLSHandshakeTimeout: connectTimeout,
		}

		client = &http.Client{
			Transport: transport,
			Timeout:   totalTimeout,
			// Per-hop IP validation is enforced at dial time; cap the chain here.
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
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

	// For HTML, require readability to extract a real article. Falling back to
	// raw HTML here would feed login/SSO/JS-shell markup to the summarizer, so
	// instead report it as unreadable. Raw text/plain bodies are kept as-is.
	if strings.Contains(ct, "text/html") {
		article, rerr := readability.FromReader(strings.NewReader(text), parsed)
		if rerr != nil || utf8.RuneCountInString(strings.TrimSpace(article.TextContent)) < minArticleChars {
			return "", ErrNoReadableContent
		}
		text = article.TextContent
	}

	text = strings.TrimSpace(text)

	if runes := []rune(text); len(runes) > maxChars {
		text = string(runes[:maxChars])
	}

	if text == "" {
		return "", ErrNoReadableContent
	}

	return text, nil
}

func isAllowedContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "text/plain")
}
