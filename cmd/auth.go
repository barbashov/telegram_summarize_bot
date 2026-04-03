package cmd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"telegram_summarize_bot/provider"
)

const (
	openAIAuthURL = "https://auth0.openai.com/authorize"
	oauthAudience = "https://api.openai.com/v1"
	oauthScopes   = "openid profile email offline_access"
)

// RunAuth performs the OAuth PKCE flow for OpenAI Codex subscription.
func RunAuth(clientID, tokenDir string) error {
	// Generate PKCE values
	verifier, err := generateCodeVerifier()
	if err != nil {
		return fmt.Errorf("generate code verifier: %w", err)
	}
	challenge := computeCodeChallenge(verifier)
	state, err := generateState()
	if err != nil {
		return fmt.Errorf("generate state: %w", err)
	}

	// Start local callback server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start callback server: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			errCh <- fmt.Errorf("state mismatch")
			http.Error(w, "State mismatch", http.StatusBadRequest)
			return
		}
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			desc := r.URL.Query().Get("error_description")
			errCh <- fmt.Errorf("OAuth error: %s: %s", errMsg, desc)
			_, _ = fmt.Fprintf(w, "<html><body><h2>Authentication failed</h2><p>%s: %s</p></body></html>", errMsg, desc)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("no authorization code in callback")
			http.Error(w, "No code", http.StatusBadRequest)
			return
		}
		codeCh <- code
		_, _ = fmt.Fprint(w, "<html><body><h2>Authentication successful!</h2><p>You can close this window.</p></body></html>")
	})

	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = server.Shutdown(ctx)
		cancel()
	}()

	// Build authorization URL
	authURL := buildAuthURL(clientID, redirectURI, challenge, state)
	fmt.Printf("\nOpening browser for OpenAI authentication...\n")
	fmt.Printf("If the browser doesn't open, visit this URL manually:\n\n%s\n\n", authURL)
	openBrowser(authURL)

	// Wait for callback
	fmt.Println("Waiting for authentication (3 minutes timeout)...")
	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-time.After(3 * time.Minute):
		return fmt.Errorf("authentication timed out after 3 minutes")
	}

	// Exchange code for tokens
	fmt.Println("Exchanging authorization code for tokens...")
	tokens, err := exchangeCode(clientID, code, redirectURI, verifier)
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}

	// Save tokens
	store := provider.NewTokenStore(tokenDir, clientID)
	if err := store.Save(tokens); err != nil {
		return fmt.Errorf("save tokens: %w", err)
	}

	fmt.Printf("\n✓ Authentication successful! Token saved to %s/openai_tokens.json\n", tokenDir)

	// List available models
	if err := listModels(tokens.AccessToken); err != nil {
		fmt.Printf("\nWarning: could not list models: %v\n", err)
	}

	fmt.Printf("\nAdd to your .env:\n")
	fmt.Printf("  LLM_MODE=oauth\n")
	fmt.Printf("  MODEL=gpt-4o\n")

	return nil
}

func generateCodeVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func computeCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

func buildAuthURL(clientID, redirectURI, challenge, state string) string {
	params := url.Values{}
	params.Set("client_id", clientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("response_type", "code")
	params.Set("scope", oauthScopes)
	params.Set("state", state)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("audience", oauthAudience)
	return openAIAuthURL + "?" + params.Encode()
}

func openBrowser(rawURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "linux":
		cmd = exec.Command("xdg-open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		return
	}
	_ = cmd.Start()
}

func exchangeCode(clientID, code, redirectURI, verifier string) (*provider.OAuthTokens, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}

	client := provider.HTTPClient(30 * time.Second)
	resp, err := client.Post(
		provider.OpenAITokenURL, "application/x-www-form-urlencoded",
		strings.NewReader(data.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var tr provider.TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("parse response: %w (body: %s)", err, string(body))
	}
	if tr.Error != "" {
		return nil, fmt.Errorf("%s: %s", tr.Error, tr.ErrorDesc)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("no access_token in response")
	}

	expiresAt := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return &provider.OAuthTokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}

type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func listModels(accessToken string) error {
	req, err := http.NewRequest("GET", "https://api.openai.com/v1/models", http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := provider.HTTPClient(10 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var models modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return err
	}

	if len(models.Data) == 0 {
		fmt.Println("\nNo models available.")
		return nil
	}

	fmt.Println("\nAvailable models:")
	for _, m := range models.Data {
		fmt.Printf("  - %s\n", m.ID)
	}
	return nil
}
