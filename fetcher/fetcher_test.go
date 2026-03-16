package fetcher

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		ip      string
		private bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true},
		{"::1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if got := isPrivateIP(ip); got != tt.private {
			t.Errorf("isPrivateIP(%s) = %v, want %v", tt.ip, got, tt.private)
		}
	}
}

func TestFetchRejectsPrivateIPs(t *testing.T) {
	tests := []string{
		"http://127.0.0.1/",
		"http://10.0.0.1/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
	}
	for _, u := range tests {
		_, err := Fetch(context.Background(), u, 0)
		if err == nil {
			t.Errorf("expected error for %s, got nil", u)
		}
		if !strings.Contains(err.Error(), "blocked") && !strings.Contains(err.Error(), "DNS lookup failed") {
			t.Errorf("unexpected error for %s: %v", u, err)
		}
	}
}

func TestFetchRejectsNonHTTPSchemes(t *testing.T) {
	tests := []string{
		"ftp://example.com/file",
		"file:///etc/passwd",
		"gopher://example.com",
	}
	for _, u := range tests {
		_, err := Fetch(context.Background(), u, 0)
		if err == nil {
			t.Errorf("expected error for %s, got nil", u)
		}
		if !strings.Contains(err.Error(), "unsupported scheme") {
			t.Errorf("unexpected error for %s: %v", u, err)
		}
	}
}

// Tests below use fetch() with ssrfCheck=false to allow httptest servers on 127.0.0.1.

func TestFetchRejectsUnsupportedContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("fake pdf"))
	}))
	defer srv.Close()

	_, err := fetch(context.Background(), srv.URL, 0, false)
	if err == nil {
		t.Fatal("expected error for unsupported content type")
	}
	if !strings.Contains(err.Error(), "unsupported content type") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFetchPlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Hello, world!"))
	}))
	defer srv.Close()

	text, err := fetch(context.Background(), srv.URL, 0, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Hello, world!" {
		t.Fatalf("unexpected text: %q", text)
	}
}

func TestFetchHTML(t *testing.T) {
	htmlContent := `<!DOCTYPE html>
<html><head><title>Test Article</title></head>
<body>
<nav>Navigation menu</nav>
<article>
<h1>Test Article</h1>
<p>This is the main article content that should be extracted by readability. It needs to be long enough to be considered the main content of the page, so here is some additional text to make it substantial enough for the readability algorithm to pick it up.</p>
<p>Another paragraph with more content to ensure readability can identify this as the main article text. The more content we have here, the better readability can distinguish it from navigation and other boilerplate elements.</p>
</article>
<footer>Footer content</footer>
</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(htmlContent))
	}))
	defer srv.Close()

	text, err := fetch(context.Background(), srv.URL, 0, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(text, "main article content") {
		t.Fatalf("expected article content, got: %q", text)
	}
}

func TestFetchTruncatesLongContent(t *testing.T) {
	long := strings.Repeat("x", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(long))
	}))
	defer srv.Close()

	text, err := fetch(context.Background(), srv.URL, 100, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len([]rune(text)) != 100 {
		t.Fatalf("expected 100 runes, got %d", len([]rune(text)))
	}
}

func TestFetchRedirectToPrivateIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1/secret", http.StatusFound)
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL, 0)
	if err == nil {
		t.Fatal("expected error for redirect to private IP")
	}
}

func TestFetchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := fetch(context.Background(), srv.URL, 0, false)
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveAndValidateBlocksPrivateIP(t *testing.T) {
	_, err := resolveAndValidate("127.0.0.1")
	if err == nil {
		t.Fatal("expected error for 127.0.0.1")
	}
}

func TestResolveAndValidateAllowsPublicIP(t *testing.T) {
	ip, err := resolveAndValidate("8.8.8.8")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip.String() != "8.8.8.8" {
		t.Fatalf("unexpected IP: %s", ip)
	}
}

func TestIsAllowedContentType(t *testing.T) {
	tests := []struct {
		ct      string
		allowed bool
	}{
		{"text/html", true},
		{"text/html; charset=utf-8", true},
		{"TEXT/HTML", true},
		{"text/plain", true},
		{"application/json", false},
		{"application/pdf", false},
		{"image/png", false},
	}
	for _, tt := range tests {
		if got := isAllowedContentType(tt.ct); got != tt.allowed {
			t.Errorf("isAllowedContentType(%q) = %v, want %v", tt.ct, got, tt.allowed)
		}
	}
}

func TestFetchEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, "   ")
	}))
	defer srv.Close()

	_, err := fetch(context.Background(), srv.URL, 0, false)
	if err == nil {
		t.Fatal("expected error for empty body")
	}
	if !strings.Contains(err.Error(), "no text content") {
		t.Fatalf("unexpected error: %v", err)
	}
}
