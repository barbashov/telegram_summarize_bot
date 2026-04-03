package cmd

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGenerateCodeVerifier(t *testing.T) {
	v1, err := generateCodeVerifier()
	if err != nil {
		t.Fatalf("generateCodeVerifier: %v", err)
	}
	v2, err := generateCodeVerifier()
	if err != nil {
		t.Fatalf("generateCodeVerifier: %v", err)
	}

	if v1 == v2 {
		t.Error("two verifiers should not be equal")
	}
	if len(v1) < 32 {
		t.Errorf("verifier too short: %d chars", len(v1))
	}
}

func TestComputeCodeChallenge(t *testing.T) {
	verifier := "test-verifier-123"
	challenge := computeCodeChallenge(verifier)

	// Verify manually
	h := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(h[:])

	if challenge != expected {
		t.Errorf("challenge = %q, want %q", challenge, expected)
	}
}

func TestGenerateState(t *testing.T) {
	s1, err := generateState()
	if err != nil {
		t.Fatalf("generateState: %v", err)
	}
	s2, err := generateState()
	if err != nil {
		t.Fatalf("generateState: %v", err)
	}

	if s1 == s2 {
		t.Error("two states should not be equal")
	}
	if len(s1) != 32 { // 16 bytes = 32 hex chars
		t.Errorf("state length = %d, want 32", len(s1))
	}
}

func TestBuildAuthURL(t *testing.T) {
	authURL := buildAuthURL("client-123", "http://localhost:8080/callback", "challenge-abc", "state-xyz")

	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	if !strings.HasPrefix(authURL, openAIAuthURL) {
		t.Errorf("URL should start with %s, got %s", openAIAuthURL, authURL)
	}

	params := parsed.Query()
	checks := map[string]string{
		"client_id":             "client-123",
		"redirect_uri":          "http://localhost:8080/callback",
		"response_type":         "code",
		"scope":                 oauthScopes,
		"state":                 "state-xyz",
		"code_challenge":        "challenge-abc",
		"code_challenge_method": "S256",
		"audience":              oauthAudience,
	}
	for key, want := range checks {
		if got := params.Get(key); got != want {
			t.Errorf("param %s = %q, want %q", key, got, want)
		}
	}
}

func TestCallbackHandler(t *testing.T) {
	state := "test-state-123"
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			errCh <- http.ErrAbortHandler
			http.Error(w, "State mismatch", http.StatusBadRequest)
			return
		}
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			errCh <- http.ErrAbortHandler
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- http.ErrAbortHandler
			http.Error(w, "No code", http.StatusBadRequest)
			return
		}
		codeCh <- code
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	t.Run("success", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/callback?code=auth123&state=" + state)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		code := <-codeCh
		if code != "auth123" {
			t.Errorf("code = %q, want %q", code, "auth123")
		}
	})

	t.Run("state mismatch", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/callback?code=auth123&state=wrong-state")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		<-errCh
	})

	t.Run("missing code", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/callback?state=" + state)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		<-errCh
	})
}
